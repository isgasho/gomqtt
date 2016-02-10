// Copyright (c) 2014 The gomqtt Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package client

import (
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/gomqtt/packet"
	"github.com/gomqtt/session"
	"github.com/gomqtt/transport"
	"gopkg.in/tomb.v2"
)

// ErrAlreadyConnecting is returned by Connect if there has been already a
// connection attempt.
var ErrClientAlreadyConnecting = errors.New("client already connecting")

// ErrNotConnected is returned by Publish, Subscribe and Unsubscribe if the
// client is not currently connected.
var ErrClientNotConnected = errors.New("client not connected")

// ErrMissingClientID is returned by Connect if no ClientID has been provided in
// the options while requesting to resume a session.
var ErrClientMissingID = errors.New("client missing id")

// ErrConnectionDenied is returned in the Callback if the connection has been
// reject by the broker.
var ErrClientConnectionDenied = errors.New("client connection denied")

// ErrMissingPong is returned in the Callback if the broker did not respond
// in time to a PingreqPacket.
var ErrClientMissingPong = errors.New("client missing pong")

// ErrUnexpectedClose is returned in the Callback if the broker closed the
// connection without receiving a DisconnectPacket from the client.
var ErrClientUnexpectedClose = errors.New("client unexpected close")

// Callback is a function called by the client upon received messages or
// internal errors.
type Callback func(topic string, payload []byte, err error)

// Logger is a function called by the client to log activity.
type Logger func(msg string)

// Client connects to a broker and handles the transmission of packets. It will
// automatically send PingreqPackets to keep the connection alive. Outgoing
// publish related packets will be stored in session and resend when the
// connection gets closed abruptly. All methods return Futures that get completed
// when the packets get acknowledged by the broker. Once the connection is closed
// all waiting futures get canceled.
//
// Note: If clean session is false and there are packets in the store, messages
// might get completed after connecting without triggering any futures to complete.
type Client struct {
	conn transport.Conn

	Session  session.Session
	Callback Callback
	Logger   Logger

	state   *state
	tracker *tracker
	clean   bool

	futureStore   *futureStore
	connectFuture *ConnectFuture

	tomb   tomb.Tomb
	mutex  sync.Mutex
	finish sync.Once
}

// New returns a new client that by default uses a fresh MemorySession.
func New() *Client {
	return &Client{
		Session:     session.NewMemorySession(),
		state:       newState(),
		futureStore: newFutureStore(),
	}
}

// Connect opens the connection to the broker and sends a ConnectPacket. It will
// return a ConnectFuture that gets completed once a ConnackPacket has been
// received. If the ConnectPacket couldn't be transmitted it will return an error.
func (c *Client) Connect(urlString string, opts *Options) (*ConnectFuture, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	// check if already connecting
	if c.state.get() >= stateConnecting {
		return nil, ErrClientAlreadyConnecting
	}

	// parse url
	urlParts, err := url.ParseRequestURI(urlString)
	if err != nil {
		return nil, err
	}

	// save opts
	if opts == nil {
		opts = NewOptions()
	}

	// check client id
	if !opts.CleanSession && opts.ClientID == "" {
		return nil, ErrClientMissingID
	}

	// parse keep alive
	keepAlive, err := time.ParseDuration(opts.KeepAlive)
	if err != nil {
		return nil, err
	}

	// allocate and initialize tracker
	c.tracker = newTracker(keepAlive)

	// dial broker
	c.conn, err = transport.Dial(urlString)
	if err != nil {
		return nil, err
	}

	// set to connecting as from this point the client cannot be reused
	c.state.set(stateConnecting)

	// from now on the connection has been used and we have to close the
	// connection and cleanup on any subsequent error

	// save clean
	c.clean = opts.CleanSession

	// reset store
	if c.clean {
		err = c.Session.Reset()
		if err != nil {
			return nil, c.cleanup(err, true)
		}
	}

	// allocate packet
	connect := packet.NewConnectPacket()
	connect.ClientID = []byte(opts.ClientID)
	connect.KeepAlive = uint16(keepAlive.Seconds())
	connect.CleanSession = opts.CleanSession

	// check for credentials
	if urlParts.User != nil {
		connect.Username = []byte(urlParts.User.Username())
		p, _ := urlParts.User.Password()
		connect.Password = []byte(p)
	}

	// set will
	connect.WillTopic = []byte(opts.WillTopic)
	connect.WillPayload = opts.WillPayload
	connect.WillQOS = opts.WillQos
	connect.WillRetain = opts.WillRetained

	// create new ConnackFuture
	c.connectFuture = &ConnectFuture{}
	c.connectFuture.initialize()

	// send connect packet
	err = c.send(connect, false)
	if err != nil {
		return nil, c.cleanup(err, false)
	}

	// start process routine
	c.tomb.Go(c.processor)

	// start keep alive if greater than zero
	if keepAlive > 0 {
		c.tomb.Go(c.pinger)
	}

	return c.connectFuture, nil
}

