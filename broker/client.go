package broker

import (
	"errors"
	"sync/atomic"
	"time"

	"github.com/256dpi/gomqtt/packet"
	"github.com/256dpi/gomqtt/session"
	"github.com/256dpi/gomqtt/transport"

	"gopkg.in/tomb.v2"
)

// A Session is used to get packet ids, persist incoming/outgoing packets, store
// subscriptions and the will.
type Session interface {
	// NextID should return the next id for outgoing packets.
	NextID() packet.ID

	// SavePacket should store a packet in the session. An eventual existing
	// packet with the same id should be quietly overwritten.
	SavePacket(session.Direction, packet.Generic) error

	// LookupPacket should retrieve a packet from the session using the packet id.
	LookupPacket(session.Direction, packet.ID) (packet.Generic, error)

	// DeletePacket should remove a packet from the session. The method should
	// not return an error if no packet with the specified id does exists.
	DeletePacket(session.Direction, packet.ID) error

	// AllPackets should return all packets currently saved in the session.
	AllPackets(session.Direction) ([]packet.Generic, error)

	// SaveSubscription should store the subscription in the session. An eventual
	// subscription with the same topic should be quietly overwritten.
	SaveSubscription(packet.Subscription) error

	// LookupSubscription should match a topic against the stored subscriptions
	// and eventually return the first found subscription.
	LookupSubscription(topic string) (*packet.Subscription, error)

	// DeleteSubscription should remove the subscription from the session. The
	// method should not return an error if no subscription with the specified
	// topic does exist.
	DeleteSubscription(topic string) error

	// AllSubscriptions should return all subscriptions currently saved in the
	// session.
	AllSubscriptions() ([]packet.Subscription, error)

	// SaveWill should store the will message.
	SaveWill(*packet.Message) error

	// LookupWill should retrieve the will message.
	LookupWill() (*packet.Message, error)

	// ClearWill should remove the will message from the store.
	ClearWill() error
}

// Ack is executed by the Backend or Client to signal that a message will be
// delivered under the selected qos level and is therefore safe to be deleted
// from either queue.
type Ack func(message *packet.Message)

// A Backend provides the effective brokering functionality to its clients.
type Backend interface {
	// Authenticate should authenticate the client using the user and password
	// values and return true if the client is eligible to continue or false
	// when the broker should terminate the connection.
	Authenticate(client *Client, user, password string) (ok bool, err error)

	// Setup is called when a new client comes online and is successfully
	// authenticated. Setup should return the already stored session for the
	// supplied id or create and return a new one if it is missing or a clean
	// session is requested. If the supplied id has a zero length, a new
	// temporary session should returned that is not stored further. The backend
	// should also close any existing clients that use the same client id.
	//
	// Note: In this call the Backend may also allocate other resources and
	// setup the client for further usage as the broker will acknowledge the
	// connection when the call returns. The Terminate function is called for
	// every client that Setup has been called for.
	Setup(client *Client, id string, clean bool) (a Session, resumed bool, err error)

	// Restored is called after the client has restored packets and
	// subscriptions from the session. The client will begin with processing
	// incoming packets and queued messages.
	Restored(client *Client) error

	// Subscribe should subscribe the passed client to the specified topic.
	// Incoming messages that match the supplied subscription should be added to
	// queue that is drained when Dequeue is called. The subscription will be
	// acknowledged if the call returns without an error.
	//
	// Retained messages that match the supplied subscription should be added to
	// a temporary queue that is also drained when Dequeue is called. Retained
	// messages are not part of the stored session queue as they are anyway
	// redelivered using the stored subscription mechanism.
	//
	// Subscribe is also called to resubscribe stored subscriptions between calls
	// to Setup and Restored. Retained messages that are delivered as a result of
	// resubscribing a stored subscription must be delivered with the retain flag
	// set to false.
	Subscribe(client *Client, subs []packet.Subscription, stored bool) error

	// Unsubscribe should unsubscribe the passed client from the specified topic.
	// The unsubscription will be acknowledged if the call returns without an error.
	Unsubscribe(client *Client, topics []string) error

	// Publish should forward the passed message to all other clients that hold
	// a subscription that matches the messages topic. It should also add the
	// message to all sessions that have a matching offline subscription. The
	// later may only apply to messages with a QoS greater than 0. If an Ack is
	// provided, the message will be acknowledged when called during or after
	// the call to Publish.
	//
	// If the retained flag is set, messages with a payload should replace the
	// currently retained message. Otherwise, the currently retained message
	// should be removed. The flag should be cleared before publishing the
	// message to subscribed clients.
	Publish(client *Client, msg *packet.Message, ack Ack) error

	// Dequeue is called by the Client to obtain the next message from the queue
	// and must return either a message or an error. The backend must only return
	// no message and no error if the client's Closing channel has been closed.
	// The Backend may return an Ack to receive a signal that the message is being
	// delivered under the selected qos level and is therefore safe to be deleted
	// from the queue. The Client might dequeue other messages before acknowledging
	// a message.
	Dequeue(client *Client) (*packet.Message, Ack, error)

	// Terminate is called when the client goes offline. Terminate should
	// unsubscribe the passed client from all previously subscribed topics. The
	// backend may also convert a clients subscriptions to offline subscriptions.
	//
	// Note: The Backend may also cleanup previously allocated resources for
	// that client as the broker will close the connection when the call
	// returns.
	Terminate(client *Client) error
}

