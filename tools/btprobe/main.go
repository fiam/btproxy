// Command btprobe is a development client for btproxy: it performs the
// full Home Assistant style handshake against an ESPHome native API server,
// optionally toggles the BLE Scanning switch, subscribes with the RAW flag,
// and prints decoded advertisement batches.
package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"time"

	"github.com/ankon/btproxy/internal/esphome"
	"github.com/ankon/btproxy/internal/frame"
)

func main() {
	var (
		addr     = flag.String("addr", "127.0.0.1:6053", "server address")
		switchTo = flag.String("switch", "", "toggle the BLE Scanning switch: on|off")
		duration = flag.Duration("duration", 0, "how long to receive advertisements (0: until Ctrl-C)")
		debug    = flag.Bool("debug", false, "dump raw frames")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	if *debug {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		}))
	}

	conn, err := net.DialTimeout("tcp", *addr, 5*time.Second)
	if err != nil {
		logger.Error("connect failed", "addr", *addr, "err", err)
		os.Exit(1)
	}
	defer conn.Close()
	logger.Info("connected", "addr", *addr)

	c := &client{conn: conn, dec: &frame.Decoder{}, log: logger, debug: *debug}

	// Hello
	c.send(esphome.MsgHelloRequest, (&esphome.HelloRequest{Client: "btprobe"}).Encode(),
		esphome.MsgHelloResponse)
	var hello esphome.HelloResponse
	c.decodeInto(esphome.MsgHelloResponse, &hello)
	fmt.Printf("HelloResponse: api v%d.%d server=%q name=%q\n",
		hello.APIVersionMajor, hello.APIVersionMinor, hello.ServerInfo, hello.Name)

	// Connect
	c.send(esphome.MsgConnectRequest, nil, esphome.MsgConnectResponse)
	var connect esphome.ConnectResponse
	c.decodeInto(esphome.MsgConnectResponse, &connect)
	fmt.Printf("ConnectResponse: invalid_password=%v\n", connect.InvalidPassword)

	// DeviceInfo
	c.send(esphome.MsgDeviceInfoRequest, nil, esphome.MsgDeviceInfoResponse)
	var di esphome.DeviceInfoResponse
	c.decodeInto(esphome.MsgDeviceInfoResponse, &di)
	fmt.Printf("DeviceInfo: name=%q mac=%s version=%s model=%q manufacturer=%q\n",
		di.Name, di.MACAddress, di.ESPHomeVersion, di.Model, di.Manufacturer)
	fmt.Printf("DeviceInfo: ble_proxy_feature_flags=0x%x bt_mac=%s\n",
		di.BluetoothProxyFeatureFlags, di.BluetoothMACAddress)

	// ListEntities
	c.send(esphome.MsgListEntitiesRequest, nil, esphome.MsgListEntitiesDoneResponse)
	for {
		msgType, payload := c.recv()
		if msgType == esphome.MsgListEntitiesDoneResponse {
			break
		}
		switch msgType {
		case esphome.MsgListEntitiesSwitchResponse:
			var sw esphome.ListEntitiesSwitchResponse
			decodeOrExit(payload, &sw)
			fmt.Printf("Entity: switch %q (key %d, name %q, icon %q)\n", sw.ObjectID, sw.Key, sw.Name, sw.Icon)
		case esphome.MsgListEntitiesSensorResponse:
			var se esphome.ListEntitiesSensorResponse
			decodeOrExit(payload, &se)
			fmt.Printf("Entity: sensor %q (key %d, name %q, unit %q)\n", se.ObjectID, se.Key, se.Name, se.UnitOfMeasurement)
		case esphome.MsgListEntitiesTextSensorResponse:
			var ts esphome.ListEntitiesTextSensorResponse
			decodeOrExit(payload, &ts)
			fmt.Printf("Entity: text_sensor %q (key %d, name %q, icon %q)\n", ts.ObjectID, ts.Key, ts.Name, ts.Icon)
		}
	}

	// SubscribeStates
	c.send(esphome.MsgSubscribeStatesRequest, nil, esphome.MsgSwitchStateResponse)
	msgType, payload := c.recv()
	if msgType == esphome.MsgSwitchStateResponse {
		var st esphome.SwitchStateResponse
		decodeOrExit(payload, &st)
		fmt.Printf("SwitchState: key=%d state=%v\n", st.Key, st.State)
	}

	// Optional switch command
	if *switchTo != "" {
		if *switchTo != "on" && *switchTo != "off" {
			logger.Error("invalid --switch value", "value", *switchTo)
			os.Exit(1)
		}
		cmd := esphome.SwitchCommandRequest{Key: esphome.SwitchKeyBLEScanning, State: *switchTo == "on"}
		c.send(esphome.MsgSwitchCommandRequest, (&cmd).Encode(), esphome.MsgSwitchStateResponse)
		var st esphome.SwitchStateResponse
		c.decodeInto(esphome.MsgSwitchStateResponse, &st)
		fmt.Printf("SwitchState after command: key=%d state=%v\n", st.Key, st.State)
	}

	// Subscribe with RAW flag
	sub := esphome.SubscribeBluetoothLEAdvertisementsRequest{Flags: esphome.SubFlagRaw}
	c.send(esphome.MsgSubscribeBluetoothLEAdvertisementsRequest, (&sub).Encode(), 0)
	fmt.Println("Subscribed with RAW flag; receiving advertisements...")

	deadline := time.Time{}
	if *duration > 0 {
		deadline = time.Now().Add(*duration)
		_ = conn.SetReadDeadline(deadline)
	}
	for {
		msgType, payload, err := c.readFrame()
		if err != nil {
			if os.IsTimeout(err) || (deadline != time.Time{} && time.Now().After(deadline)) {
				fmt.Println("Done.")
				return
			}
			logger.Error("read failed", "err", err)
			os.Exit(1)
		}
		switch msgType {
		case esphome.MsgBluetoothLERawAdvertisementsResponse:
			var batch esphome.BluetoothLERawAdvertisementsResponse
			decodeOrExit(payload, &batch)
			printAdvs(batch.Advertisements)
		case esphome.MsgSwitchStateResponse:
			var st esphome.SwitchStateResponse
			decodeOrExit(payload, &st)
			fmt.Printf("SwitchState: key=%d state=%v\n", st.Key, st.State)
		case esphome.MsgTextSensorStateResponse:
			var ts esphome.TextSensorStateResponse
			decodeOrExit(payload, &ts)
			fmt.Printf("TextSensorState: key=%d state=%q\n", ts.Key, ts.State)
		case esphome.MsgSensorStateResponse:
			var se esphome.SensorStateResponse
			decodeOrExit(payload, &se)
			fmt.Printf("SensorState: key=%d state=%v\n", se.Key, se.State)
		case esphome.MsgPingResponse:
		default:
			if *debug {
				fmt.Printf("message type %d: %x\n", msgType, payload)
			}
		}
	}
}

