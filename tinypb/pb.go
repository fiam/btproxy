// Package pb implements the subset of the protobuf wire format used by the
// ESPHome native API: varint, fixed32, fixed64 and length-delimited fields.
//
// Encoding conventions follow the reference C implementation (esphome-linux):
// scalar fields are written unconditionally; empty strings and empty byte
// slices are omitted; callers decide whether an optional scalar is sent at
// all.
package tinypb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// Protobuf wire types.
const (
	WireVarint  = 0
	WireFixed64 = 1
	WireBytes   = 2
	WireFixed32 = 5
)

// Decoder errors.
var (
	ErrTruncated = errors.New("tinypb: truncated message")
	ErrOverflow  = errors.New("tinypb: varint overflows 64 bits")
	ErrWireType  = errors.New("tinypb: unsupported wire type")
	ErrFieldNum  = errors.New("tinypb: invalid field number")
)

// AppendUvarint appends v as a protobuf varint to dst.
func AppendUvarint(dst []byte, v uint64) []byte {
	return binary.AppendUvarint(dst, v)
}

// Writer encodes protobuf fields into a growable buffer.
type Writer struct {
	buf []byte
}

// NewWriter returns an empty Writer.
func NewWriter() *Writer { return &Writer{} }

// Bytes returns the encoded message.
func (w *Writer) Bytes() []byte { return w.buf }

// Len returns the number of encoded bytes so far.
func (w *Writer) Len() int { return len(w.buf) }

func (w *Writer) tag(field, wire int) {
	if field <= 0 {
		panic(fmt.Sprintf("tinypb: invalid field number %d", field))
	}
	w.buf = binary.AppendUvarint(w.buf, uint64(field)<<3|uint64(wire))
}

// Bool writes a bool field (always encoded, even when false).
func (w *Writer) Bool(field int, v bool) {
	w.tag(field, WireVarint)
	if v {
		w.buf = append(w.buf, 1)
	} else {
		w.buf = append(w.buf, 0)
	}
}

// Uint32 writes a uint32 field.
func (w *Writer) Uint32(field int, v uint32) {
	w.tag(field, WireVarint)
	w.buf = binary.AppendUvarint(w.buf, uint64(v))
}

// Uint64 writes a uint64 field.
func (w *Writer) Uint64(field int, v uint64) {
	w.tag(field, WireVarint)
	w.buf = binary.AppendUvarint(w.buf, v)
}

// Sint32 writes an int32 field using zigzag encoding (e.g. RSSI).
func (w *Writer) Sint32(field int, v int32) {
	// (v << 1) ^ (v >> 31) with int32 arithmetic: the signed right shift
	// yields 0 or -1, so the xor complements every bit for negatives.
	w.Uint32(field, uint32(v<<1)^uint32(v>>31))
}

// Fixed32 writes a fixed32 field (entity keys).
func (w *Writer) Fixed32(field int, v uint32) {
	w.tag(field, WireFixed32)
	w.buf = binary.LittleEndian.AppendUint32(w.buf, v)
}

// Float32 writes a float32 field (sensor states).
func (w *Writer) Float32(field int, v float32) {
	w.Fixed32(field, math.Float32bits(v))
}

// Fixed64 writes a fixed64 field.
func (w *Writer) Fixed64(field int, v uint64) {
	w.tag(field, WireFixed64)
	w.buf = binary.LittleEndian.AppendUint64(w.buf, v)
}

// String writes a string field; empty strings are omitted.
func (w *Writer) String(field int, s string) {
	if s == "" {
		return
	}
	w.bytes(field, []byte(s))
}

// BytesField writes a bytes field; empty slices are omitted.
func (w *Writer) BytesField(field int, b []byte) {
	if len(b) == 0 {
		return
	}
	w.bytes(field, b)
}

func (w *Writer) bytes(field int, b []byte) {
	w.tag(field, WireBytes)
	w.buf = binary.AppendUvarint(w.buf, uint64(len(b)))
	w.buf = append(w.buf, b...)
}

// Message writes a nested message field, encoding it with fn.
func (w *Writer) Message(field int, fn func(*Writer)) {
	sub := NewWriter()
	fn(sub)
	w.bytes(field, sub.buf)
}

