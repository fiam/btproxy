package bluez

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/godbus/dbus/v5"
)

// Adv is one re-encoded advertisement produced by the scanner.
type Adv struct {
	Address     string // "AA:BB:CC:DD:EE:FF"
	AddressType uint32 // 0 public, 1 random
	RSSI        int32
	Data        []byte // AD structures, max 62 bytes
}

// Scanner watches BlueZ Device1 properties and turns them into Adv events.
// It talks to org.bluez through the system bus; bluetoothd keeps running
// normally.
type Scanner struct {
	RequireLiveServiceData bool // scale mode: never replay cached service payloads
	log                    *slog.Logger
	conn                   *dbus.Conn
	adapterPath            dbus.ObjectPath

	adapterAddress string

	advs chan Adv

	mu      sync.Mutex
	devices map[string]*deviceProps // by MAC address
	running bool
	cancel  func()
}

// well-known BlueZ names.
const (
	bluezName      = "org.bluez"
	adapterIface   = "org.bluez.Adapter1"
	deviceIface    = "org.bluez.Device1"
	propsIface     = "org.freedesktop.DBus.Properties"
	objectMgrIface = "org.freedesktop.DBus.ObjectManager"
)

// New connects to the system bus and resolves the named adapter (e.g.
// "hci0"). Call Close when done.
func New(logger *slog.Logger, adapter string) (*Scanner, error) {
	if adapter == "" {
		adapter = "hci0"
	}
	if logger == nil {
		logger = slog.Default()
	}
	conn, err := dbus.SystemBus()
	if err != nil {
		return nil, fmt.Errorf("bluez: connect to system bus: %w", err)
	}
	s := &Scanner{
		log:     logger,
		conn:    conn,
		advs:    make(chan Adv, 256),
		devices: make(map[string]*deviceProps),
	}
	if err := s.resolveAdapter(adapter); err != nil {
		conn.Close()
		return nil, err
	}
	return s, nil
}

// resolveAdapter finds the adapter via the ObjectManager and reads its MAC.
func (s *Scanner) resolveAdapter(name string) error {
	om := s.conn.Object(bluezName, "/")
	var objects map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	if err := om.Call(objectMgrIface+".GetManagedObjects", 0).Store(&objects); err != nil {
		return fmt.Errorf("bluez: GetManagedObjects: %w", err)
	}
	want := dbus.ObjectPath("/org/bluez/" + name)
	for path, ifaces := range objects {
		props, ok := ifaces[adapterIface]
		if !ok || path != want {
			continue
		}
		s.adapterPath = path
		s.adapterAddress = stringOf(props["Address"])
		return nil
	}
	return fmt.Errorf("bluez: adapter %q not found", name)
}

// AdapterAddress returns the adapter's MAC address ("AA:BB:CC:DD:EE:FF").
func (s *Scanner) AdapterAddress() string { return s.adapterAddress }

// Advertisements returns the scanner event channel.
func (s *Scanner) Advertisements() <-chan Adv { return s.advs }

// Start begins BLE discovery with Transport=le and DuplicateData=true, and
// processes BlueZ signals until ctx is cancelled or Stop is called. It is
// safe to call again after Stop (BlueZ discovery is reference-counted by
// BlueZ itself; we simply restart it).
func (s *Scanner) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	adapter := s.conn.Object(bluezName, s.adapterPath)
	filter := map[string]dbus.Variant{
		"Transport":     dbus.MakeVariant("le"),
		"DuplicateData": dbus.MakeVariant(true),
	}
	if err := adapter.Call(adapterIface+".SetDiscoveryFilter", 0, filter).Err; err != nil {
		return fmt.Errorf("bluez: SetDiscoveryFilter: %w", err)
	}
	if err := adapter.Call(adapterIface+".StartDiscovery", 0).Err; err != nil {
		// Already discovering is fine (e.g. bluetoothctl scan on).
		if !isAlreadyDiscovering(err) {
			return fmt.Errorf("bluez: StartDiscovery: %w", err)
		}
	}

	if err := s.addSignalMatches(); err != nil {
		_ = adapter.Call(adapterIface+".StopDiscovery", 0)
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	signals := make(chan *dbus.Signal, 512)
	s.conn.Signal(signals)

	s.mu.Lock()
	s.running = true
	s.cancel = func() {
		cancel()
		s.conn.RemoveSignal(signals)
	}
	s.mu.Unlock()

	go s.dispatch(ctx, signals)

	// Seed the snapshot cache with devices BlueZ already knows about:
	// their parsed properties (name, manufacturer/service data, UUIDs)
	// only reach us via signals when they change, so without seeding,
	// cached metadata remains useful. Never replay cached service data as a new reading.
	s.seedDevices()

	s.log.Info("bluetooth discovery started", "adapter", s.adapterPath)
	return nil
}

