package esphome

import (
	"encoding/hex"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ankon/btproxy/internal/frame"
	"github.com/ankon/btproxy/tinypb"
)

// Golden payloads produced by the reference C implementation
// (switch_proto.c, esphome_proto.c); see docs/protocol-notes.md.
const (
	goldenListSwitch   = "0a0c626c655f7363616e6e696e6715640000001a0c424c45205363616e6e696e672a136d64693a626c7565746f6f74682d617564696f"
	goldenSwitchState  = "0d640000001001"
	goldenSwitchFrame  = "00071a0d640000001001"
	goldenBLEAdvsFrame = "00395d0a1d08ffddf7e6bcd72a108d011800220e020106020a0007ff4c0010050f12" +
		"0a1808e6aa919aa3a40410751801220a05094142436403160f18"
	goldenBLEAdvsPayload = "0a1d08ffddf7e6bcd72a108d011800220e020106020a0007ff4c0010050f12" +
		"0a1808e6aa919aa3a40410751801220a05094142436403160f18"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	require.NoError(t, err, "bad golden hex")
	return b
}

func TestGoldenListEntitiesSwitch(t *testing.T) {
	sw := ListEntitiesSwitchResponse{
		ObjectID: "ble_scanning",
		Key:      100,
		Name:     "BLE Scanning",
		Icon:     "mdi:bluetooth-audio",
	}
	assert.Equal(t, mustHex(t, goldenListSwitch), sw.Encode())
}

func TestGoldenSwitchState(t *testing.T) {
	st := SwitchStateResponse{Key: 100, State: true}
	assert.Equal(t, mustHex(t, goldenSwitchState), st.Encode())
	assert.Equal(t, mustHex(t, goldenSwitchFrame), frame.Encode(MsgSwitchStateResponse, st.Encode()))
}

func TestGoldenRawAdvertisements(t *testing.T) {
	batch := BluetoothLERawAdvertisementsResponse{Advertisements: []*BluetoothLERawAdvertisement{
		{
			Address:     0xAABBCCDDEEFF,
			RSSI:        -71,
			AddressType: 0,
			Data: []byte{
				0x02, 0x01, 0x06, 0x02, 0x0a, 0x00,
				0x07, 0xff, 0x4c, 0x00, 0x10, 0x05, 0x0f, 0x12,
			},
		},
		{
			Address:     0x112233445566,
			RSSI:        -59,
			AddressType: 1,
			Data: []byte{
				0x05, 0x09, 'A', 'B', 'C', 'd', 0x03, 0x16, 0x0f, 0x18,
			},
		},
	}}
	payload := batch.Encode()
	assert.Equal(t, mustHex(t, goldenBLEAdvsPayload), payload)
	assert.Equal(t, mustHex(t, goldenBLEAdvsFrame), frame.Encode(MsgBluetoothLERawAdvertisementsResponse, payload))
}

func TestGeneratedDecodeGolden(t *testing.T) {
	var st SwitchStateResponse
	require.NoError(t, st.Decode(mustHex(t, goldenSwitchState)))
	assert.Equal(t, uint32(100), st.Key)
	assert.True(t, st.State)

	var sw ListEntitiesSwitchResponse
	require.NoError(t, sw.Decode(mustHex(t, goldenListSwitch)))
	assert.Equal(t, "ble_scanning", sw.ObjectID)
	assert.Equal(t, uint32(100), sw.Key)
	assert.Equal(t, "BLE Scanning", sw.Name)
	assert.Equal(t, "mdi:bluetooth-audio", sw.Icon)

	var batch BluetoothLERawAdvertisementsResponse
	require.NoError(t, batch.Decode(mustHex(t, goldenBLEAdvsPayload)))
	require.Len(t, batch.Advertisements, 2)
	assert.Equal(t, uint64(0xAABBCCDDEEFF), batch.Advertisements[0].Address)
	assert.Equal(t, int32(-71), batch.Advertisements[0].RSSI)
	assert.Equal(t, uint32(0), batch.Advertisements[0].AddressType)
	assert.Equal(t, uint64(0x112233445566), batch.Advertisements[1].Address)
	assert.Equal(t, int32(-59), batch.Advertisements[1].RSSI)
	assert.Equal(t, uint32(1), batch.Advertisements[1].AddressType)
}

func TestDecodeSkipsUnknownFields(t *testing.T) {
	// Golden switch state plus unknown fields of every wire type.
	w := tinypb.NewWriter()
	w.Fixed32(1, 42)
	w.Bool(2, false)
	w.String(9, "unexpected")
	w.Uint64(10, 1<<40)
	w.BytesField(11, []byte{1, 2})
	w.Fixed64(12, 99)

	var cmd SwitchCommandRequest
	require.NoError(t, cmd.Decode(w.Bytes()))
	assert.Equal(t, uint32(42), cmd.Key)
	assert.False(t, cmd.State)
}

