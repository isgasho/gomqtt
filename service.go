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
	"fmt"
	"sync"
	"time"

	"github.com/gomqtt/packet"
	"github.com/gomqtt/session"
	"github.com/jpillora/backoff"
	"gopkg.in/tomb.v2"
)

// ClearSession will connect/disconnect once with a clean session request to force
// the broker to reset the clients session. This is useful in situations where
// its not clear in what state the last session was left.
func ClearSession(url string, clientID string) error {
	client := New()

	// prepare options
	options := NewOptions()
	options.ClientID = clientID
	options.CleanSession = true

	// connect to broker
	future, err := client.Connect(url, options)
	if err != nil {
		return err
	}

	// wait for connack
	future.Wait()

	// check if connection has been accepted
	if future.ReturnCode != packet.ConnectionAccepted {
		return ErrClientConnectionDenied
	}

	// disconnect
	return client.Disconnect()
}

type publish struct {
	topic   string
	payload []byte
	qos     byte
	retain  bool
	future  *PublishFuture
}

type subscribe struct {
	filters map[string]byte
	future  *SubscribeFuture
}

type unsubscribe struct {
	topics []string
	future *UnsubscribeFuture
}

type Online func(resumed bool)

type Message func(topic string, payload []byte)

type Offline func()

// Service is an abstraction for Client that provides a stable interface to the
// application, while it automatically connects and reconnects clients in the
// background. Errors are not returned but logged using the Logger callback.
// All methods return Futures that get completed once the acknowledgements are
// received. Once the services is stopped all waiting futures get canceled.
//
// Note: If clean session is false and there are packets in the store, messages
// might get completed after starting without triggering any futures to complete.
type Service struct {
	broker  string
	options *Options
	backoff *backoff.Backoff

	Session session.Session
	Online  Online
	Message Message
	Offline Offline
	Logger  Logger

	MinReconnectDelay time.Duration
	MaxReconnectDelay time.Duration
	ConnectTimeout    time.Duration
	DisconnectTimeout time.Duration

	subscribeQueue   chan *subscribe
	unsubscribeQueue chan *unsubscribe
	publishQueue     chan *publish
	futureStore      *futureStore

	started bool
	mutex   sync.Mutex
	tomb    *tomb.Tomb
}

func NewService() *Service {
	return &Service{
		Session:           session.NewMemorySession(),
		MinReconnectDelay: 1 * time.Second,
		MaxReconnectDelay: 32 * time.Second,
		ConnectTimeout:    5 * time.Second,
		DisconnectTimeout: 10 * time.Second,
		subscribeQueue:    make(chan *subscribe, 100),
		unsubscribeQueue:  make(chan *unsubscribe, 100),
		publishQueue:      make(chan *publish, 100),
		futureStore:       newFutureStore(),
	}
}

func (s *Service) Start(url string, opts *Options) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.started {
		return
	}

	s.broker = url
	s.options = opts

	s.backoff = &backoff.Backoff{
		Min:    s.MinReconnectDelay,
		Max:    s.MaxReconnectDelay,
		Factor: 2,
	}

	s.started = true

	s.tomb = &tomb.Tomb{}
	s.tomb.Go(s.reconnector)
}

// Publish will send a PublishPacket containing the passed parameters. It will
// return a PublishFuture that gets completed once the quality of service flow
// has been completed.
func (s *Service) Publish(topic string, payload []byte, qos byte, retain bool) *PublishFuture {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	future := &PublishFuture{}
	future.initialize()

	s.publishQueue <- &publish{
		topic:   topic,
		payload: payload,
		qos:     qos,
		retain:  retain,
		future:  future,
	}

	return future
}

// Subscribe will send a SubscribePacket containing one topic to subscribe.
func (s *Service) Subscribe(topic string, qos byte) *SubscribeFuture {
	return s.SubscribeMultiple(map[string]byte{
		topic: qos,
	})
}

// SubscribeMultiple will send a SubscribePacket containing multiple topics to
// subscribe.
func (s *Service) SubscribeMultiple(filters map[string]byte) *SubscribeFuture {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	future := &SubscribeFuture{}
	future.initialize()

	s.subscribeQueue <- &subscribe{
		filters: filters,
		future:  future,
	}

	return future
}

