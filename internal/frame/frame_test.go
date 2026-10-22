package frame

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ankon/btproxy/tinypb"
)

// Golden frames produced by the reference C implementation
// (esphome-linux esphome_frame_message); see docs/protocol-notes.md.

// HELLO_FRAME: HelloResponse (type 2) with the golden pb payload.
const goldenHelloFrame = "0020020801100c1a11627470726f787920323032362e31302e302207627470726f7879"

// BLEADVS_FRAME: BluetoothLERawAdvertisementsResponse (type 93).
const goldenBLEAdvsFrame = "00395d0a1d08ffddf7e6bcd72a108d011800220e020106020a0007ff4c0010050f12" +
	"0a1808e6aa919aa3a40410751801220a05094142436403160f18"

// SWITCHSTATE_FRAME: SwitchStateResponse (type 26) key=100 state=true.
const goldenSwitchStateFrame = "00071a0d640000001001"

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	require.NoError(t, err, "bad golden hex")
	return b
}

func TestGoldenHelloFrame(t *testing.T) {
	payload := mustHex(t, "0801100c1a11627470726f787920323032362e31302e302207627470726f7879")
	assert.Equal(t, mustHex(t, goldenHelloFrame), Encode(2, payload), "hello frame")
}

func TestGoldenSwitchStateFrame(t *testing.T) {
	assert.Equal(t, mustHex(t, goldenSwitchStateFrame), Encode(26, mustHex(t, "0d640000001001")), "switch state frame")
}

func TestDecodeGoldenFrames(t *testing.T) {
	for _, tc := range []struct {
		name       string
		golden     string
		msgType    uint32
		payloadLen int
	}{
		{"hello", goldenHelloFrame, 2, 32},
		{"ble_advs", goldenBLEAdvsFrame, 93, 57},
		{"switch_state", goldenSwitchStateFrame, 26, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &Decoder{}
			d.Feed(mustHex(t, tc.golden))
			msgType, payload, err := d.Next()
			require.NoError(t, err)
			assert.Equal(t, tc.msgType, msgType, "message type")
			require.Len(t, payload, tc.payloadLen, "payload length")

			_, _, err = d.Next()
			assert.ErrorIs(t, err, ErrNeedMore, "no second frame")
		})
	}
}

func TestDecodeSplitAcrossFeeds(t *testing.T) {
	frameBytes := mustHex(t, goldenBLEAdvsFrame)
	for split := 0; split < len(frameBytes); split++ {
		t.Run(fmt.Sprintf("split_at_%d", split), func(t *testing.T) {
			d := &Decoder{}
			d.Feed(frameBytes[:split])
			_, _, err := d.Next()
			assert.ErrorIs(t, err, ErrNeedMore, "partial frame must not decode")

			d.Feed(frameBytes[split:])
			msgType, payload, err := d.Next()
			require.NoError(t, err)
			assert.Equal(t, uint32(93), msgType)
			assert.NotEmpty(t, payload)
		})
	}
}

func TestDecodeMultipleFramesOneFeed(t *testing.T) {
	d := &Decoder{}
	d.Feed(append(mustHex(t, goldenHelloFrame), mustHex(t, goldenSwitchStateFrame)...))
	want := []struct {
		msgType uint32
		len     int
	}{{2, 32}, {26, 7}}
	for i, w := range want {
		msgType, payload, err := d.Next()
		require.NoError(t, err, "frame %d", i)
		assert.Equal(t, w.msgType, msgType, "frame %d type", i)
		assert.Len(t, payload, w.len, "frame %d payload", i)
	}
}

func TestDecodeErrors(t *testing.T) {
	d := &Decoder{}
	d.Feed([]byte{0x01})
	_, _, err := d.Next()
	assert.ErrorIs(t, err, ErrEncryptionRequired, "preamble 0x01 means encryption required")

	d = &Decoder{}
	d.Feed([]byte{0x02, 0x01, 0x01})
	_, _, err = d.Next()
	assert.ErrorIs(t, err, ErrBadPreamble, "unknown preamble")

	d = &Decoder{}
	d.Feed([]byte{0x00, 0x80, 0x80, 0x04, 0x01}) // payload length 65536
	_, _, err = d.Next()
	assert.ErrorIs(t, err, ErrTooLarge, "oversized frame")

	d = &Decoder{}
	d.Feed([]byte{0x00, 0x05, 0x01})
	_, _, err = d.Next()
	assert.ErrorIs(t, err, ErrNeedMore, "incomplete frame")
}

func TestMaxPayloadRoundTrip(t *testing.T) {
	payload := bytes.Repeat([]byte{0xab}, MaxPayload)
	d := &Decoder{}
	d.Feed(Encode(93, payload))
	msgType, got, err := d.Next()
	require.NoError(t, err)
	assert.Equal(t, uint32(93), msgType)
	assert.True(t, bytes.Equal(got, payload), "max payload round trip")
}

func TestPayloadAliasesDecoderBuffer(t *testing.T) {
	// Returned payloads stay valid until the next Next call; make sure
	// feeding more data does not clobber them.
	d := &Decoder{}
	d.Feed(Encode(26, tinypb.AppendUvarint(nil, 0x64)))
	_, first, err := d.Next()
	require.NoError(t, err)
	snapshot := append([]byte(nil), first...)
	d.Feed(Encode(26, tinypb.AppendUvarint(nil, 0x65)))
	_, _, err = d.Next()
	require.NoError(t, err)
	assert.Equal(t, snapshot, first, "payload invalidated by next call")
}