func TestDecodeRejectsTruncated(t *testing.T) {
	var st SwitchStateResponse
	assert.Error(t, st.Decode([]byte{0x0d, 0x01, 0x02}))
}

func TestPackMAC(t *testing.T) {
	v, err := PackMAC("AA:BB:CC:DD:EE:FF")
	require.NoError(t, err)
	assert.Equal(t, uint64(0xAABBCCDDEEFF), v)

	v, err = PackMAC("11:22:33:44:55:66")
	require.NoError(t, err)
	assert.Equal(t, uint64(0x112233445566), v)

	for _, bad := range []string{"", "aa:bb", "aa:bb:cc:dd:ee:ff:00", "aa:bb:cc:dd:ee:gg"} {
		_, err := PackMAC(bad)
		assert.Error(t, err, "MAC %q", bad)
	}
}

// --- integration: server + client over TCP ---

type testClient struct {
	t    *testing.T
	conn net.Conn
	dec  *frame.Decoder
}

func newTestClient(t *testing.T, addr net.Addr) *testClient {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr.String(), 2*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	return &testClient{t: t, conn: conn, dec: &frame.Decoder{}}
}

func (c *testClient) send(msgType uint32, payload []byte) {
	c.t.Helper()
	_, err := c.conn.Write(frame.Encode(msgType, payload))
	require.NoError(c.t, err)
}

// recv returns the next message, skipping types in skip.
func (c *testClient) recv(skip ...uint32) (uint32, []byte) {
	c.t.Helper()
	buf := make([]byte, 4096)
	for {
		msgType, payload, err := c.dec.Next()
		if err == nil {
			if contains(skip, msgType) {
				continue
			}
			return msgType, payload
		}
		require.ErrorIs(c.t, err, frame.ErrNeedMore)
		n, err := c.conn.Read(buf)
		require.NoError(c.t, err)
		c.dec.Feed(buf[:n])
	}
}

func contains(vs []uint32, v uint32) bool {
	for _, x := range vs {
		if x == v {
			return true
		}
	}
	return false
}

func startServer(t *testing.T, cfg Config) (*Server, net.Addr) {
	t.Helper()
	if cfg.Name == "" {
		cfg.Name = "btproxy"
		cfg.MACAddress = "aa:bb:cc:dd:ee:ff"
		cfg.BluetoothMAC = "aa:bb:cc:dd:ee:ff"
	}
	srv := NewServer(cfg, testLogger(t))
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(srv.Close)
	return srv, l.Addr()
}

func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(testWriter{t}, nil))
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}