// ErrExpectedConnect is returned when the first received packet is not a
// ConnectPacket.
var ErrExpectedConnect = errors.New("expected a ConnectPacket as the first packet")

// ErrNotAuthorized is returned when a client is not authorized.
var ErrNotAuthorized = errors.New("client is not authorized")

// ErrMissingSession is returned if the backend does not return a session.
var ErrMissingSession = errors.New("no session returned from Backend")

// ErrClientDisconnected is returned if a client disconnects cleanly.
var ErrClientDisconnected = errors.New("client has disconnected")

// ErrClientClosed is returned if a client is being closed by the broker.
var ErrClientClosed = errors.New("client has been closed")

const (
	clientConnecting uint32 = iota
	clientConnected
	clientDisconnected
)

type outgoing struct {
	pkt packet.Generic
	msg *packet.Message
	ack Ack
}

// A Client represents a remote client that is connected to the broker.
type Client struct {
	// PacketPrefetch may be set during Setup to control the number of packets
	// that are read by Client and made available for processing. Will default
	// to 10 if not set.
	PacketPrefetch int

	// ParallelPublishes may be set during Setup to control the number of
	// parallel calls to Publish a client can perform. Will default to 10.
	ParallelPublishes int

	// ParallelDequeues may be set during Setup to control the number of
	// parallel calls to Dequeue a client can perform. Will default to 10.
	ParallelDequeues int

	// read-only
	backend Backend
	logger  Logger
	conn    transport.Conn

	// atomically written and read
	state uint32

	// set during connect
	id      string
	session Session

	incoming chan packet.Generic
	outgoing chan outgoing

	publishTokens chan struct{}
	dequeueTokens chan struct{}

	tomb tomb.Tomb
	done chan struct{}
}

// NewClient takes over a connection and returns a Client.
func NewClient(backend Backend, logger Logger, conn transport.Conn) *Client {
	// create client
	c := &Client{
		state:   clientConnecting,
		backend: backend,
		logger:  logger,
		conn:    conn,
		done:    make(chan struct{}),
	}

	// start processor
	c.tomb.Go(c.processor)

	// run cleanup goroutine
	go func() {
		// wait for death and cleanup
		c.tomb.Wait()
		c.cleanup()

		// close channel
		close(c.done)
	}()

	return c
}

// Session returns the current Session used by the client.
func (c *Client) Session() Session {
	return c.session
}

// ID returns the clients id that has been supplied during connect.
func (c *Client) ID() string {
	return c.id
}

// Conn returns the client's underlying connection. Calls to SetReadLimit,
// SetBuffers, LocalAddr and RemoteAddr are safe.
func (c *Client) Conn() transport.Conn {
	return c.conn
}

// Close will immediately close the client.
func (c *Client) Close() {
	c.tomb.Kill(ErrClientClosed)
	c.conn.Close()
}

// Closing returns a channel that is closed when the client is closing.
func (c *Client) Closing() <-chan struct{} {
	return c.tomb.Dying()
}

// Closed returns a channel that is closed when the client is closed.
func (c *Client) Closed() <-chan struct{} {
	return c.done
}

