// Package esphome implements the server side of the ESPHome native API:
// a TCP listener speaking the plaintext protobuf framing, per-client
// sessions, and the message subset btproxy serves to Home Assistant.
// Message structs and codecs are generated from api.pidl (see api_gen.go).
package esphome

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/ankon/btproxy/internal/frame"
)

// Config holds the identity values reported to Home Assistant.
type Config struct {
	Name            string // device name (mDNS name, DeviceInfo.name)
	MACAddress      string // primary NIC MAC
	BluetoothMAC    string // adapter MAC (DeviceInfo field 18)
	ESPHomeVersion  string // string HA displays
	CompilationTime string
	Model           string
	Manufacturer    string
	FriendlyName    string
	SuggestedArea   string
	Port            int
}

// Server serves ESPHome native API clients over TCP.
type Server struct {
	cfg Config
	log *slog.Logger

	mu              sync.Mutex
	sessions        map[*session]struct{}
	scanning        bool // BLE Scanning switch state
	scanningRunning bool // last state pushed to ScanControl
	advsForwarded   uint64
	startedAt       time.Time
	textSensors     []textSensor
	sensors         []numericSensor
	statePushEvery  time.Duration // text/sensor state refresh interval

	// ScanControl is invoked whenever the desired scan state changes:
	// the switch is on and at least one client subscribes with the RAW
	// flag. May be nil. Called with no server locks held.
	ScanControl func(on bool)

	listener  net.Listener
	closed    chan struct{}
	closeOnce sync.Once
}

// NewServer creates a Server (call Serve to start accepting).
func NewServer(cfg Config, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	if cfg.Name == "" {
		cfg.Name = "btproxy"
	}
	if cfg.ESPHomeVersion == "" {
		cfg.ESPHomeVersion = "2026.10.0"
	}
	if cfg.Model == "" {
		cfg.Model = "btproxy"
	}
	if cfg.Manufacturer == "" {
		cfg.Manufacturer = "btproxy"
	}
	if cfg.FriendlyName == "" {
		cfg.FriendlyName = cfg.Name
	}
	if cfg.CompilationTime == "" {
		cfg.CompilationTime = time.Now().Format(time.RFC3339)
	}
	s := &Server{
		cfg:            cfg,
		log:            log,
		sessions:       make(map[*session]struct{}),
		scanning:       true, // BLE Scanning switch enabled by default, as in the C impl
		startedAt:      time.Now(),
		statePushEvery: time.Minute,
		closed:         make(chan struct{}),
	}
	s.textSensors = s.defaultTextSensors()
	s.sensors = s.defaultSensors()
	return s
}

// Addr returns the listener address once Serve is running.
func (s *Server) Addr() (net.Addr, error) {
	if s.listener == nil {
		return nil, errors.New("esphome: server not started")
	}
	return s.listener.Addr(), nil
}

// Serve accepts connections until Close is called.
func (s *Server) Serve(l net.Listener) error {
	s.listener = l
	go s.pushPeriodicStates()
	for {
		conn, err := l.Accept()
		if err != nil {
			select {
			case <-s.closed:
				return nil
			default:
				return fmt.Errorf("esphome: accept: %w", err)
			}
		}
		go s.handleConn(conn)
	}
}

// Close stops the listener and all client sessions.
func (s *Server) Close() {
	s.closeOnce.Do(func() {
		close(s.closed)
		if s.listener != nil {
			s.listener.Close()
		}
	})
	s.mu.Lock()
	for sess := range s.sessions {
		sess.close()
	}
	s.mu.Unlock()
}

// Scanning reports the BLE Scanning switch state.
func (s *Server) Scanning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scanning
}

// SetScanning sets the BLE Scanning switch state as if commanded from HA.
func (s *Server) SetScanning(on bool) {
	s.mu.Lock()
	s.scanning = on
	s.mu.Unlock()
	s.updateScanControl()
}

// SendAdvertisements broadcasts a batch of raw advertisements (type 93) to
// every client subscribed with the RAW flag.
func (s *Server) SendAdvertisements(advs []*BluetoothLERawAdvertisement) {
	if len(advs) == 0 {
		return
	}
	batch := BluetoothLERawAdvertisementsResponse{Advertisements: advs}
	payload := batch.Encode()

	s.mu.Lock()
	s.advsForwarded += uint64(len(advs))
	var failed []*session
	for sess := range s.sessions {
		if !sess.subscribed {
			continue
		}
		if err := sess.write(MsgBluetoothLERawAdvertisementsResponse, payload); err != nil {
			failed = append(failed, sess)
		}
	}
	for _, sess := range failed {
		delete(s.sessions, sess)
		sess.close()
	}
	s.mu.Unlock()
}

// updateScanControl recomputes the desired scan state (switch on + at least
// one RAW subscriber) and invokes ScanControl on changes.
func (s *Server) updateScanControl() {
	s.mu.Lock()
	want := s.scanning && s.hasRawSubscribersLocked()
	changed := want != s.scanningRunning
	if changed {
		s.scanningRunning = want
	}
	cb := s.ScanControl
	s.mu.Unlock()

	if changed && cb != nil {
		cb(want)
	}
}