func TestHandshakeFlow(t *testing.T) {
	srv, addr := startServer(t, Config{
		Name:           "btproxy",
		MACAddress:     "aa:bb:cc:dd:ee:ff",
		BluetoothMAC:   "aa:bb:cc:dd:ee:ff",
		ESPHomeVersion: "2026.10.0",
	})
	_ = srv

	c := newTestClient(t, addr)

	// Hello
	c.send(MsgHelloRequest, (&HelloRequest{Client: "test"}).Encode())
	msgType, payload := c.recv()
	require.Equal(t, uint32(MsgHelloResponse), msgType)
	var hello HelloResponse
	require.NoError(t, hello.Decode(payload))
	assert.Equal(t, uint32(1), hello.APIVersionMajor)
	assert.Equal(t, uint32(12), hello.APIVersionMinor)
	assert.Equal(t, "btproxy", hello.Name)
	assert.Contains(t, hello.ServerInfo, "btproxy")

	// Connect
	c.send(MsgConnectRequest, nil)
	msgType, payload = c.recv()
	require.Equal(t, uint32(MsgConnectResponse), msgType)
	var conn ConnectResponse
	require.NoError(t, conn.Decode(payload))
	assert.False(t, conn.InvalidPassword)

	// DeviceInfo
	c.send(MsgDeviceInfoRequest, nil)
	msgType, payload = c.recv()
	require.Equal(t, uint32(MsgDeviceInfoResponse), msgType)
	var di DeviceInfoResponse
	require.NoError(t, di.Decode(payload))
	assert.Equal(t, "btproxy", di.Name)
	assert.Equal(t, "aa:bb:cc:dd:ee:ff", di.BluetoothMACAddress)
	assert.Equal(t, uint32(0x21), di.BluetoothProxyFeatureFlags)

	// ListEntities: switch, sensor, text sensor, then done.
	c.send(MsgListEntitiesRequest, nil)
	msgType, payload = c.recv()
	require.Equal(t, uint32(MsgListEntitiesSwitchResponse), msgType)
	assert.Equal(t, mustHex(t, goldenListSwitch), payload)
	msgType, payload = c.recv()
	require.Equal(t, uint32(MsgListEntitiesSensorResponse), msgType)
	var sensorList ListEntitiesSensorResponse
	require.NoError(t, sensorList.Decode(payload))
	assert.Equal(t, "advertisements", sensorList.ObjectID)
	assert.Equal(t, SensorKeyAdvertisements, sensorList.Key)
	assert.Equal(t, "adv", sensorList.UnitOfMeasurement)
	assert.Equal(t, uint32(3), sensorList.StateClass, "TOTAL_INCREASING")
	msgType, payload = c.recv()
	require.Equal(t, uint32(MsgListEntitiesTextSensorResponse), msgType)
	var tsList ListEntitiesTextSensorResponse
	require.NoError(t, tsList.Decode(payload))
	assert.Equal(t, "uptime", tsList.ObjectID)
	assert.Equal(t, TextSensorKeyUptime, tsList.Key)
	assert.Equal(t, "Uptime", tsList.Name)
	msgType, _ = c.recv()
	assert.Equal(t, uint32(MsgListEntitiesDoneResponse), msgType)

	// SubscribeStates: switch state, sensor state, then text sensor state.
	c.send(MsgSubscribeStatesRequest, nil)
	msgType, payload = c.recv()
	require.Equal(t, uint32(MsgSwitchStateResponse), msgType)
	assert.Equal(t, mustHex(t, goldenSwitchState), payload)
	msgType, payload = c.recv()
	require.Equal(t, uint32(MsgSensorStateResponse), msgType)
	var sensorState SensorStateResponse
	require.NoError(t, sensorState.Decode(payload))
	assert.Equal(t, SensorKeyAdvertisements, sensorState.Key)
	assert.Equal(t, float32(0), sensorState.State, "no advertisements yet")
	msgType, payload = c.recv()
	require.Equal(t, uint32(MsgTextSensorStateResponse), msgType)
	var tsState TextSensorStateResponse
	require.NoError(t, tsState.Decode(payload))
	assert.Equal(t, TextSensorKeyUptime, tsState.Key)
	assert.Regexp(t, `^\d+[dhms]( \d+[dhms])*$`, tsState.State)

	// Ping
	c.send(MsgPingRequest, nil)
	msgType, payload = c.recv()
	assert.Equal(t, uint32(MsgPingResponse), msgType)
	assert.Empty(t, payload)

	// GetTime
	c.send(MsgGetTimeRequest, nil)
	msgType, payload = c.recv()
	assert.Equal(t, uint32(MsgGetTimeResponse), msgType)
	assert.NotEmpty(t, payload)

	// Server state sanity.
	assert.True(t, srv.Scanning())
}

func TestSwitchCommandTogglesScanning(t *testing.T) {
	controlEvents := make(chan bool, 8)
	srv, addr := startServer(t, Config{Name: "btproxy", MACAddress: "aa:bb:cc:dd:ee:ff"})
	srv.ScanControl = func(on bool) { controlEvents <- on }

	c := newTestClient(t, addr)
	handshake(t, c)

	// Subscribe with RAW: scanning should turn on via ScanControl.
	c.send(MsgSubscribeBluetoothLEAdvertisementsRequest,
		(&SubscribeBluetoothLEAdvertisementsRequest{Flags: SubFlagRaw}).Encode())
	select {
	case on := <-controlEvents:
		assert.True(t, on)
	case <-time.After(2 * time.Second):
		t.Fatal("ScanControl not called on subscribe")
	}

	// Toggle the switch off.
	c.send(MsgSwitchCommandRequest, (&SwitchCommandRequest{Key: SwitchKeyBLEScanning, State: false}).Encode())
	msgType, payload := c.recv()
	require.Equal(t, uint32(MsgSwitchStateResponse), msgType)
	var st SwitchStateResponse
	require.NoError(t, st.Decode(payload))
	assert.Equal(t, SwitchKeyBLEScanning, st.Key)
	assert.False(t, st.State)
	assert.False(t, srv.Scanning())

	select {
	case on := <-controlEvents:
		assert.False(t, on)
	case <-time.After(2 * time.Second):
		t.Fatal("ScanControl not called on switch off")
	}

	// Toggle back on.
	c.send(MsgSwitchCommandRequest, (&SwitchCommandRequest{Key: SwitchKeyBLEScanning, State: true}).Encode())
	msgType, _ = c.recv()
	require.Equal(t, uint32(MsgSwitchStateResponse), msgType)
	assert.True(t, srv.Scanning())
	select {
	case on := <-controlEvents:
		assert.True(t, on)
	case <-time.After(2 * time.Second):
		t.Fatal("ScanControl not called on switch on")
	}
}