/* goroutines */

// main processor
func (c *Client) processor() error {
	c.log(NewConnection, c, nil, nil, nil)

	// get first packet from connection
	pkt, err := c.conn.Receive()
	if err != nil {
		return c.die(TransportError, err)
	}

	c.log(PacketReceived, c, pkt, nil, nil)

	// get connect
	connect, ok := pkt.(*packet.ConnectPacket)
	if !ok {
		return c.die(ClientError, ErrExpectedConnect)
	}

	// process connect
	err = c.processConnect(connect)
	if err != nil {
		return err // error has already been cleaned
	}

	// start dequeuer and sender
	c.tomb.Go(c.dequeuer)
	c.tomb.Go(c.sender)

	for {
		// check if still alive
		if !c.tomb.Alive() {
			return tomb.ErrDying
		}

		// receive next packet
		pkt, err := c.conn.Receive()
		if err != nil {
			return c.die(TransportError, err)
		}

		c.log(PacketReceived, c, pkt, nil, nil)

		// process packet
		err = c.processPacket(pkt)
		if err != nil {
			return err // error has already been cleaned
		}
	}
}

// message dequeuer
func (c *Client) dequeuer() error {
	for {
		// acquire dequeue token
		select {
		case <-c.dequeueTokens:
			// continue
		case <-c.tomb.Dying():
			return tomb.ErrDying
		}

		// request next message
		msg, ack, err := c.backend.Dequeue(c)
		if err != nil {
			return c.die(BackendError, err)
		} else if msg == nil {
			return tomb.ErrDying
		}

		c.log(MessageDequeued, c, nil, msg, nil)

		// queue message
		select {
		case c.outgoing <- outgoing{msg: msg, ack: ack}:
			// continue
		case <-c.tomb.Dying():
			return tomb.ErrDying
		}
	}
}

// message and packet sender
func (c *Client) sender() error {
	for {
		select {
		case e := <-c.outgoing:
			if e.pkt != nil {
				// send acknowledgment
				err := c.sendAck(e.pkt)
				if err != nil {
					return err // error has already been cleaned
				}
			} else if e.msg != nil {
				// send message
				err := c.sendMessage(e.msg, e.ack)
				if err != nil {
					return err // error has already been cleaned
				}
			}
		case <-c.tomb.Dying():
			return tomb.ErrDying
		}
	}
}

/* packet handling */

// handle an incoming ConnackPacket
func (c *Client) processConnect(pkt *packet.ConnectPacket) error {
	// save id
	c.id = pkt.ClientID

	// authenticate
	ok, err := c.backend.Authenticate(c, pkt.Username, pkt.Password)
	if err != nil {
		return c.die(BackendError, err)
	}

	// prepare connack packet
	connack := packet.NewConnackPacket()
	connack.ReturnCode = packet.ConnectionAccepted
	connack.SessionPresent = false

	// check authentication
	if !ok {
		// set return code
		connack.ReturnCode = packet.ErrNotAuthorized

		// send connack
		err = c.send(connack, false)
		if err != nil {
			return c.die(TransportError, err)
		}

		// close client
		return c.die(ClientError, ErrNotAuthorized)
	}

	// set state
	atomic.StoreUint32(&c.state, clientConnected)

	// set keep alive
	if pkt.KeepAlive > 0 {
		c.conn.SetReadTimeout(time.Duration(pkt.KeepAlive) * 1500 * time.Millisecond)
	} else {
		c.conn.SetReadTimeout(0)
	}

	// retrieve session
	s, resumed, err := c.backend.Setup(c, pkt.ClientID, pkt.CleanSession)
	if err != nil {
		return c.die(BackendError, err)
	} else if s == nil {
		return c.die(BackendError, ErrMissingSession)
	}

	// set session present
	connack.SessionPresent = !pkt.CleanSession && resumed

	// assign session
	c.session = s

	// set default packet prefetch
	if c.PacketPrefetch <= 0 {
		c.PacketPrefetch = 10
	}

	// set default unacked publishes
	if c.ParallelPublishes <= 0 {
		c.ParallelPublishes = 10
	}

	// set default parallel dequeues
	if c.ParallelDequeues <= 0 {
		c.ParallelDequeues = 10
	}

	// prepare publish tokens
	c.publishTokens = make(chan struct{}, c.ParallelPublishes)
	for i := 0; i < c.ParallelPublishes; i++ {
		c.publishTokens <- struct{}{}
	}

	// prepare dequeue tokens
	c.dequeueTokens = make(chan struct{}, c.ParallelDequeues)
	for i := 0; i < c.ParallelDequeues; i++ {
		c.dequeueTokens <- struct{}{}
	}

	// crate incoming queue
	c.incoming = make(chan packet.Generic, c.PacketPrefetch)

	// create outgoing queue
	c.outgoing = make(chan outgoing, c.ParallelPublishes+c.ParallelDequeues)

	// save will if present
	if pkt.Will != nil {
		err = c.session.SaveWill(pkt.Will)
		if err != nil {
			return c.die(SessionError, err)
		}
	}

	// send connack
	err = c.send(connack, false)
	if err != nil {
		return c.die(TransportError, err)
	}

	// retrieve stored packets
	packets, err := c.session.AllPackets(session.Outgoing)
	if err != nil {
		return c.die(SessionError, err)
	}

	// resend stored packets
	for _, pkt := range packets {
		// set the dup flag on a publish packet
		publish, ok := pkt.(*packet.PublishPacket)
		if ok {
			publish.Dup = true
		}

		// send packet
		err = c.send(pkt, true)
		if err != nil {
			return c.die(TransportError, err)
		}
	}

	// get stored subscriptions
	subs, err := s.AllSubscriptions()
	if err != nil {
		return c.die(SessionError, err)
	}

	// resubscribe subscriptions
	err = c.backend.Subscribe(c, subs, true)
	if err != nil {
		return c.die(BackendError, err)
	}

	// signal restored client
	err = c.backend.Restored(c)
	if err != nil {
		return c.die(BackendError, err)
	}

	return nil
}

