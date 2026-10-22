// Package mdns announces the ESPHome API service via the avahi daemon's
// D-Bus API (org.freedesktop.Avahi.Server), so Home Assistant can discover
// btproxy as a native ESPHome device. Requires a running avahi-daemon.
package mdns

import (
	"fmt"
	"log/slog"

	"github.com/godbus/dbus/v5"
)

const (
	avahiName    = "org.freedesktop.Avahi"
	serverIface  = "org.freedesktop.Avahi.Server"
	serverPath   = dbus.ObjectPath("/")
	groupIface   = "org.freedesktop.Avahi.EntryGroup"
	serviceType  = "_esphomelib._tcp"
	unspecIfiace = int32(-1) // all interfaces
	unspecProto  = int32(-1) // IPv4 + IPv6
)

// Entry-group states (avahi-common/defs.h, AvahiEntryGroupState).
const (
	stateUncommited  = 0
	stateRegistering = 1
	stateEstablished = 2
	stateCollision   = 3
	stateFailure     = 4
)

// Publisher holds one avahi entry group for the announced service.
type Publisher struct {
	log   *slog.Logger
	conn  *dbus.Conn
	group dbus.BusObject

	signals chan *dbus.Signal
	stop    chan struct{}
}

// Publish announces name on _esphomelib._tcp:port with the given TXT
// records (e.g. "version=2026.10.0", "mac=aa:bb:cc:dd:ee:ff"). The service
// stays announced until Unpublish is called.
func Publish(logger *slog.Logger, name string, port int, txt []string) (*Publisher, error) {
	if logger == nil {
		logger = slog.Default()
	}
	conn, err := dbus.SystemBus()
	if err != nil {
		return nil, fmt.Errorf("mdns: connect system bus: %w", err)
	}

	// Fail early with a clear message when avahi is not running.
	avahi := conn.Object(avahiName, serverPath)
	if err := avahi.Call(serverIface+".GetVersionString", 0).Err; err != nil {
		conn.Close()
		return nil, fmt.Errorf("mdns: avahi not reachable (is avahi-daemon running?): %w", err)
	}

	var groupPath dbus.ObjectPath
	if err := avahi.Call(serverIface+".EntryGroupNew", 0).Store(&groupPath); err != nil {
		conn.Close()
		return nil, fmt.Errorf("mdns: EntryGroupNew: %w", err)
	}
	group := conn.Object(avahiName, groupPath)

	txtBytes := make([][]byte, 0, len(txt))
	for _, t := range txt {
		txtBytes = append(txtBytes, []byte(t))
	}

	if err := group.Call(groupIface+".AddService", 0,
		unspecIfiace, unspecProto, uint32(0),
		name, serviceType, "", "", uint16(port), txtBytes).Err; err != nil {
		conn.Close()
		return nil, fmt.Errorf("mdns: AddService: %w", err)
	}
	if err := group.Call(groupIface+".Commit", 0).Err; err != nil {
		conn.Close()
		return nil, fmt.Errorf("mdns: Commit: %w", err)
	}

	p := &Publisher{
		log:     logger,
		conn:    conn,
		group:   group,
		signals: make(chan *dbus.Signal, 8),
		stop:    make(chan struct{}),
	}

	// Watch entry-group state: collisions and failures are logged; the
	// service keeps its announced name unless avahi renames it.
	if err := conn.AddMatchSignal(
		dbus.WithMatchObjectPath(groupPath),
		dbus.WithMatchInterface(groupIface),
	); err != nil {
		p.Unpublish()
		return nil, fmt.Errorf("mdns: add match: %w", err)
	}
	conn.Signal(p.signals)
	go p.watchState()

	logger.Info("announced via avahi", "name", name, "type", serviceType, "port", port)
	return p, nil
}

func (p *Publisher) watchState() {
	for {
		select {
		case <-p.stop:
			return
		case sig := <-p.signals:
			if len(sig.Body) == 0 {
				continue
			}
			state, ok := sig.Body[0].(int32)
			if !ok {
				continue
			}
			switch state {
			case stateCollision:
				// A service with this name already exists (typically
				// another btproxy instance announcing on the same
				// network).
				p.log.Warn("mDNS service name already in use")
			case stateFailure:
				p.log.Error("mDNS entry group failed")
			case stateEstablished:
				p.log.Info("mDNS service established")
			}
		}
	}
}

// Unpublish removes the entry group and releases the D-Bus connection.
func (p *Publisher) Unpublish() {
	close(p.stop)
	if err := p.group.Call(groupIface+".Free", 0).Err; err != nil {
		p.log.Debug("mdns: Free failed", "err", err)
	}
	p.conn.Close()
}
