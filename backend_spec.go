package broker

import (
	"testing"

	"github.com/gomqtt/packet"
	"github.com/stretchr/testify/assert"
)

// BackendSpec will test a Backend implementation. The test will test all methods
// using a fake client. The passed builder callback should always return a
// fresh instances of the Backend. For Authentication tests, it expected that
// the Backend allows the login "allow:allow".
func BackendSpec(t *testing.T, builder func() Backend) {
	println("Running Backend Authentication Test")
	backendAuthenticationTest(t, builder())

	println("Running Backend Setup Test")
	backendSetupTest(t, builder())

	println("Running Backend Basic Queuing Test")
	backendBasicQueuingTest(t, builder())

	// TODO: Offline Message Test?

	println("Running Backend Retained Messages Test")
	backendRetainedMessagesTest(t, builder())
}

func backendAuthenticationTest(t *testing.T, backend Backend) {
	client := newFakeClient()

	ok, err := backend.Authenticate(client, "allow", "allow")
	assert.True(t, ok)
	assert.NoError(t, err)

	ok, err = backend.Authenticate(client, "deny", "deny")
	assert.False(t, ok)
	assert.NoError(t, err)
}

func backendSetupTest(t *testing.T, backend Backend) {
	client := newFakeClient()

	// has id and clean=false

	session1, resumed, err := backend.Setup(client, "foo", false)
	assert.NoError(t, err)
	assert.False(t, resumed)
	assert.NotNil(t, session1)

	session2, resumed, err := backend.Setup(client, "foo", false)
	assert.NoError(t, err)
	assert.True(t, resumed)
	assert.NotNil(t, session2)

	// has id and clean=true

	session3, resumed, err := backend.Setup(client, "foo", true)
	assert.NoError(t, err)
	assert.True(t, resumed)
	assert.NotNil(t, session3)

	// has other id and clean=false

	session4, resumed, err := backend.Setup(client, "bar", false)
	assert.NoError(t, err)
	assert.False(t, resumed)
	assert.NotNil(t, session4)

	// has no id and clean=true

	session5, resumed, err := backend.Setup(client, "", true)
	assert.NoError(t, err)
	assert.False(t, resumed)
	assert.NotNil(t, session5)

	session6, resumed, err := backend.Setup(client, "", true)
	assert.NoError(t, err)
	assert.False(t, resumed)
	assert.NotNil(t, session6)
}

func backendBasicQueuingTest(t *testing.T, backend Backend) {
	client1 := newFakeClient()
	client2 := newFakeClient()

	msg := &packet.Message{
		Topic:   "test",
		Payload: []byte("test"),
	}

	// setup both clients

	_, _, err := backend.Setup(client1, "client1", true)
	assert.NoError(t, err)

	_, _, err = backend.Setup(client2, "client2", true)
	assert.NoError(t, err)

	// subscribe both clients

	err = backend.Subscribe(client1, "test")
	assert.NoError(t, err)

	err = backend.Subscribe(client2, "test")
	assert.NoError(t, err)

	// publish message

	err = backend.Publish(client1, msg)
	assert.NoError(t, err)
	assert.Equal(t, msg, client1.in[0])
	assert.Equal(t, msg, client2.in[0])

	// unsubscribe one client

	err = backend.Unsubscribe(client2, "test")
	assert.NoError(t, err)

	// publish another message

	err = backend.Publish(client1, msg)
	assert.NoError(t, err)
	assert.Equal(t, 2, len(client1.in))
	assert.Equal(t, 1, len(client2.in))

	// terminate both clients

	err = backend.Terminate(client1)
	assert.NoError(t, err)

	err = backend.Terminate(client2)
	assert.NoError(t, err)
}

func backendRetainedMessagesTest(t *testing.T, backend Backend) {
	client := newFakeClient()

	msg1 := &packet.Message{
		Topic:   "foo",
		Payload: []byte("bar"),
		QOS:     1,
		Retain:  true,
	}

	msg2 := &packet.Message{
		Topic:   "foo/bar",
		Payload: []byte("bar"),
		QOS:     1,
		Retain:  true,
	}

	msg3 := &packet.Message{
		Topic:   "foo",
		Payload: []byte("bar"),
		QOS:     2,
		Retain:  true,
	}

	// should be empty
	err := backend.QueueRetained(client, "foo")
	assert.NoError(t, err)
	assert.Empty(t, client.in)

	err = backend.StoreRetained(client, msg1)
	assert.NoError(t, err)

	// should have one
	err = backend.QueueRetained(client, "foo")
	assert.NoError(t, err)
	assert.Equal(t, 1, len(client.in))
	client.in = nil

	err = backend.StoreRetained(client, msg2)
	assert.NoError(t, err)

	// should have two
	err = backend.QueueRetained(client, "#")
	assert.NoError(t, err)
	assert.Equal(t, 2, len(client.in))
	client.in = nil

	err = backend.StoreRetained(client, msg3)
	assert.NoError(t, err)

	// should have another
	err = backend.QueueRetained(client, "foo")
	assert.NoError(t, err)
	assert.Equal(t, 1, len(client.in))
	client.in = nil

	err = backend.ClearRetained(client, "foo")
	assert.NoError(t, err)

	// should have none
	err = backend.QueueRetained(client, "foo")
	assert.NoError(t, err)
	assert.Empty(t, client.in)
}