// handle an incoming packet
func (c *Client) processPacket(pkt packet.Generic) error {
	// prepare error
	var err error

	// handle individual packets
	switch typedPkt := pkt.(type) {
	case *packet.SubscribePacket:
		err = c.processSubscribe(typedPkt)
	case *packet.UnsubscribePacket:
		err = c.processUnsubscribe(typedPkt)
	case *packet.PublishPacket:
		err = c.processPublish(typedPkt)
	case *packet.PubackPacket:
		err = c.processPubackAndPubcomp(typedPkt.ID)
	case *packet.PubcompPacket:
		err = c.processPubackAndPubcomp(typedPkt.ID)
	case *packet.PubrecPacket:
		err = c.processPubrec(typedPkt.ID)
	case *packet.PubrelPacket:
		err = c.processPubrel(typedPkt.ID)
	case *packet.PingreqPacket:
		err = c.processPingreq()
	case *packet.DisconnectPacket:
		err = c.processDisconnect()
	}

	// return eventual error
	if err != nil {
		return err // error has already been cleaned
	}

	return nil
}

// handle an incoming PingreqPacket
func (c *Client) processPingreq() error {
	// send a pingresp packet
	err := c.send(packet.NewPingrespPacket(), true)
	if err != nil {
		return c.die(TransportError, err)
	}

	return nil
}

// handle an incoming SubscribePacket
func (c *Client) processSubscribe(pkt *packet.SubscribePacket) error {
	// prepare suback packet
	suback := packet.NewSubackPacket()
	suback.ReturnCodes = make([]byte, len(pkt.Subscriptions))
	suback.ID = pkt.ID

	// handle contained subscriptions
	for i, subscription := range pkt.Subscriptions {
		// save subscription in session
		err := c.session.SaveSubscription(subscription)
		if err != nil {
			return c.die(SessionError, err)
		}

		// save to be granted qos
		suback.ReturnCodes[i] = subscription.QOS
	}

	// subscribe client to queue
	err := c.backend.Subscribe(c, pkt.Subscriptions, false)
	if err != nil {
		return c.die(BackendError, err)
	}

	// send suback
	err = c.send(suback, true)
	if err != nil {
		return c.die(TransportError, err)
	}

	return nil
}