func (s *Server) hasRawSubscribersLocked() bool {
	for sess := range s.sessions {
		if sess.subscribed {
			return true
		}
	}
	return false
}

// pushPeriodicStates broadcasts text sensor and sensor states (uptime,
// advertisement counter) to all connected clients every statePushEvery
// until the server is closed.
func (s *Server) pushPeriodicStates() {
	interval := s.statePushEvery
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-s.closed:
			return
		case <-t.C:
			for i := range s.textSensors {
				resp := s.textSensors[i].stateResponse()
				s.broadcastTextSensorState(resp)
			}
			for i := range s.sensors {
				resp := s.sensors[i].stateResponse()
				s.broadcastSensorState(resp)
			}
		}
	}
}

func (s *Server) broadcastTextSensorState(st TextSensorStateResponse) {
	payload := st.Encode()
	s.mu.Lock()
	var failed []*session
	for sess := range s.sessions {
		if err := sess.write(MsgTextSensorStateResponse, payload); err != nil {
			failed = append(failed, sess)
		}
	}
	for _, sess := range failed {
		delete(s.sessions, sess)
		sess.close()
	}
	s.mu.Unlock()
}

func (s *Server) broadcastSensorState(st SensorStateResponse) {
	payload := st.Encode()
	s.mu.Lock()
	var failed []*session
	for sess := range s.sessions {
		if err := sess.write(MsgSensorStateResponse, payload); err != nil {
			failed = append(failed, sess)
		}
	}
	for _, sess := range failed {
		delete(s.sessions, sess)
		sess.close()
	}
	s.mu.Unlock()
}

func (s *Server) addSession(sess *session) {
	s.mu.Lock()
	s.sessions[sess] = struct{}{}
	s.mu.Unlock()
}

func (s *Server) removeSession(sess *session) {
	s.mu.Lock()
	if _, ok := s.sessions[sess]; ok {
		delete(s.sessions, sess)
		s.mu.Unlock()
		s.updateScanControl()
		return
	}
	s.mu.Unlock()
}

// session is one client connection.
type session struct {
	srv  *Server
	conn net.Conn
	dec  *frame.Decoder
	wmu  sync.Mutex

	subscribed bool // subscribed with RAW flag
}

func (s *Server) handleConn(conn net.Conn) {
	sess := &session{srv: s, conn: conn, dec: &frame.Decoder{}}
	defer func() {
		s.removeSession(sess)
		conn.Close()
	}()
	s.addSession(sess)

	s.log.Info("client connected", "remote", conn.RemoteAddr())
	buf := make([]byte, 32*1024)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			sess.dec.Feed(buf[:n])
			for {
				msgType, payload, err := sess.dec.Next()
				if errors.Is(err, frame.ErrNeedMore) {
					break
				}
				if err != nil {
					s.log.Error("framing error", "remote", conn.RemoteAddr(), "err", err)
					return
				}
				if done := sess.handleMessage(msgType, payload); done {
					return
				}
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.log.Error("read error", "remote", conn.RemoteAddr(), "err", err)
			}
			return
		}
	}
}

