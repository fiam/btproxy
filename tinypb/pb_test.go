package tinypb

import (
	"encoding/hex"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Golden vectors produced by the reference C implementation
// (esphome-linux src/esphome_proto.c); see docs/protocol-notes.md.
// HELLO: HelloResponse with version 1.12, server_info "btproxy 2026.10.0",
// name "btproxy".
const goldenHello = "0801100c1a11627470726f787920323032362e31302e302207627470726f7879"

// CONNECT: ConnectResponse with invalid_password=false.
const goldenConnect = "0800"

// DEVICEINFO: DeviceInfoResponse for btproxy, BLE proxy flags 0x21.
const goldenDeviceInfo = "08001207627470726f78791a1161613a62623a63633a64643a65653a6666" +
	"2209323032362e31302e30" + // esphome_version
	"2a144f637420323220323032362032323a33393a3030" + // compilation_time
	"3207627470726f7879" + // model
	"3800" + // has_deep_sleep=false
	"6207627470726f7879" + // manufacturer
	"6a07627470726f7879" + // friendly_name
	"7821" + // bluetooth_proxy_feature_flags=0x21
	"92011161613a62623a63633a64643a65653a6666" // bluetooth_mac_address

// BLEADVS: BluetoothLERawAdvertisementsResponse with two advertisements.
const goldenBLEAdvs = "0a1d08ffddf7e6bcd72a108d011800220e020106020a0007ff4c0010050f12" +
	"0a1808e6aa919aa3a40410751801220a05094142436403160f18"

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	require.NoError(t, err, "bad golden hex")
	return b
}

func TestGoldenHello(t *testing.T) {
	w := NewWriter()
	w.Uint32(1, 1) // api_version_major
	w.Uint32(2, 12)
	w.String(3, "btproxy 2026.10.0") // server_info
	w.String(4, "btproxy")           // name
	assert.Equal(t, mustHex(t, goldenHello), w.Bytes(), "hello payload")
}

func TestGoldenConnect(t *testing.T) {
	w := NewWriter()
	w.Bool(1, false) // invalid_password
	assert.Equal(t, mustHex(t, goldenConnect), w.Bytes(), "connect payload")
}

func TestGoldenDeviceInfo(t *testing.T) {
	w := NewWriter()
	w.Bool(1, false)                 // uses_password
	w.String(2, "btproxy")           // name
	w.String(3, "aa:bb:cc:dd:ee:ff") // mac_address
	w.String(4, "2026.10.0")         // esphome_version
	w.String(5, "Oct 22 2026 22:39:00")
	w.String(6, "btproxy") // model
	w.Bool(7, false)       // has_deep_sleep
	// field 10 webserver_port omitted (zero)
	w.String(12, "btproxy")           // manufacturer
	w.String(13, "btproxy")           // friendly_name
	w.Uint32(15, 0x21)                // bluetooth_proxy_feature_flags
	w.String(16, "")                  // suggested_area omitted
	w.String(18, "aa:bb:cc:dd:ee:ff") // bluetooth_mac_address
	assert.Equal(t, mustHex(t, goldenDeviceInfo), w.Bytes(), "device info payload")
}

func TestGoldenBLEAdvertisements(t *testing.T) {
	adv1 := NewWriter()
	adv1.Uint64(1, 0xAABBCCDDEEFF)
	adv1.Sint32(2, -71)
	adv1.Uint32(3, 0) // address_type: public
	adv1.BytesField(4, []byte{
		0x02, 0x01, 0x06, 0x02, 0x0a, 0x00,
		0x07, 0xff, 0x4c, 0x00, 0x10, 0x05, 0x0f, 0x12,
	})

	adv2 := NewWriter()
	adv2.Uint64(1, 0x112233445566)
	adv2.Sint32(2, -59)
	adv2.Uint32(3, 1) // address_type: random
	adv2.BytesField(4, []byte{
		0x05, 0x09, 'A', 'B', 'C', 'd', 0x03, 0x16, 0x0f, 0x18,
	})

	// The batch wraps each advertisement as a field-1 nested message.
	w := NewWriter()
	w.BytesField(1, adv1.Bytes()) // repeated field 1
	w.BytesField(1, adv2.Bytes())

	assert.Equal(t, mustHex(t, goldenBLEAdvs), w.Bytes(), "ble advs payload")
}

func TestVarintRoundTrip(t *testing.T) {
	values := []uint64{
		0, 1, 0x7f, 0x80, 0x3fff, 0x4000, 1<<32 - 1,
		0xAABBCCDDEEFF, 1<<63 - 1, 1 << 63,
		^uint64(0),
	}
	for _, v := range values {
		w := NewWriter()
		w.Uint64(1, v)
		r := NewReader(w.Bytes())
		field, wire, err := r.Next()
		require.NoError(t, err, "varint %d", v)
		require.Equal(t, 1, field)
		require.Equal(t, WireVarint, wire)
		got, err := r.Varint()
		require.NoError(t, err, "varint %d", v)
		assert.Equal(t, v, got, "varint round trip")
	}
}