// handle an incoming UnsubscribePacket
func (c *Client) processUnsubscribe(pkt *packet.UnsubscribePacket) error {
	// unsubscribe topics
	err := c.backend.Unsubscribe(c, pkt.Topics)
	if err != nil {
		return c.die(BackendError, err)
	}

	// handle contained topics
	for _, topic := range pkt.Topics {
		// remove subscription from session
		err = c.session.DeleteSubscription(topic)
		if err != nil {
			return c.die(SessionError, err)
		}
	}

	// prepare unsuback packet
	unsuback := packet.NewUnsubackPacket()
	unsuback.ID = pkt.ID

	// send packet
	err = c.send(unsuback, true)
	if err != nil {
		return c.die(TransportError, err)
	}

	return nil
}

// handle an incoming PublishPacket
func (c *Client) processPublish(publish *packet.PublishPacket) error {
	// handle qos 0 flow
	if publish.Message.QOS == 0 {
		// publish message to others
		err := c.backend.Publish(c, &publish.Message, nil)
		if err != nil {
			return c.die(BackendError, err)
		}

		c.log(MessagePublished, c, nil, &publish.Message, nil)
	}

	// handle qos 1 flow
	if publish.Message.QOS == 1 {
		// prepare puback
		puback := packet.NewPubackPacket()
		puback.ID = publish.ID

		// acquire publish token
		select {
		case <-c.publishTokens:
			// continue
		case <-c.tomb.Dying():
			return tomb.ErrDying
		}

		// publish message to others and queue puback if ack is called
		err := c.backend.Publish(c, &publish.Message, func(msg *packet.Message) {
			c.log(MessageAcknowledged, c, nil, msg, nil)

			select {
			case c.outgoing <- outgoing{pkt: puback}:
			case <-c.tomb.Dying():
			}
		})
		if err != nil {
			return c.die(BackendError, err)
		}

		c.log(MessagePublished, c, nil, &publish.Message, nil)
	}

	// handle qos 2 flow
	if publish.Message.QOS == 2 {
		// store packet
		err := c.session.SavePacket(session.Incoming, publish)
		if err != nil {
			return c.die(SessionError, err)
		}

		// prepare pubrec packet
		pubrec := packet.NewPubrecPacket()
		pubrec.ID = publish.ID

		// signal qos 2 pubrec
		err = c.send(pubrec, true)
		if err != nil {
			return c.die(TransportError, err)
		}
	}

	return nil
}

// handle an incoming PubackPacket or PubcompPacket
func (c *Client) processPubackAndPubcomp(id packet.ID) error {
	// remove packet from store
	err := c.session.DeletePacket(session.Outgoing, id)
	if err != nil {
		return c.die(SessionError, err)
	}

	// put back dequeue token
	c.dequeueTokens <- struct{}{}

	return nil
}

// handle an incoming PubrecPacket
func (c *Client) processPubrec(id packet.ID) error {
	// allocate packet
	pubrel := packet.NewPubrelPacket()
	pubrel.ID = id

	// overwrite stored PublishPacket with PubrelPacket
	err := c.session.SavePacket(session.Outgoing, pubrel)
	if err != nil {
		return c.die(SessionError, err)
	}

	// send packet
	err = c.send(pubrel, true)
	if err != nil {
		return c.die(TransportError, err)
	}

	return nil
}

// handle an incoming PubrelPacket
func (c *Client) processPubrel(id packet.ID) error {
	// get publish packet from store
	pkt, err := c.session.LookupPacket(session.Incoming, id)
	if err != nil {
		return c.die(SessionError, err)
	}

	// get packet from store
	publish, ok := pkt.(*packet.PublishPacket)
	if !ok {
		return nil // ignore a wrongly sent PubrelPacket
	}

	// prepare pubcomp packet
	pubcomp := packet.NewPubcompPacket()
	pubcomp.ID = publish.ID

	// the pubrec packet will be cleared from the session once the pubcomp
	// has been sent

	// acquire publish token
	select {
	case <-c.publishTokens:
		// continue
	case <-c.tomb.Dying():
		return tomb.ErrDying
	}

	// publish message to others and queue pubcomp if ack is called
	err = c.backend.Publish(c, &publish.Message, func(msg *packet.Message) {
		c.log(MessageAcknowledged, c, nil, msg, nil)

		select {
		case c.outgoing <- outgoing{pkt: pubcomp}:
		case <-c.tomb.Dying():
		}
	})
	if err != nil {
		return c.die(BackendError, err)
	}

	c.log(MessagePublished, c, nil, &publish.Message, nil)

	return nil
}