// handleMessage dispatches one framed message; it returns true when the
// connection should be closed.
func (sess *session) handleMessage(msgType uint32, payload []byte) bool {
	s := sess.srv
	switch msgType {
	case MsgHelloRequest:
		hello := HelloResponse{
			APIVersionMajor: APIVersionMajor,
			APIVersionMinor: APIVersionMinor,
			ServerInfo:      fmt.Sprintf("%s (btproxy %s)", s.cfg.Name, s.cfg.ESPHomeVersion),
			Name:            s.cfg.Name,
		}
		return sess.writeErr(MsgHelloResponse, hello.Encode())

	case MsgConnectRequest:
		// No password support: same trust model as the C reference.
		resp := ConnectResponse{}
		return sess.writeErr(MsgConnectResponse, resp.Encode())

	case MsgDisconnectRequest:
		s.log.Info("client disconnecting", "remote", sess.conn.RemoteAddr())
		return true

	case MsgPingRequest:
		return sess.writeErr(MsgPingResponse, nil)

	case MsgDeviceInfoRequest:
		di := DeviceInfoResponse{
			Name:                       s.cfg.Name,
			MACAddress:                 s.cfg.MACAddress,
			BluetoothMACAddress:        s.cfg.BluetoothMAC,
			ESPHomeVersion:             s.cfg.ESPHomeVersion,
			CompilationTime:            s.cfg.CompilationTime,
			Model:                      s.cfg.Model,
			Manufacturer:               s.cfg.Manufacturer,
			FriendlyName:               s.cfg.FriendlyName,
			SuggestedArea:              s.cfg.SuggestedArea,
			BluetoothProxyFeatureFlags: DefaultFeatureFlags,
		}
		return sess.writeErr(MsgDeviceInfoResponse, di.Encode())

	case MsgListEntitiesRequest:
		sw := ListEntitiesSwitchResponse{
			ObjectID: "ble_scanning",
			Key:      SwitchKeyBLEScanning,
			Name:     "BLE Scanning",
			Icon:     "mdi:bluetooth-audio",
		}
		if err := sess.write(MsgListEntitiesSwitchResponse, sw.Encode()); err != nil {
			sess.logWriteErr(err)
			return true
		}
		for i := range s.sensors {
			resp := s.sensors[i].listResponse()
			if err := sess.write(MsgListEntitiesSensorResponse, resp.Encode()); err != nil {
				sess.logWriteErr(err)
				return true
			}
		}
		for i := range s.textSensors {
			resp := s.textSensors[i].listResponse()
			if err := sess.write(MsgListEntitiesTextSensorResponse, resp.Encode()); err != nil {
				sess.logWriteErr(err)
				return true
			}
		}
		return sess.writeErr(MsgListEntitiesDoneResponse, nil)

	case MsgSubscribeStatesRequest:
		s.mu.Lock()
		state := s.scanning
		s.mu.Unlock()
		st := SwitchStateResponse{Key: SwitchKeyBLEScanning, State: state}
		if err := sess.write(MsgSwitchStateResponse, st.Encode()); err != nil {
			sess.logWriteErr(err)
			return true
		}
		for i := range s.sensors {
			resp := s.sensors[i].stateResponse()
			if err := sess.write(MsgSensorStateResponse, resp.Encode()); err != nil {
				sess.logWriteErr(err)
				return true
			}
		}
		for i := range s.textSensors {
			resp := s.textSensors[i].stateResponse()
			if err := sess.write(MsgTextSensorStateResponse, resp.Encode()); err != nil {
				sess.logWriteErr(err)
				return true
			}
		}
		return false

	case MsgSwitchCommandRequest:
		var cmd SwitchCommandRequest
		if err := cmd.Decode(payload); err != nil {
			s.log.Error("bad switch command", "remote", sess.conn.RemoteAddr(), "err", err)
			return true
		}
		if cmd.Key != SwitchKeyBLEScanning {
			s.log.Warn("switch command for unknown key", "key", cmd.Key)
			return false
		}
		s.SetScanning(cmd.State)
		// Reply to the commanding client; other clients see the new state
		// via the broadcast below.
		st := SwitchStateResponse{Key: cmd.Key, State: cmd.State}
		if err := sess.write(MsgSwitchStateResponse, st.Encode()); err != nil {
			sess.logWriteErr(err)
			return true
		}
		s.broadcastSwitchState(st, map[*session]bool{sess: true})
		return false

	case MsgGetTimeRequest:
		resp := GetTimeResponse{EpochSeconds: uint32(time.Now().Unix())}
		return sess.writeErr(MsgGetTimeResponse, resp.Encode())

	case MsgSubscribeBluetoothLEAdvertisementsRequest:
		var sub SubscribeBluetoothLEAdvertisementsRequest
		if err := sub.Decode(payload); err != nil {
			s.log.Error("bad subscribe-ble request", "remote", sess.conn.RemoteAddr(), "err", err)
			return true
		}
		if sub.Flags&SubFlagRaw == 0 {
			s.log.Warn("client subscribes without RAW flag; ignoring", "remote", sess.conn.RemoteAddr())
			return false
		}
		s.mu.Lock()
		sess.subscribed = true
		s.mu.Unlock()
		s.updateScanControl()
		return false

	case MsgUnsubscribeBluetoothLEAdvertisementsRequest:
		s.mu.Lock()
		sess.subscribed = false
		s.mu.Unlock()
		s.updateScanControl()
		return false

	default:
		s.log.Debug("ignoring message", "type", msgType, "bytes", len(payload), "remote", sess.conn.RemoteAddr())
		return false
	}
}

// broadcastSwitchState sends the switch state to every connected client in
// except.
func (s *Server) broadcastSwitchState(st SwitchStateResponse, except map[*session]bool) {
	payload := st.Encode()
	s.mu.Lock()
	var failed []*session
	for sess := range s.sessions {
		if except[sess] {
			continue
		}
		if err := sess.write(MsgSwitchStateResponse, payload); err != nil {
			failed = append(failed, sess)
		}
	}
	for _, sess := range failed {
		delete(s.sessions, sess)
		sess.close()
	}
	s.mu.Unlock()
}

func (sess *session) write(msgType uint32, payload []byte) error {
	frameBytes := frame.Encode(msgType, payload)
	sess.wmu.Lock()
	defer sess.wmu.Unlock()
	// A stalled client must not block advertisement broadcasts to others.
	_ = sess.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := sess.conn.Write(frameBytes)
	return err
}

func (sess *session) writeErr(msgType uint32, payload []byte) bool {
	err := sess.write(msgType, payload)
	if err != nil {
		sess.logWriteErr(err)
	}
	return err != nil
}

func (sess *session) logWriteErr(err error) {
	sess.srv.log.Error("write error", "remote", sess.conn.RemoteAddr(), "err", err)
}

func (sess *session) close() {
	sess.conn.Close()
}