// Unsubscribe will send a UnsubscribePacket containing one topic to unsubscribe.
func (s *Service) Unsubscribe(topic string) *UnsubscribeFuture {
	return s.UnsubscribeMultiple([]string{topic})
}

// UnsubscribeMultiple will send a UnsubscribePacket containing multiple
// topics to unsubscribe.
func (s *Service) UnsubscribeMultiple(topics []string) *UnsubscribeFuture {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	future := &UnsubscribeFuture{}
	future.initialize()

	s.unsubscribeQueue <- &unsubscribe{
		topics: topics,
		future: future,
	}

	return future
}

func (s *Service) Stop() {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if !s.started {
		return
	}

	s.started = false

	s.tomb.Kill(nil)
	s.tomb.Wait()
}

func (s *Service) reconnector() error {
	first := true

	for {
		if first {
			// no delay on first attempt
			first = false
		} else {
			// get backoff duration
			d := s.backoff.Duration()
			s.log("Delay Reconnect: %v", d)

			// sleep but return on Stop
			select {
			case <-time.After(d):
			case <-s.tomb.Dying():
				return tomb.ErrDying
			}
		}

		s.log("Next Reconnect")

		// prepare the stop channel
		fail := make(chan struct{})

		// try once to get a client
		client, resumed := s.getClient(fail)
		if client == nil {
			continue
		}

		// run callback
		s.notify(true, resumed)

		// run dispatcher on client
		dying := s.dispatcher(client, fail)

		// run callback
		s.notify(false, false)

		// return goroutine if dying
		if dying {
			return tomb.ErrDying
		}
	}
}

// will try to connect one client to the broker
func (s *Service) getClient(fail chan struct{}) (*Client, bool) {
	client := New()
	client.Session = s.Session
	client.Logger = s.Logger
	client.futureStore = s.futureStore

	client.Callback = func(topic string, payload []byte, err error) {
		if err != nil {
			s.log("Error: %v", err)
			close(fail)
			return
		}

		// call the handler
		if s.Message != nil {
			s.Message(topic, payload)
		}
	}

	future, err := client.Connect(s.broker, s.options)
	if err != nil {
		s.log("Connect Error: %v", err)
		return nil, false
	}

	err = future.Wait(s.ConnectTimeout)

	if err == ErrFutureCanceled {
		s.log("Connack: %v", err)
		return nil, false
	}

	if err == ErrFutureTimeout {
		client.Close()

		s.log("Connack: %v", err)
		return nil, false
	}

	if future.ReturnCode != packet.ConnectionAccepted {
		client.Close()

		s.log("Connack: %s", future.ReturnCode.Error())
		return nil, false
	}

	return client, future.SessionPresent
}

// reads from the queues and calls the current client
func (s *Service) dispatcher(client *Client, fail chan struct{}) bool {
	for {
		select {
		case sub := <-s.subscribeQueue:
			future, err := client.SubscribeMultiple(sub.filters)
			if err != nil {
				s.log("Subscribe Error: %v", err)

				//TODO: requeue subscribe?
				return false
			}

			sub.future.bind(future)
		case unsub := <-s.unsubscribeQueue:
			future, err := client.UnsubscribeMultiple(unsub.topics)
			if err != nil {
				s.log("Unsubscribe Error: %v", err)

				//TODO: requeue unsubscribe?
				return false
			}

			unsub.future.bind(future)
		case msg := <-s.publishQueue:
			future, err := client.Publish(msg.topic, msg.payload, msg.qos, msg.retain)
			if err != nil {
				s.log("Publish Error: %v", err)
				return false
			}

			msg.future.bind(future)
		case <-s.tomb.Dying():
			// disconnect client on Stop
			err := client.Disconnect(s.DisconnectTimeout)
			if err != nil {
				s.log("Disconnect Error: %v", err)
			}

			return true
		case <-fail:
			// TODO: Prevent cancellation of all futures.
			return false
		}
	}
}

// run online or offline callback
func (s *Service) notify(online bool, resumed bool) {
	if online {
		s.Online(resumed)
	} else {
		s.Offline()
	}
}

// log a message
func (s *Service) log(format string, a ...interface{}) {
	if s.Logger != nil {
		s.Logger(fmt.Sprintf(format, a...))
	}
}
