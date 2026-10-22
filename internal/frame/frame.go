// Package frame implements the ESPHome native API framing:
//
//	0x00 || varint(len(payload)) || varint(msgType) || payload
//
// The length varint counts the payload only, not the type varint. Preamble
// 0x01 means the connection requires encryption; btproxy never sends it.
package frame

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/ankon/btproxy/tinypb"
)

// MaxPayload is the largest payload aioesphomeapi accepts in one frame.
const MaxPayload = 65535

// Errors returned by Decoder.
var (
	ErrNeedMore           = errors.New("frame: need more data")
	ErrBadPreamble        = errors.New("frame: bad preamble byte")
	ErrEncryptionRequired = errors.New("frame: connection requires encryption")
	ErrTooLarge           = errors.New("frame: payload exceeds maximum size")
)

// Encode frames a message. The returned buffer is freshly allocated.
func Encode(msgType uint32, payload []byte) []byte {
	buf := make([]byte, 0, len(payload)+16)
	buf = append(buf, 0x00)
	buf = tinypb.AppendUvarint(buf, uint64(len(payload)))
	buf = tinypb.AppendUvarint(buf, uint64(msgType))
	return append(buf, payload...)
}

// Decoder reassembles frames from a stream of bytes.
type Decoder struct {
	buf []byte
}

// Feed appends newly received bytes.
func (d *Decoder) Feed(p []byte) {
	d.buf = append(d.buf, p...)
}

// Next returns the next complete message, consuming its bytes. It returns
// ErrNeedMore until a full frame has been fed. Returned payloads alias the
// internal buffer and stay valid until the following Next call.
func (d *Decoder) Next() (msgType uint32, payload []byte, err error) {
	if len(d.buf) == 0 {
		return 0, nil, ErrNeedMore
	}
	switch d.buf[0] {
	case 0x00:
	case 0x01:
		return 0, nil, ErrEncryptionRequired
	default:
		return 0, nil, fmt.Errorf("%w: %#02x", ErrBadPreamble, d.buf[0])
	}

	length, n := binary.Uvarint(d.buf[1:])
	if n == 0 {
		return 0, nil, ErrNeedMore
	}
	if n < 0 {
		return 0, nil, tinypb.ErrOverflow
	}
	if length > MaxPayload {
		return 0, nil, ErrTooLarge
	}
	i := 1 + n

	typ, n := binary.Uvarint(d.buf[i:])
	if n == 0 {
		return 0, nil, ErrNeedMore
	}
	if n < 0 {
		return 0, nil, tinypb.ErrOverflow
	}
	i += n

	if length > uint64(len(d.buf)-i) {
		return 0, nil, ErrNeedMore
	}
	payload = d.buf[i : i+int(length) : i+int(length)]
	d.buf = d.buf[i+int(length):]
	return uint32(typ), payload, nil
}