// AppendRaw appends pre-encoded bytes (e.g. a nested message encoded by a
// previous Writer, for repeated fields).
func (w *Writer) AppendRaw(b []byte) {
	w.buf = append(w.buf, b...)
}

// Reader decodes protobuf fields from a message payload.
type Reader struct {
	buf []byte
}

// NewReader returns a Reader over b.
func NewReader(b []byte) *Reader { return &Reader{buf: b} }

// Next returns the next field number and wire type, or io.EOF when the
// message is exhausted. Unknown fields can be skipped with Skip.
func (r *Reader) Next() (field, wire int, err error) {
	if len(r.buf) == 0 {
		return 0, 0, io.EOF
	}
	v, n := binary.Uvarint(r.buf)
	if n == 0 {
		return 0, 0, ErrTruncated
	}
	if n < 0 {
		return 0, 0, ErrOverflow
	}
	r.buf = r.buf[n:]
	field, wire = int(v>>3), int(v&7)
	if field == 0 {
		return 0, 0, ErrFieldNum
	}
	switch wire {
	case WireVarint, WireFixed64, WireBytes, WireFixed32:
		return field, wire, nil
	default:
		// Wire types 3/4 (groups) and 6/7 are unused here.
		return 0, 0, fmt.Errorf("%w: %d", ErrWireType, wire)
	}
}

// Varint reads a raw varint value.
func (r *Reader) Varint() (uint64, error) {
	v, n := binary.Uvarint(r.buf)
	if n == 0 {
		return 0, ErrTruncated
	}
	if n < 0 {
		return 0, ErrOverflow
	}
	r.buf = r.buf[n:]
	return v, nil
}

// Bool reads a bool field value.
func (r *Reader) Bool() (bool, error) {
	v, err := r.Varint()
	return v != 0, err
}

// Uint32 reads a uint32 field value.
func (r *Reader) Uint32() (uint32, error) {
	v, err := r.Varint()
	return uint32(v), err
}

// Sint32 reads a zigzag-encoded int32 field value.
func (r *Reader) Sint32() (int32, error) {
	v, err := r.Varint()
	if err != nil {
		return 0, err
	}
	z := uint32(v)
	return int32(z>>1) ^ -int32(z&1), nil
}

// Fixed32 reads a fixed32 field value.
func (r *Reader) Fixed32() (uint32, error) {
	if len(r.buf) < 4 {
		return 0, ErrTruncated
	}
	v := binary.LittleEndian.Uint32(r.buf)
	r.buf = r.buf[4:]
	return v, nil
}

// Float32 reads a float32 field value.
func (r *Reader) Float32() (float32, error) {
	bits, err := r.Fixed32()
	return math.Float32frombits(bits), err
}

// Fixed64 reads a fixed64 field value.
func (r *Reader) Fixed64() (uint64, error) {
	if len(r.buf) < 8 {
		return 0, ErrTruncated
	}
	v := binary.LittleEndian.Uint64(r.buf)
	r.buf = r.buf[8:]
	return v, nil
}

// Bytes reads a length-delimited field. The returned slice aliases the
// reader's buffer and is only valid until the next read.
func (r *Reader) Bytes() ([]byte, error) {
	v, err := r.Varint()
	if err != nil {
		return nil, err
	}
	if v > uint64(len(r.buf)) {
		return nil, ErrTruncated
	}
	n := int(v)
	b := r.buf[:n:n]
	r.buf = r.buf[n:]
	return b, nil
}

// String reads a length-delimited string field.
func (r *Reader) String() (string, error) {
	b, err := r.Bytes()
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Skip discards a field with the given wire type.
func (r *Reader) Skip(wire int) error {
	switch wire {
	case WireVarint:
		_, err := r.Varint()
		return err
	case WireFixed64:
		_, err := r.Fixed64()
		return err
	case WireFixed32:
		_, err := r.Fixed32()
		return err
	case WireBytes:
		_, err := r.Bytes()
		return err
	default:
		return fmt.Errorf("%w: %d", ErrWireType, wire)
	}
}

// Remaining returns the number of unread bytes.
func (r *Reader) Remaining() int { return len(r.buf) }

// DiscardAll skips every remaining field, validating the message structure.
func (r *Reader) DiscardAll() error {
	for {
		_, wire, err := r.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := r.Skip(wire); err != nil {
			return err
		}
	}
}