// seedDevices populates the snapshot cache from the BlueZ ObjectManager and
// never emits cached readings. Only live ServiceData signals can be forwarded.
func (s *Scanner) seedDevices() {
	om := s.conn.Object(bluezName, "/")
	var objects map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	if err := om.Call(objectMgrIface+".GetManagedObjects", 0).Store(&objects); err != nil {
		s.log.Warn("bluez: GetManagedObjects failed", "err", err)
		return
	}
	n := 0
	for path, ifaces := range objects {
		props, ok := ifaces[deviceIface]
		if !ok || !s.underAdapter(path) {
			continue
		}
		p := &deviceProps{}
		applyProps(p, props)
		if p.Address == "" {
			p.Address = pathToAddress(string(path))
		}
		if p.Address == "" {
			continue
		}
		s.mu.Lock()
		s.devices[pathToMAC(string(path))] = p
		s.mu.Unlock()
		if !s.RequireLiveServiceData {
			s.emit(p, encodeAD(p))
		}
		n++
	}
	s.log.Info("seeded device cache from BlueZ", "devices", n)
}

func isAlreadyDiscovering(err error) bool {
	var dbusErr dbus.Error
	if ok := asDBusError(err, &dbusErr); ok {
		return dbusErr.Name == "org.bluez.Error.AlreadyDiscovering" ||
			dbusErr.Name == "org.bluez.Error.InProgress"
	}
	return false
}

func asDBusError(err error, target *dbus.Error) bool {
	if e, ok := err.(dbus.Error); ok {
		*target = e
		return true
	}
	return false
}

func (s *Scanner) addSignalMatches() error {
	if err := s.conn.AddMatchSignal(
		dbus.WithMatchSender(bluezName),
		dbus.WithMatchInterface(propsIface),
	); err != nil {
		return fmt.Errorf("bluez: add Properties match: %w", err)
	}
	if err := s.conn.AddMatchSignal(
		dbus.WithMatchSender(bluezName),
		dbus.WithMatchInterface(objectMgrIface),
	); err != nil {
		return fmt.Errorf("bluez: add ObjectManager match: %w", err)
	}
	return nil
}

// Stop stops discovery and the signal loop. Safe to call multiple times.
func (s *Scanner) Stop() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	s.running = false
	cancel := s.cancel
	s.mu.Unlock()

	cancel()
	adapter := s.conn.Object(bluezName, s.adapterPath)
	if err := adapter.Call(adapterIface+".StopDiscovery", 0).Err; err != nil {
		s.log.Debug("StopDiscovery error", "err", err)
	}
	s.log.Info("bluetooth discovery stopped", "adapter", s.adapterPath)
}

// Close releases the D-Bus connection.
func (s *Scanner) Close() {
	s.Stop()
	s.conn.Close()
}

// dispatch consumes BlueZ signals until ctx is done.
func (s *Scanner) dispatch(ctx context.Context, signals chan *dbus.Signal) {
	for {
		select {
		case <-ctx.Done():
			return
		case sig := <-signals:
			s.handleSignal(sig)
		}
	}
}

func (s *Scanner) handleSignal(sig *dbus.Signal) {
	switch sig.Name {
	case propsIface + ".PropertiesChanged":
		if len(sig.Body) < 2 {
			return
		}
		iface, _ := sig.Body[0].(string)
		switch iface {
		case deviceIface:
			if !s.underAdapter(sig.Path) {
				return
			}
			changed, _ := sig.Body[1].(map[string]dbus.Variant)
			if len(changed) == 0 {
				return
			}
			s.log.Debug("device properties changed", "path", sig.Path, "fields", sortedKeys(changed))
			s.updateDevice(string(sig.Path), changed)

		case adapterIface:
			// Someone (e.g. bluetoothctl scan off) stopped our discovery.
			if sig.Path != s.adapterPath {
				return
			}
			changed, _ := sig.Body[1].(map[string]dbus.Variant)
			if v, ok := changed["Discovering"]; ok && v.Value() == false {
				s.restartDiscovery()
			}
		}

	case objectMgrIface + ".InterfacesAdded":
		if len(sig.Body) < 2 {
			return
		}
		ifaces, ok := sig.Body[1].(map[string]map[string]dbus.Variant)
		if !ok {
			return
		}
		props, ok := ifaces[deviceIface]
		if !ok {
			return
		}
		path, ok := sig.Body[0].(dbus.ObjectPath)
		if ok && s.underAdapter(path) {
			s.addDevice(string(path), props)
		}

	case objectMgrIface + ".InterfacesRemoved":
		if len(sig.Body) < 2 {
			return
		}
		path, ok := sig.Body[0].(dbus.ObjectPath)
		ifaces, _ := sig.Body[1].([]string)
		if ok && s.underAdapter(path) {
			for _, iface := range ifaces {
				if iface == deviceIface {
					s.removeDevice(string(path))
				}
			}
		}
	}
}

