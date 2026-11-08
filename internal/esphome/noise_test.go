package esphome

import (
	"bytes"
	"github.com/ankon/btproxy/internal/frame"
	"github.com/flynn/noise"
	"github.com/stretchr/testify/require"
	"net"
	"testing"
	"time"
)

func noiseClient(t *testing.T, addr string, key []byte) (net.Conn, error) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(3 * time.Second))
	require.NoError(t, writeNoiseFrame(c, nil))
	_, err = readNoiseFrame(c)
	require.NoError(t, err)
	hs, err := noise.NewHandshakeState(noise.Config{CipherSuite: noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashSHA256), Pattern: noise.HandshakeNN, Initiator: true, Prologue: []byte("NoiseAPIInit\x00\x00"), PresharedKey: key, PresharedKeyPlacement: 0})
	require.NoError(t, err)
	msg, _, _, err := hs.WriteMessage(nil, nil)
	require.NoError(t, err)
	require.NoError(t, writeNoiseFrame(c, append([]byte{0}, msg...)))
	reply, err := readNoiseFrame(c)
	if err != nil {
		return nil, err
	}
	if reply[0] != 0 {
		return nil, frame.ErrBadPreamble
	}
	_, send, receive, err := hs.ReadMessage(nil, reply[1:])
	if err != nil {
		return nil, err
	}
	return &noiseConn{Conn: c, encrypt: send, decrypt: receive}, nil
}
func TestNoiseRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{42}, 32)
	_, addr := startServer(t, Config{Name: "noise-test", NoisePSK: key})
	c, err := noiseClient(t, addr.String(), key)
	require.NoError(t, err)
	_, err = c.Write(frame.Encode(MsgDeviceInfoRequest, nil))
	require.NoError(t, err)
	b := make([]byte, 4096)
	n, err := c.Read(b)
	require.NoError(t, err)
	d := &frame.Decoder{}
	d.Feed(b[:n])
	kind, payload, err := d.Next()
	require.NoError(t, err)
	require.Equal(t, uint32(MsgDeviceInfoResponse), kind)
	var info DeviceInfoResponse
	require.NoError(t, info.Decode(payload))
	require.False(t, info.UsesPassword)
}
func TestNoiseRejectsWrongKeyAndPlaintext(t *testing.T) {
	key := bytes.Repeat([]byte{42}, 32)
	_, addr := startServer(t, Config{Name: "noise-test", NoisePSK: key})
	_, err := noiseClient(t, addr.String(), bytes.Repeat([]byte{43}, 32))
	require.Error(t, err)
	c, err := net.Dial("tcp", addr.String())
	require.NoError(t, err)
	defer c.Close()
	c.SetDeadline(time.Now().Add(time.Second))
	_, err = c.Write(frame.Encode(MsgDeviceInfoRequest, nil))
	require.NoError(t, err)
	b := make([]byte, 3)
	n, err := c.Read(b)
	require.NoError(t, err)
	require.Equal(t, []byte{1, 0, 0}, b[:n])
}
func TestNoiseRejectsTamperedFrame(t *testing.T) {
	key := bytes.Repeat([]byte{42}, 32)
	_, addr := startServer(t, Config{Name: "noise-test", NoisePSK: key})
	c, err := noiseClient(t, addr.String(), key)
	require.NoError(t, err)
	nc := c.(*noiseConn)
	ciphertext, err := nc.encrypt.Encrypt(nil, nil, []byte{0, 9, 0, 0})
	require.NoError(t, err)
	ciphertext[0] ^= 1
	require.NoError(t, writeNoiseFrame(nc.Conn, ciphertext))
	_, err = nc.Read(make([]byte, 100))
	require.Error(t, err)
}