// Publish will send a PublishPacket containing the passed parameters. It will
// return a PublishFuture that gets completed once the quality of service flow
// has been completed.
func (c *Client) Publish(topic string, payload []byte, qos byte, retain bool) (*PublishFuture, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	// check if connected
	if c.state.get() != stateConnected {
		return nil, ErrClientNotConnected
	}

	// allocate packet
	publish := packet.NewPublishPacket()
	publish.Topic = []byte(topic)
	publish.Payload = payload
	publish.QOS = qos
	publish.Retain = retain
	publish.Dup = false

	// set packet id
	if qos > 0 {
		publish.PacketID = c.Session.PacketID()
	}

	// create future
	future := &PublishFuture{}
	future.initialize()

	// store future
	c.futureStore.put(publish.PacketID, future)

	// send packet and store it if at least qos 1
	err := c.send(publish, qos >= 1)
	if err != nil {
		return nil, c.cleanup(err, false)
	}

	// complete and remove qos 1 future
	if qos == 0 {
		future.complete()
		c.futureStore.del(publish.PacketID)
	}

	return future, nil
}

// Subscribe will send a SubscribePacket containing one topic to subscribe. It
// will return a SubscribeFuture that gets completed once a SubackPacket has
// been received.
func (c *Client) Subscribe(topic string, qos byte) (*SubscribeFuture, error) {
	return c.SubscribeMultiple(map[string]byte{
		topic: qos,
	})
}

// SubscribeMultiple will send a SubscribePacket containing multiple topics to
// subscribe. It will return a SubscribeFuture that gets completed once a
// SubackPacket has been received.
func (c *Client) SubscribeMultiple(filters map[string]byte) (*SubscribeFuture, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	// check if connected
	if c.state.get() != stateConnected {
		return nil, ErrClientNotConnected
	}

	// allocate packet
	subscribe := packet.NewSubscribePacket()
	subscribe.Subscriptions = make([]packet.Subscription, 0, len(filters))
	subscribe.PacketID = c.Session.PacketID()

	// append filters
	for topic, qos := range filters {
		subscribe.Subscriptions = append(subscribe.Subscriptions, packet.Subscription{
			Topic: []byte(topic),
			QOS:   qos,
		})
	}

	// create future
	future := &SubscribeFuture{}
	future.initialize()

	// store future
	c.futureStore.put(subscribe.PacketID, future)

	// send packet
	err := c.send(subscribe, true)
	if err != nil {
		return nil, c.cleanup(err, false)
	}

	return future, nil
}

// Unsubscribe will send a UnsubscribePacket containing one topic to unsubscribe.
// It will return a UnsubscribeFuture that gets completed once a UnsubackPacket
// has been received.
func (c *Client) Unsubscribe(topic string) (*UnsubscribeFuture, error) {
	return c.UnsubscribeMultiple([]string{topic})
}

// UnsubscribeMultiple will send a UnsubscribePacket containing multiple
// topics to unsubscribe. It will return a UnsubscribeFuture that gets completed
// once a UnsubackPacket has been received.
func (c *Client) UnsubscribeMultiple(topics []string) (*UnsubscribeFuture, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	// check if connected
	if c.state.get() != stateConnected {
		return nil, ErrClientNotConnected
	}

	// allocate packet
	unsubscribe := packet.NewUnsubscribePacket()
	unsubscribe.Topics = make([][]byte, 0, len(topics))
	unsubscribe.PacketID = c.Session.PacketID()

	// append topics
	for _, t := range topics {
		unsubscribe.Topics = append(unsubscribe.Topics, []byte(t))
	}

	// create future
	future := &UnsubscribeFuture{}
	future.initialize()

	// store future
	c.futureStore.put(unsubscribe.PacketID, future)

	// send packet
	err := c.send(unsubscribe, true)
	if err != nil {
		return nil, c.cleanup(err, false)
	}

	return future, nil
}

// Disconnect will send a DisconnectPacket and close the connection.
func (c *Client) Disconnect(timeout ...time.Duration) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	// check if connected
	if c.state.get() != stateConnected {
		return ErrClientNotConnected
	}

	// finish current packets
	if len(timeout) > 0 {
		c.futureStore.await(timeout[0])
	}

	// set state
	c.state.set(stateDisconnecting)

	// allocate packet
	m := packet.NewDisconnectPacket()

	// send disconnect packet
	err := c.send(m, false)

	return c.end(err)
}

// Close closes the client immediately without sending a DisconnectPacket and
// waiting for outgoing transmissions to finish.
func (c *Client) Close() error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	// check if connected
	if c.state.get() < stateConnecting {
		return ErrClientNotConnected
	}

	return c.end(nil)
}