// restartDiscovery re-arms BlueZ discovery after an external stop, with a
// short backoff to avoid a fight with a user's bluetoothctl session.
func (s *Scanner) restartDiscovery() {
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	if !running {
		return
	}
	s.log.Warn("discovery stopped externally; restarting")
	adapter := s.conn.Object(bluezName, s.adapterPath)
	if err := adapter.Call(adapterIface+".StartDiscovery", 0).Err; err != nil {
		s.log.Error("restart discovery failed", "err", err)
	}
}

// underAdapter reports whether the object path belongs to this adapter.
func (s *Scanner) underAdapter(path dbus.ObjectPath) bool {
	return strings.HasPrefix(string(path), string(s.adapterPath)+"/dev_")
}

// updateDevice merges changed Device1 properties into the per-MAC snapshot
// and emits an advertisement when the resulting view differs from the
// previous one.
func (s *Scanner) updateDevice(path string, changed map[string]dbus.Variant) {
	s.mu.Lock()
	mac, p := s.snapshotLocked(path)
	if p == nil {
		// Not tracked yet. Prefer the Address property; fall back to the
		// MAC embedded in the BlueZ object path (covers devices discovered
		// before we started, which only emit PropertiesChanged).
		addr := stringOf(changed["Address"])
		if addr == "" {
			addr = pathToAddress(path)
		}
		if addr == "" {
			s.mu.Unlock()
			return
		}
		p = &deviceProps{Address: addr}
		s.devices[mac] = p
	}
	p = p.clone()
	applyProps(p, changed)

	// Skip devices without a signal; they carry no useful advertisement.
	if p.RSSI == 0 && len(p.ManufacturerData) == 0 && len(p.ServiceData) == 0 &&
		len(p.ServiceUUIDs) == 0 && p.Name == "" {
		s.mu.Unlock()
		return
	}

	data := encodeAD(p)
	prev, tracked := s.devices[mac]
	changedView := !tracked || !equalProps(prev, p) || prev.RSSI != p.RSSI
	if changedView {
		s.devices[mac] = p
	}
	s.mu.Unlock()

	if _, live := changed["ServiceData"]; (s.RequireLiveServiceData && live) || (!s.RequireLiveServiceData && changedView) {
		s.emit(p, data)
	}
}

// addDevice registers a newly seen device from an InterfacesAdded signal.
func (s *Scanner) addDevice(path string, props map[string]dbus.Variant) {
	s.mu.Lock()
	mac, p := s.snapshotLocked(path)
	if p == nil {
		addr := stringOf(props["Address"])
		if addr == "" {
			s.mu.Unlock()
			return
		}
		p = &deviceProps{Address: addr}
		s.devices[mac] = p
	}
	p = p.clone()
	applyProps(p, props)
	s.devices[mac] = p
	s.mu.Unlock()

	if _, live := props["ServiceData"]; !s.RequireLiveServiceData || live {
		s.emit(p, encodeAD(p))
	}
}

func (s *Scanner) removeDevice(path string) {
	mac := pathToMAC(path)
	s.mu.Lock()
	delete(s.devices, mac)
	s.mu.Unlock()
}

// snapshotLocked returns the tracked snapshot for a device path, keyed by
// the Address property when known and by path otherwise.
func (s *Scanner) snapshotLocked(path string) (string, *deviceProps) {
	mac := pathToMAC(path)
	return mac, s.devices[mac]
}

func (s *Scanner) emit(p *deviceProps, data []byte) {
	adv := Adv{
		Address:     p.Address,
		AddressType: p.AddressType,
		RSSI:        p.RSSI,
		Data:        data,
	}
	select {
	case s.advs <- adv:
	default:
		// Consumer too slow: drop rather than block discovery processing.
		s.log.Warn("advertisement dropped (channel full)")
	}
}

