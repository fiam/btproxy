package bluez

import (
	"github.com/godbus/dbus/v5"
	"log/slog"
	"testing"
)

func TestLiveScaleDataOnly(t *testing.T) {
	s := &Scanner{RequireLiveServiceData: true, log: slog.Default(), adapterPath: "/org/bluez/hci0", advs: make(chan Adv, 10), devices: make(map[string]*deviceProps)}
	path := dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF")
	props := map[string]dbus.Variant{
		"Address": dbus.MakeVariant("AA:BB:CC:DD:EE:FF"), "RSSI": dbus.MakeVariant(int16(-85)),
		"ServiceData": dbus.MakeVariant(map[string]dbus.Variant{"0000181b-0000-1000-8000-00805f9b34fb": dbus.MakeVariant([]byte{2, 36, 178, 7, 3, 4, 23, 55, 35, 0, 0, 152, 58})}),
	}
	// ObjectManager signals are emitted at /, with the device path in the body.
	s.handleSignal(&dbus.Signal{Path: "/", Name: objectMgrIface + ".InterfacesAdded", Body: []interface{}{path, map[string]map[string]dbus.Variant{deviceIface: props}}})
	if len(s.advs) != 1 {
		t.Fatal("new live device was not forwarded")
	}
	<-s.advs
	s.updateDevice(string(path), map[string]dbus.Variant{"RSSI": dbus.MakeVariant(int16(-90))})
	if len(s.advs) != 0 {
		t.Fatal("RSSI-only update replayed cached weight")
	}
	s.updateDevice(string(path), map[string]dbus.Variant{"ServiceData": props["ServiceData"]})
	if len(s.advs) != 1 {
		t.Fatal("identical live advertisement was dropped")
	}
	<-s.advs
	s.handleSignal(&dbus.Signal{Path: "/", Name: objectMgrIface + ".InterfacesRemoved", Body: []interface{}{path, []string{deviceIface}}})
	if len(s.devices) != 0 {
		t.Fatal("removed device stayed cached")
	}
}