/* processor goroutine */

// processes incoming packets
func (c *Client) processor() error {
	for {
		// get next packet from connection
		pkt, err := c.conn.Receive()
		if err != nil {
			// if we are disconnecting we can ignore the error
			if c.state.get() >= stateDisconnecting {

				return nil
			}

			// check if the connection has been closed by the server
			transportErr, ok := err.(transport.Error)
			if ok && transportErr.Code() == transport.ConnectionClose {
				return c.die(ErrClientUnexpectedClose, false)
			}

			// die on any other error
			return c.die(err, false)
		}

		c.log("Received: %s", pkt.String())

		// call handlers for packet types and ignore other packets
		switch pkt.Type() {
		case packet.CONNACK:
			err = c.processConnack(pkt.(*packet.ConnackPacket))
		case packet.SUBACK:
			err = c.processSuback(pkt.(*packet.SubackPacket))
		case packet.UNSUBACK:
			err = c.processUnsuback(pkt.(*packet.UnsubackPacket))
		case packet.PINGRESP:
			c.tracker.pong()
		case packet.PUBLISH:
			err = c.processPublish(pkt.(*packet.PublishPacket))
		case packet.PUBACK:
			err = c.processPubackAndPubcomp(pkt.(*packet.PubackPacket).PacketID)
		case packet.PUBCOMP:
			err = c.processPubackAndPubcomp(pkt.(*packet.PubcompPacket).PacketID)
		case packet.PUBREC:
			err = c.processPubrec(pkt.(*packet.PubrecPacket).PacketID)
		case packet.PUBREL:
			err = c.processPubrel(pkt.(*packet.PubrelPacket).PacketID)
		}

		// return eventual error
		if err != nil {
			return err // error has already been cleaned
		}
	}
}

// handle the incoming ConnackPacket
func (c *Client) processConnack(connack *packet.ConnackPacket) error {
	// check state
	if c.state.get() != stateConnecting {
		return nil // ignore wrongly sent ConnackPacket
	}

	// fill future
	c.connectFuture.SessionPresent = connack.SessionPresent
	c.connectFuture.ReturnCode = connack.ReturnCode

	// return connection denied error and close connection if not accepted
	if connack.ReturnCode != packet.ConnectionAccepted {
		c.state.set(stateConnacked)
		err := c.die(ErrClientConnectionDenied, true)
		c.connectFuture.complete()
		return err
	}

	// set state to connected
	c.state.set(stateConnected)

	// complete future
	c.connectFuture.complete()

	// retrieve stored packets
	packets, err := c.Session.AllPackets(session.Outgoing)
	if err != nil {
		return c.die(err, true)
	}

	// resend stored packets
	for _, pkt := range packets {
		// TODO: Set dup flag on PublishPackets
		err = c.send(pkt, false)
		if err != nil {
			return c.die(err, false)
		}
	}

	return nil
}

// handle an incoming SubackPacket
func (c *Client) processSuback(suback *packet.SubackPacket) error {
	// remove packet from store
	c.Session.DeletePacket(session.Outgoing, suback.PacketID)

	// get future
	subscribeFuture, ok := c.futureStore.get(suback.PacketID).(*SubscribeFuture)
	if !ok {
		return nil // ignore a wrongly sent SubackPacket
	}

	// complete future
	subscribeFuture.ReturnCodes = suback.ReturnCodes
	subscribeFuture.complete()

	// remove future from store
	c.futureStore.del(suback.PacketID)

	return nil
}

// handle an incoming UnsubackPacket
func (c *Client) processUnsuback(unsuback *packet.UnsubackPacket) error {
	// remove packet from store
	c.Session.DeletePacket(session.Outgoing, unsuback.PacketID)

	// get future
	unsubscribeFuture, ok := c.futureStore.get(unsuback.PacketID).(*UnsubscribeFuture)
	if !ok {
		return nil // ignore a wrongly sent UnsubackPacket
	}

	// complete future
	unsubscribeFuture.complete()

	// remove future from store
	c.futureStore.del(unsuback.PacketID)

	return nil
}

// handle an incoming PublishPacket
func (c *Client) processPublish(publish *packet.PublishPacket) error {
	if publish.QOS == 1 {
		puback := packet.NewPubackPacket()
		puback.PacketID = publish.PacketID

		// acknowledge qos 1 publish
		err := c.send(puback, false)
		if err != nil {
			return c.die(err, false)
		}
	}

	if publish.QOS == 2 {
		// store packet
		err := c.Session.SavePacket(session.Incoming, publish)
		if err != nil {
			return c.die(err, true)
		}

		pubrec := packet.NewPubrecPacket()
		pubrec.PacketID = publish.PacketID

		// signal qos 2 publish
		err = c.send(pubrec, false)
		if err != nil {
			return c.die(err, false)
		}
	}

	if publish.QOS <= 1 {
		// call callback
		c.forward(publish)
	}

	return nil
}

