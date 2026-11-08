package esphome

// ESPHome Noise framing, using flynn/noise for the cryptographic protocol.
// Protocol: Noise_NNpsk0_25519_ChaChaPoly_SHA256, prologue NoiseAPIInit\x00\x00.
import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/ankon/btproxy/internal/frame"
	"github.com/flynn/noise"
)

type noiseConn struct {
	net.Conn
	encrypt, decrypt *noise.CipherState
	pending          []byte
}

func readNoiseFrame(c net.Conn) ([]byte, error) {
	var h [3]byte
	if _, err := io.ReadFull(c, h[:]); err != nil {
		return nil, err
	}
	if h[0] != 1 {
		return nil, fmt.Errorf("Noise encryption required")
	}
	b := make([]byte, int(binary.BigEndian.Uint16(h[1:])))
	_, err := io.ReadFull(c, b)
	return b, err
}
func writeAll(c net.Conn, b []byte) error {
	for len(b) > 0 {
		n, err := c.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
func writeNoiseFrame(c net.Conn, b []byte) error {
	if len(b) > 65535 {
		return fmt.Errorf("encrypted frame too large")
	}
	h := []byte{1, byte(len(b) >> 8), byte(len(b))}
	return writeAll(c, append(h, b...))
}
func acceptNoise(c net.Conn, cfg Config) (net.Conn, error) {
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	hello, err := readNoiseFrame(c)
	if err != nil {
		// Lets aioesphomeapi detect encryption when probing in plaintext mode.
		_ = writeNoiseFrame(c, nil)
		return nil, err
	}
	if len(hello) != 0 {
		return nil, errors.New("invalid Noise client hello")
	}
	if err = writeNoiseFrame(c, append([]byte{1}, []byte(cfg.Name+"\x00"+cfg.MACAddress+"\x00")...)); err != nil {
		return nil, err
	}
	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite: noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashSHA256),
		Pattern:     noise.HandshakeNN, Initiator: false, Prologue: []byte("NoiseAPIInit\x00\x00"),
		PresharedKey: cfg.NoisePSK, PresharedKeyPlacement: 0,
	})
	if err != nil {
		return nil, err
	}
	request, err := readNoiseFrame(c)
	if err != nil {
		return nil, err
	}
	if len(request) < 1 || request[0] != 0 {
		return nil, errors.New("invalid Noise handshake")
	}
	if _, _, _, err = hs.ReadMessage(nil, request[1:]); err != nil {
		_ = writeNoiseFrame(c, append([]byte{1}, []byte("Handshake MAC failure")...))
		return nil, errors.New("invalid Noise key")
	}
	response, receive, send, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return nil, err
	}
	if err = writeNoiseFrame(c, append([]byte{0}, response...)); err != nil {
		return nil, err
	}
	_ = c.SetDeadline(time.Time{})
	return &noiseConn{Conn: c, encrypt: send, decrypt: receive}, nil
}
func (c *noiseConn) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	for len(c.pending) == 0 {
		encrypted, err := readNoiseFrame(c.Conn)
		if err != nil {
			return 0, err
		}
		msg, err := c.decrypt.Decrypt(nil, nil, encrypted)
		if err != nil {
			return 0, errors.New("Noise authentication failed")
		}
		if len(msg) < 4 || int(binary.BigEndian.Uint16(msg[2:4])) != len(msg)-4 {
			return 0, errors.New("invalid encrypted message length")
		}
		c.pending = frame.Encode(uint32(binary.BigEndian.Uint16(msg[:2])), msg[4:])
	}
	n := copy(b, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}
func (c *noiseConn) Write(b []byte) (int, error) {
	decoder := &frame.Decoder{}
	decoder.Feed(b)
	kind, payload, err := decoder.Next()
	if err != nil {
		return 0, err
	}
	if kind > 65535 || len(payload) > 65515 {
		return 0, errors.New("Noise message too large")
	}
	msg := []byte{byte(kind >> 8), byte(kind), byte(len(payload) >> 8), byte(len(payload))}
	encrypted, err := c.encrypt.Encrypt(nil, nil, append(msg, payload...))
	if err != nil {
		return 0, err
	}
	if err = writeNoiseFrame(c.Conn, encrypted); err != nil {
		return 0, err
	}
	return len(b), nil
}