func TestVarintTruncated(t *testing.T) {
	// All continuation bytes, no terminator.
	_, err := NewReader([]byte{0x80, 0x80, 0x80}).Varint()
	assert.ErrorIs(t, err, ErrTruncated)

	// Ten bytes, the last with bits beyond the 64th — an overflow.
	_, err = NewReader([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}).Varint()
	assert.ErrorIs(t, err, ErrOverflow)
}

func TestSint32RoundTrip(t *testing.T) {
	values := []int32{0, 1, -1, 71, -71, -59, 127, -128, 1 << 30, -(1 << 30)}
	for _, v := range values {
		w := NewWriter()
		w.Sint32(1, v)
		r := NewReader(w.Bytes())
		_, _, err := r.Next()
		require.NoError(t, err, "sint %d", v)
		got, err := r.Sint32()
		require.NoError(t, err, "sint %d", v)
		assert.Equal(t, v, got, "sint32 round trip")
	}
}

func TestFixed32RoundTrip(t *testing.T) {
	w := NewWriter()
	w.Fixed32(1, 100) // BLE Scanning switch key
	assert.Equal(t, []byte{0x0d, 100, 0, 0, 0}, w.Bytes(), "fixed32 encoding")

	r := NewReader(w.Bytes())
	field, wire, err := r.Next()
	require.NoError(t, err)
	require.Equal(t, 1, field)
	require.Equal(t, WireFixed32, wire)
	got, err := r.Fixed32()
	require.NoError(t, err)
	assert.Equal(t, uint32(100), got)
}

func TestSkipUnknownFields(t *testing.T) {
	w := NewWriter()
	w.Uint32(1, 123) // field we care about
	w.String(5, "hello")
	w.Fixed64(6, 99)
	w.Uint64(7, 1<<40)
	w.BytesField(9, []byte{1, 2, 3})
	w.Fixed32(10, 77)
	w.Uint32(12, 456)

	r := NewReader(w.Bytes())
	var got uint32
	var seen int
	for {
		field, wire, err := r.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		seen++
		if field == 1 {
			got, err = r.Uint32()
			require.NoError(t, err)
			continue
		}
		require.NoError(t, r.Skip(wire), "skip field %d", field)
	}
	assert.Equal(t, uint32(123), got, "field 1 value")
	assert.Equal(t, 7, seen, "fields visited")
	assert.Zero(t, r.Remaining(), "buffer consumed")
}

func TestReaderErrors(t *testing.T) {
	// Field number 0.
	_, _, err := NewReader([]byte{0x00}).Next()
	assert.ErrorIs(t, err, ErrFieldNum)

	// Unsupported wire type (6).
	_, _, err = NewReader([]byte{0x36}).Next()
	assert.ErrorIs(t, err, ErrWireType)

	// Length-delimited field longer than the buffer.
	r := NewReader([]byte{0x0a, 0x05, 'a'})
	_, _, err = r.Next()
	require.NoError(t, err)
	_, err = r.Bytes()
	assert.ErrorIs(t, err, ErrTruncated)

	// Fixed32 shorter than 4 bytes.
	r = NewReader([]byte{0x0d, 0x01, 0x02})
	_, _, err = r.Next()
	require.NoError(t, err)
	_, err = r.Fixed32()
	assert.ErrorIs(t, err, ErrTruncated)
}

func TestMessageNested(t *testing.T) {
	var nested []byte
	w := NewWriter()
	w.Message(1, func(sub *Writer) {
		sub.Uint64(1, 42)
		sub.Sint32(2, -5)
		nested = append(nested[:0], sub.Bytes()...)
	})
	r := NewReader(w.Bytes())
	field, wire, err := r.Next()
	require.NoError(t, err)
	require.Equal(t, 1, field)
	require.Equal(t, WireBytes, wire)
	b, err := r.Bytes()
	require.NoError(t, err)
	assert.Equal(t, nested, b, "nested message bytes")
}

func TestDiscardAll(t *testing.T) {
	w := NewWriter()
	w.String(1, "x")
	w.Uint32(2, 3)
	w.Fixed64(3, 4)
	w.BytesField(4, []byte{9})
	assert.NoError(t, NewReader(w.Bytes()).DiscardAll())

	w = NewWriter()
	w.BytesField(1, []byte{1, 2, 3, 4, 5, 6})
	err := NewReader([]byte{0x0a, 0x05, 'a'}).DiscardAll()
	assert.Error(t, err, "truncated message should fail DiscardAll")
}
