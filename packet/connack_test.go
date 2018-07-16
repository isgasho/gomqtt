package packet

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestConnackReturnCodes(t *testing.T) {
	assert.Equal(t, ConnectionAccepted.Error(), ConnackCode(0).Error())
	assert.Equal(t, ErrInvalidProtocolVersion.Error(), ConnackCode(1).Error())
	assert.Equal(t, ErrIdentifierRejected.Error(), ConnackCode(2).Error())
	assert.Equal(t, ErrServerUnavailable.Error(), ConnackCode(3).Error())
	assert.Equal(t, ErrBadUsernameOrPassword.Error(), ConnackCode(4).Error())
	assert.Equal(t, ErrNotAuthorized.Error(), ConnackCode(5).Error())
	assert.Equal(t, "unknown error", ConnackCode(6).Error())
}

func TestConnackInterface(t *testing.T) {
	pkt := NewConnack()

	assert.Equal(t, pkt.Type(), CONNACK)
	assert.Equal(t, "<Connack SessionPresent=false ReturnCode=0>", pkt.String())
}

func TestConnackDecode(t *testing.T) {
	pktBytes := []byte{
		byte(CONNACK << 4),
		2,
		0, // session not present
		0, // connection accepted
	}

	pkt := NewConnack()

	n, err := pkt.Decode(pktBytes)

	assert.NoError(t, err)
	assert.Equal(t, 4, n)
	assert.False(t, pkt.SessionPresent)
	assert.Equal(t, ConnectionAccepted, pkt.ReturnCode)
}

func TestConnackDecodeError1(t *testing.T) {
	pktBytes := []byte{
		byte(CONNACK << 4),
		3, // < wrong size
		0, // session not present
		0, // connection accepted
	}

	pkt := NewConnack()

	_, err := pkt.Decode(pktBytes)
	assert.Error(t, err)
}

func TestConnackDecodeError2(t *testing.T) {
	pktBytes := []byte{
		byte(CONNACK << 4),
		2,
		0, // session not present
		// < wrong packet size
	}

	pkt := NewConnack()

	_, err := pkt.Decode(pktBytes)
	assert.Error(t, err)
}

func TestConnackDecodeError3(t *testing.T) {
	pktBytes := []byte{
		byte(CONNACK << 4),
		2,
		64, // < wrong value
		0,  // connection accepted
	}

	pkt := NewConnack()

	_, err := pkt.Decode(pktBytes)
	assert.Error(t, err)
}

func TestConnackDecodeError4(t *testing.T) {
	pktBytes := []byte{
		byte(CONNACK << 4),
		2,
		0,
		6, // < wrong code
	}

	pkt := NewConnack()

	_, err := pkt.Decode(pktBytes)
	assert.Error(t, err)
}

func TestConnackDecodeError5(t *testing.T) {
	pktBytes := []byte{
		byte(CONNACK << 4),
		1, // < wrong remaining length
		0,
		6,
	}

	pkt := NewConnack()

	_, err := pkt.Decode(pktBytes)
	assert.Error(t, err)
}

func TestConnackEncode(t *testing.T) {
	pktBytes := []byte{
		byte(CONNACK << 4),
		2,
		1, // session present
		0, // connection accepted
	}

	pkt := NewConnack()
	pkt.ReturnCode = ConnectionAccepted
	pkt.SessionPresent = true

	dst := make([]byte, pkt.Len())
	n, err := pkt.Encode(dst)

	assert.NoError(t, err)
	assert.Equal(t, 4, n)
	assert.Equal(t, pktBytes, dst[:n])
}

func TestConnackEncodeError1(t *testing.T) {
	pkt := NewConnack()

	dst := make([]byte, 3) // < wrong buffer size
	n, err := pkt.Encode(dst)

	assert.Error(t, err)
	assert.Equal(t, 0, n)
}

func TestConnackEncodeError2(t *testing.T) {
	pkt := NewConnack()
	pkt.ReturnCode = 11 // < wrong return code

	dst := make([]byte, pkt.Len())
	n, err := pkt.Encode(dst)

	assert.Error(t, err)
	assert.Equal(t, 3, n)
}

func TestConnackEqualDecodeEncode(t *testing.T) {
	pktBytes := []byte{
		byte(CONNACK << 4),
		2,
		0, // session not present
		0, // connection accepted
	}

	pkt := NewConnack()
	n, err := pkt.Decode(pktBytes)

	assert.NoError(t, err)
	assert.Equal(t, 4, n)

	dst := make([]byte, pkt.Len())
	n2, err := pkt.Encode(dst)

	assert.NoError(t, err)
	assert.Equal(t, 4, n2)
	assert.Equal(t, pktBytes, dst[:n2])

	n3, err := pkt.Decode(dst)

	assert.NoError(t, err)
	assert.Equal(t, 4, n3)
}

func BenchmarkConnackEncode(b *testing.B) {
	pkt := NewConnack()
	pkt.ReturnCode = ConnectionAccepted
	pkt.SessionPresent = true

	buf := make([]byte, pkt.Len())

	for i := 0; i < b.N; i++ {
		_, err := pkt.Encode(buf)
		if err != nil {
			panic(err)
		}
	}
}

func BenchmarkConnackDecode(b *testing.B) {
	pktBytes := []byte{
		byte(CONNACK << 4),
		2,
		0, // session not present
		0, // connection accepted
	}

	pkt := NewConnack()

	for i := 0; i < b.N; i++ {
		_, err := pkt.Decode(pktBytes)
		if err != nil {
			panic(err)
		}
	}
}