func printAdvs(advs []*esphome.BluetoothLERawAdvertisement) {
	for i, adv := range advs {
		fmt.Printf("adv %d: mac=%s rssi=%d type=%d data=%s\n",
			i+1, formatMAC(adv.Address), adv.RSSI, adv.AddressType, hex.EncodeToString(adv.Data))
	}
}

func formatMAC(packed uint64) string {
	return fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X",
		packed>>40&0xff, packed>>32&0xff, packed>>24&0xff,
		packed>>16&0xff, packed>>8&0xff, packed&0xff)
}

// decoder is satisfied by all generated message types.
type decoder interface {
	Decode(payload []byte) error
}

func decodeOrExit(payload []byte, m decoder) {
	if err := m.Decode(payload); err != nil {
		fmt.Fprintln(os.Stderr, "btprobe: protocol error:", err)
		os.Exit(1)
	}
}

type client struct {
	conn  net.Conn
	dec   *frame.Decoder
	log   *slog.Logger
	debug bool
}

func (c *client) send(msgType uint32, payload []byte, _ uint32) {
	if c.debug {
		c.log.Debug("send", "type", msgType, "payload", hex.EncodeToString(payload))
	}
	if _, err := c.conn.Write(frame.Encode(msgType, payload)); err != nil {
		c.log.Error("write failed", "err", err)
		os.Exit(1)
	}
}

func (c *client) readFrame() (uint32, []byte, error) {
	buf := make([]byte, 16*1024)
	for {
		msgType, payload, err := c.dec.Next()
		if err == nil {
			if c.debug {
				c.log.Debug("recv", "type", msgType, "payload", hex.EncodeToString(payload))
			}
			return msgType, payload, nil
		}
		if err != frame.ErrNeedMore {
			return 0, nil, err
		}
		n, rerr := c.conn.Read(buf)
		if rerr != nil {
			return 0, nil, rerr
		}
		c.dec.Feed(buf[:n])
	}
}

func (c *client) recv() (uint32, []byte) {
	msgType, payload, err := c.readFrame()
	if err != nil {
		c.log.Error("read failed", "err", err)
		os.Exit(1)
	}
	return msgType, payload
}

// decodeInto expects the next message to be of the given type and decodes
// its payload into m.
func (c *client) decodeInto(want uint32, m decoder) {
	msgType, payload := c.recv()
	if msgType != want {
		c.log.Error("unexpected message", "want", want, "got", msgType)
		os.Exit(1)
	}
	decodeOrExit(payload, m)
}