// handle an incoming DisconnectPacket
func (c *Client) processDisconnect() error {
	// clear will
	err := c.session.ClearWill()
	if err != nil {
		return c.die(SessionError, err)
	}

	// mark client as cleanly disconnected
	atomic.StoreUint32(&c.state, clientDisconnected)

	// close underlying connection (triggers cleanup)
	c.conn.Close()

	c.log(ClientDisconnected, c, nil, nil, nil)

	return ErrClientDisconnected
}

/* helpers */

// send messages
func (c *Client) sendMessage(msg *packet.Message, ack Ack) error {
	// prepare publish packet
	publish := packet.NewPublishPacket()
	publish.Message = *msg

	// get stored subscription
	sub, err := c.session.LookupSubscription(publish.Message.Topic)
	if err != nil {
		return c.die(SessionError, err)
	}

	// check subscription
	if sub != nil {
		// respect maximum qos
		if publish.Message.QOS > sub.QOS {
			publish.Message.QOS = sub.QOS
		}
	}

	// set packet id
	if publish.Message.QOS > 0 {
		publish.ID = c.session.NextID()
	}

	// store packet if at least qos 1
	if publish.Message.QOS > 0 {
		err := c.session.SavePacket(session.Outgoing, publish)
		if err != nil {
			return c.die(SessionError, err)
		}
	}

	// acknowledge message since it has been stored in the session if a quality
	// of service > 0 is requested
	if ack != nil {
		ack(msg)

		c.log(MessageAcknowledged, c, nil, msg, nil)
	}

	// send packet
	err = c.send(publish, true)
	if err != nil {
		return c.die(TransportError, err)
	}

	// immediately put back dequeue token for qos 0 messages
	if publish.Message.QOS == 0 {
		c.dequeueTokens <- struct{}{}
	}

	c.log(MessageForwarded, c, nil, msg, nil)

	return nil
}

// send an acknowledgment
func (c *Client) sendAck(pkt packet.Generic) error {
	// send packet
	err := c.send(pkt, true)
	if err != nil {
		return err // error already handled
	}

	// remove pubrec from session
	if pubcomp, ok := pkt.(*packet.PubcompPacket); ok {
		err = c.session.DeletePacket(session.Incoming, pubcomp.ID)
		if err != nil {
			return c.die(SessionError, err)
		}
	}

	// put back publish token
	c.publishTokens <- struct{}{}

	return nil
}

// send a packet
func (c *Client) send(pkt packet.Generic, buffered bool) error {
	// send packet
	var err error
	if buffered {
		err = c.conn.BufferedSend(pkt)
	} else {
		err = c.conn.Send(pkt)
	}

	// check error
	if err != nil {
		return err
	}

	c.log(PacketSent, c, pkt, nil, nil)

	return nil
}

/* error handling and logging */

// log a message
func (c *Client) log(event LogEvent, client *Client, pkt packet.Generic, msg *packet.Message, err error) {
	if c.logger != nil {
		c.logger(event, client, pkt, msg, err)
	}
}

// used for closing and cleaning up from internal goroutines
func (c *Client) die(event LogEvent, err error) error {
	// report error
	c.log(event, c, nil, nil, err)

	// close connection if requested
	c.conn.Close()

	return err
}

// will try to cleanup as many resources as possible
func (c *Client) cleanup() {
	// check session
	if c.session != nil && atomic.LoadUint32(&c.state) == clientConnected {
		// get will
		will, err := c.session.LookupWill()
		if err != nil {
			c.log(SessionError, c, nil, nil, err)
		}

		// publish will message
		if will != nil {
			// publish message to others
			err := c.backend.Publish(c, will, nil)
			if err != nil {
				c.log(BackendError, c, nil, nil, err)
			}

			c.log(MessagePublished, c, nil, will, nil)
		}
	}

	// remove client from the queue
	if atomic.LoadUint32(&c.state) >= clientConnected {
		err := c.backend.Terminate(c)
		if err != nil {
			c.log(BackendError, c, nil, nil, err)
		}
	}

	c.log(LostConnection, c, nil, nil, nil)
}