// handle an incoming PubackPacket or PubcompPacket
func (c *Client) processPubackAndPubcomp(packetID uint16) error {
	// remove packet from store
	c.Session.DeletePacket(session.Outgoing, packetID)

	// get future
	publishFuture, ok := c.futureStore.get(packetID).(*PublishFuture)
	if !ok {
		return nil // ignore a wrongly sent PubackPacket or PubcompPacket
	}

	// complete future
	publishFuture.complete()

	// remove future from store
	c.futureStore.del(packetID)

	return nil
}

// handle an incoming PubrecPacket
func (c *Client) processPubrec(packetID uint16) error {
	// allocate packet
	pubrel := packet.NewPubrelPacket()
	pubrel.PacketID = packetID

	// send packet and overwrite stored PublishPacket with PubrelPacket
	err := c.send(pubrel, true)
	if err != nil {
		return c.die(err, false)
	}

	return nil
}

// handle an incoming PubrelPacket
func (c *Client) processPubrel(packetID uint16) error {
	// get packet from store
	pkt, err := c.Session.LookupPacket(session.Incoming, packetID)
	if err != nil {
		return c.die(err, true)
	}

	// get packet from store
	publish, ok := pkt.(*packet.PublishPacket)
	if !ok {
		return nil // ignore a wrongly sent PubrelPacket
	}

	pubcomp := packet.NewPubcompPacket()
	pubcomp.PacketID = publish.PacketID

	// acknowledge PublishPacket
	err = c.send(pubcomp, false)
	if err != nil {
		return c.die(err, false)
	}

	// remove packet from store
	err = c.Session.DeletePacket(session.Incoming, packetID)
	if err != nil {
		return c.die(err, true)
	}

	// call callback
	c.forward(publish)

	return nil
}

/* pinger goroutine */

// manages the sending of ping packets to keep the connection alive
func (c *Client) pinger() error {
	for {
		window := c.tracker.window()

		if window < 0 {
			if c.tracker.pending() {
				return c.die(ErrClientMissingPong, true)
			}

			err := c.send(packet.NewPingreqPacket(), false)
			if err != nil {
				return c.die(err, false)
			}

			c.tracker.ping()
		} else {
			c.log(fmt.Sprintf("Delay KeepAlive by %s", window.String()))
		}

		select {
		case <-c.tomb.Dying():
			return tomb.ErrDying
		case <-time.After(window):
			continue
		}
	}
}

/* helpers */

// sends message and updates lastSend
func (c *Client) send(pkt packet.Packet, store bool) error {
	c.tracker.reset()

	// store packet
	if store {
		err := c.Session.SavePacket(session.Outgoing, pkt)
		if err != nil {
			return err
		}
	}

	// send packet
	err := c.conn.Send(pkt)
	if err != nil {
		return err
	}

	c.log("Sent: %s", pkt.String())

	return nil
}

// calls the callback with a new message
func (c *Client) forward(packet *packet.PublishPacket) {
	if c.Callback != nil {
		c.Callback(string(packet.Topic), packet.Payload, nil)
	}
}

// log a message
func (c *Client) log(format string, a ...interface{}) {
	if c.Logger != nil {
		c.Logger(fmt.Sprintf(format, a...))
	}
}

// will try to cleanup as many resources as possible
func (c *Client) cleanup(err error, close bool) error {
	// cancel connect future if appropriate
	if c.state.get() < stateConnacked && c.connectFuture != nil {
		c.connectFuture.cancel()
	}

	// set state
	c.state.set(stateDisconnected)

	// ensure that the connection gets closed
	if close {
		_err := c.conn.Close()
		if err == nil {
			err = _err
		}
	}

	// reset store
	if c.clean {
		_err := c.Session.Reset()
		if err == nil {
			err = _err
		}
	}

	// cancel futures
	for _, future := range c.futureStore.all() {
		future.cancel()
	}

	return err
}

// used for closing and cleaning up from inside internal goroutines
func (c *Client) die(err error, close bool) error {
	c.finish.Do(func() {
		err = c.cleanup(err, close)

		if c.Callback != nil {
			c.Callback("", nil, err)
		}
	})

	return err
}

// called by Disconnect and Close
func (c *Client) end(err error) error {
	// close connection
	err = c.cleanup(err, true)

	// shutdown goroutines
	c.tomb.Kill(nil)

	// wait for all goroutines to exit
	// goroutines will send eventual errors through the callback
	c.tomb.Wait()

	// do cleanup
	return err
}