// pathToMAC extracts the BlueZ path key ("/org/bluez/hci0/dev_AA_BB_..." or
// the stable "dev_XX_..." form).
func pathToMAC(path string) string {
	i := strings.LastIndex(path, "/dev_")
	if i < 0 {
		return path
	}
	return path[i+len("/dev_"):]
}

// pathToAddress derives a "AA:BB:CC:DD:EE:FF" address from a BlueZ device
// path. Returns "" for paths without a parseable MAC.
func pathToAddress(path string) string {
	key := pathToMAC(path)
	parts := strings.Split(key, "_")
	if len(parts) != 6 {
		return ""
	}
	for _, p := range parts {
		if len(p) != 2 || hexVal(p[0]) < 0 || hexVal(p[1]) < 0 {
			return ""
		}
	}
	return strings.Join(parts, ":")
}

// applyProps overwrites snapshot fields present in the variant map.
func applyProps(p *deviceProps, props map[string]dbus.Variant) {
	if v, ok := props["Address"]; ok {
		if s := stringOf(v); s != "" {
			p.Address = s
		}
	}
	if v, ok := props["AddressType"]; ok {
		switch stringOf(v) {
		case "random":
			p.AddressType = 1
		default:
			p.AddressType = 0
		}
	}
	if v, ok := props["RSSI"]; ok {
		p.RSSI = int32(int16Of(v))
	}
	if v, ok := props["Name"]; ok {
		p.Name = stringOf(v)
	}
	if v, ok := props["TxPower"]; ok {
		p.TxPower = int16Of(v)
		p.HasTxPower = true
	}
	if v, ok := props["ManufacturerData"]; ok {
		p.ManufacturerData = manufacturerDataOf(v)
	}
	if v, ok := props["ServiceData"]; ok {
		p.ServiceData = serviceDataOf(v)
	}
	if v, ok := props["UUIDs"]; ok {
		p.ServiceUUIDs = serviceUUIDsOf(v)
	}
}

func equalProps(a, b *deviceProps) bool {
	if a.Address != b.Address || a.AddressType != b.AddressType ||
		a.Name != b.Name || a.TxPower != b.TxPower || a.HasTxPower != b.HasTxPower {
		return false
	}
	if len(a.ManufacturerData) != len(b.ManufacturerData) ||
		len(a.ServiceData) != len(b.ServiceData) ||
		len(a.ServiceUUIDs) != len(b.ServiceUUIDs) {
		return false
	}
	for k, v := range a.ManufacturerData {
		if string(b.ManufacturerData[k]) != string(v) {
			return false
		}
	}
	for k, v := range a.ServiceData {
		if string(b.ServiceData[k]) != string(v) {
			return false
		}
	}
	for i, v := range a.ServiceUUIDs {
		if b.ServiceUUIDs[i] != v {
			return false
		}
	}
	return true
}

// --- dbus.Variant accessors ---------------------------------------------

func stringOf(v dbus.Variant) string {
	s, _ := v.Value().(string)
	return s
}

func int16Of(v dbus.Variant) int16 {
	switch x := v.Value().(type) {
	case int16:
		return x
	case int32:
		return int16(x)
	case int64:
		return int16(x)
	case uint16:
		return int16(x)
	default:
		return 0
	}
}

// manufacturerDataOf extracts a{qv} → map[uint16][]byte.
func manufacturerDataOf(v dbus.Variant) map[uint16][]byte {
	out := make(map[uint16][]byte)
	switch m := v.Value().(type) {
	case map[uint16][]byte:
		for k, d := range m {
			out[k] = d
		}
	case map[uint16]dbus.Variant:
		for k, vv := range m {
			if d, ok := vv.Value().([]byte); ok {
				out[k] = d
			}
		}
	default:
		if v.Value() != nil {
			// Unexpected shape; log nothing here, callers log on use.
			return out
		}
	}
	return out
}

// serviceDataOf extracts a{sv} → map[uuid][]byte.
func serviceDataOf(v dbus.Variant) map[string][]byte {
	out := make(map[string][]byte)
	if m, ok := v.Value().(map[string]dbus.Variant); ok {
		for k, vv := range m {
			if d, ok := vv.Value().([]byte); ok {
				out[k] = d
			}
		}
	}
	return out
}

// serviceUUIDsOf extracts as → []string.
func serviceUUIDsOf(v dbus.Variant) []string {
	uuids, ok := v.Value().([]string)
	if !ok {
		return nil
	}
	sort.Strings(uuids)
	return uuids
}