func TestBroadcastAdvertisements(t *testing.T) {
	controlEvents := make(chan bool, 8)
	srv, addr := startServer(t, Config{Name: "btproxy", MACAddress: "aa:bb:cc:dd:ee:ff"})
	srv.ScanControl = func(on bool) { controlEvents <- on }

	c1 := newTestClient(t, addr)
	handshake(t, c1)
	c2 := newTestClient(t, addr)
	handshake(t, c2)

	// Client 1 subscribes with RAW, client 2 does not. The ScanControl
	// event proves the server has registered the subscription.
	c1.send(MsgSubscribeBluetoothLEAdvertisementsRequest,
		(&SubscribeBluetoothLEAdvertisementsRequest{Flags: SubFlagRaw}).Encode())
	select {
	case on := <-controlEvents:
		require.True(t, on)
	case <-time.After(2 * time.Second):
		t.Fatal("ScanControl not called on subscribe")
	}

	advs := []*BluetoothLERawAdvertisement{
		{
			Address:     0xAABBCCDDEEFF,
			RSSI:        -71,
			AddressType: 0,
			Data: []byte{
				0x02, 0x01, 0x06, 0x02, 0x0a, 0x00,
				0x07, 0xff, 0x4c, 0x00, 0x10, 0x05, 0x0f, 0x12,
			},
		},
	}
	srv.SendAdvertisements(advs)

	// The subscribed client receives the type-93 frame with exactly the
	// C-compatible encoding.
	msgType, payload := c1.recv()
	require.Equal(t, uint32(MsgBluetoothLERawAdvertisementsResponse), msgType)

	var batch BluetoothLERawAdvertisementsResponse
	require.NoError(t, batch.Decode(payload))
	require.Len(t, batch.Advertisements, 1)
	assert.Equal(t, uint64(0xAABBCCDDEEFF), batch.Advertisements[0].Address)
	assert.Equal(t, int32(-71), batch.Advertisements[0].RSSI)

	// The Advertisements counter must reflect the forwarded batch.
	c1.send(MsgSubscribeStatesRequest, nil)
	msgType, payload = c1.recv()
	require.Equal(t, uint32(MsgSwitchStateResponse), msgType)
	msgType, payload = c1.recv()
	require.Equal(t, uint32(MsgSensorStateResponse), msgType)
	var cnt SensorStateResponse
	require.NoError(t, cnt.Decode(payload))
	assert.Equal(t, SensorKeyAdvertisements, cnt.Key)
	assert.Equal(t, float32(1), cnt.State)
	// SubscribeStates also reports the uptime text sensor; consume it so
	// it does not leak into the raw reads below.
	msgType, payload = c1.recv()
	require.Equal(t, uint32(MsgTextSensorStateResponse), msgType)
	var uptimeState TextSensorStateResponse
	require.NoError(t, uptimeState.Decode(payload))
	assert.Equal(t, TextSensorKeyUptime, uptimeState.Key)

	// The non-subscribed client must not receive anything.
	c2.conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	buf := make([]byte, 1024)
	n, err := c2.conn.Read(buf)
	assert.Error(t, err, "client 2 should not receive advertisements")
	assert.Zero(t, n)

	// Unsubscribe: no more advertisements. ScanControl(false) proves the
	// server processed the unsubscribe.
	c1.send(MsgUnsubscribeBluetoothLEAdvertisementsRequest, nil)
	select {
	case on := <-controlEvents:
		require.False(t, on)
	case <-time.After(2 * time.Second):
		t.Fatal("ScanControl not called on unsubscribe")
	}
	srv.SendAdvertisements(advs)
	c1.conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	n, err = c1.conn.Read(buf)
	assert.Error(t, err, "unsubscribed client should not receive advertisements")
	assert.Zero(t, n)
}

func TestScanControlOnLastUnsubscribe(t *testing.T) {
	controlEvents := make(chan bool, 8)
	srv, addr := startServer(t, Config{Name: "btproxy", MACAddress: "aa:bb:cc:dd:ee:ff"})
	srv.ScanControl = func(on bool) { controlEvents <- on }

	c := newTestClient(t, addr)
	handshake(t, c)
	c.send(MsgSubscribeBluetoothLEAdvertisementsRequest,
		(&SubscribeBluetoothLEAdvertisementsRequest{Flags: SubFlagRaw}).Encode())
	select {
	case on := <-controlEvents:
		require.True(t, on)
	case <-time.After(2 * time.Second):
		t.Fatal("ScanControl not called on subscribe")
	}

	// Closing the connection (client disappears) must stop scanning.
	c.conn.Close()
	select {
	case on := <-controlEvents:
		assert.False(t, on)
	case <-time.After(2 * time.Second):
		t.Fatal("ScanControl not called after last client disconnect")
	}
}

func TestTextSensorPeriodicPush(t *testing.T) {
	srv := NewServer(Config{Name: "btproxy", MACAddress: "aa:bb:cc:dd:ee:ff"}, testLogger(t))
	srv.statePushEvery = 50 * time.Millisecond
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(srv.Close)

	c := newTestClient(t, l.Addr())
	handshake(t, c)
	c.send(MsgSubscribeStatesRequest, nil)
	msgType, _ := c.recv()
	require.Equal(t, uint32(MsgSwitchStateResponse), msgType)

	// The initial state push and at least one periodic push (50 ms
	// interval) must arrive with non-empty uptime strings.
	seen := 0
	buf := make([]byte, 4096)
	deadline := time.Now().Add(3 * time.Second)
	for seen < 2 && time.Now().Before(deadline) {
		_ = c.conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		n, rerr := c.conn.Read(buf)
		if n > 0 {
			c.dec.Feed(buf[:n])
		}
		if rerr != nil && !os.IsTimeout(rerr) {
			t.Fatalf("read: %v", rerr)
		}
		for {
			loopType, loopPayload, ferr := c.dec.Next()
			if ferr != nil {
				break
			}
			if loopType != MsgTextSensorStateResponse {
				continue
			}
			var ts TextSensorStateResponse
			require.NoError(t, ts.Decode(loopPayload))
			assert.Equal(t, TextSensorKeyUptime, ts.Key)
			assert.NotEmpty(t, ts.State)
			seen++
		}
	}
	assert.GreaterOrEqual(t, seen, 2, "expected initial + periodic text sensor pushes")
}

// handshake performs the Hello/Connect part every test client needs.
func handshake(t *testing.T, c *testClient) {
	t.Helper()
	c.send(MsgHelloRequest, (&HelloRequest{Client: "test"}).Encode())
	msgType, _ := c.recv()
	require.Equal(t, uint32(MsgHelloResponse), msgType)
	c.send(MsgConnectRequest, nil)
	msgType, _ = c.recv()
	require.Equal(t, uint32(MsgConnectResponse), msgType)
}

func TestPasswordAuth(t *testing.T) {
	srv, addr := startServer(t, Config{
		Name:         "btproxy",
		MACAddress:   "aa:bb:cc:dd:ee:ff",
		BluetoothMAC: "aa:bb:cc:dd:ee:ff",
		Password:     "sekret",
	})
	_ = srv

	// Wrong password: ConnectResponse flags invalid_password.
	c := newTestClient(t, addr)
	c.send(MsgConnectRequest, (&ConnectRequest{Password: "wrong"}).Encode())
	msgType, payload := c.recv()
	require.Equal(t, uint32(MsgConnectResponse), msgType)
	var conn ConnectResponse
	require.NoError(t, conn.Decode(payload))
	assert.True(t, conn.InvalidPassword)

	// Hello stays available without authentication (HA sends it first).
	c.send(MsgHelloRequest, (&HelloRequest{Client: "test"}).Encode())
	msgType, _ = c.recv()
	require.Equal(t, uint32(MsgHelloResponse), msgType)

	// Anything beyond the handshake is refused: the server closes the
	// connection.
	c.send(MsgDeviceInfoRequest, nil)
	_, err := c.conn.Read(make([]byte, 64))
	assert.Error(t, err, "expected connection close on unauthenticated request")

	// Correct password: full handshake, DeviceInfo reports uses_password.
	c2 := newTestClient(t, addr)
	c2.send(MsgConnectRequest, (&ConnectRequest{Password: "sekret"}).Encode())
	msgType, payload = c2.recv()
	require.Equal(t, uint32(MsgConnectResponse), msgType)
	require.NoError(t, conn.Decode(payload))
	assert.False(t, conn.InvalidPassword)
	c2.send(MsgDeviceInfoRequest, nil)
	msgType, payload = c2.recv()
	require.Equal(t, uint32(MsgDeviceInfoResponse), msgType)
	var di DeviceInfoResponse
	require.NoError(t, di.Decode(payload))
	assert.True(t, di.UsesPassword)
}
