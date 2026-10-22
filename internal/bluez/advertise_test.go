package bluez

import (
	"io"
	"log/slog"
	"testing"

	"github.com/godbus/dbus/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustV(t *testing.T, v any) dbus.Variant {
	t.Helper()
	return dbus.MakeVariant(v)
}

func TestEncodeADFullSnapshot(t *testing.T) {
	p := &deviceProps{
		Address:          "AA:BB:CC:DD:EE:FF",
		AddressType:      0,
		RSSI:             -71,
		Name:             "ABCd",
		TxPower:          -12,
		HasTxPower:       true,
		ManufacturerData: map[uint16][]byte{0x004C: {0x10, 0x05, 0x0f, 0x12}},
		ServiceData: map[string][]byte{
			"0000fe2c-0000-1000-8000-00805f9b34fb": {0x01, 0x02},
		},
		ServiceUUIDs: []string{
			"0000180f-0000-1000-8000-00805f9b34fb", // 16-bit base
			"0000fe2c-0000-1000-8000-00805f9b34fb",
		},
	}
	// manufacturer 0x004C: len 7, type 0xff, key little-endian
	// service data 16-bit: len 5, type 0x16, uuid little-endian 2c fe
	// uuid list 16-bit: len 5, type 0x03, 0f18 2cfe
	// tx power: len 2, type 0x0a, f4
	// name: len 5, type 0x09, "ABCd"
	want := []byte{
		0x07, 0xff, 0x4c, 0x00, 0x10, 0x05, 0x0f, 0x12,
		0x05, 0x16, 0x2c, 0xfe, 0x01, 0x02,
		0x05, 0x03, 0x0f, 0x18, 0x2c, 0xfe,
		0x02, 0x0a, 0xf4,
		0x05, 0x09, 'A', 'B', 'C', 'd',
	}
	assert.Equal(t, want, encodeAD(p))
}

func TestUUIDSizes(t *testing.T) {
	// UUIDs are little-endian in AD structures.
	id, n := uuidBytes("0000180d-0000-1000-8000-00805f9b34fb")
	require.Equal(t, 2, n)
	assert.Equal(t, []byte{0x0d, 0x18}, id)

	id, n = uuidBytes("1234")
	require.Equal(t, 2, n)
	assert.Equal(t, []byte{0x34, 0x12}, id)

	id, n = uuidBytes("12345678")
	require.Equal(t, 4, n)
	assert.Equal(t, []byte{0x78, 0x56, 0x34, 0x12}, id)

	id, n = uuidBytes("12345678-1234-1234-1234-123456789abc")
	require.Equal(t, 16, n)
	assert.Equal(t,
		[]byte{0xbc, 0x9a, 0x78, 0x56, 0x34, 0x12, 0x34, 0x12, 0x34, 0x12, 0x34, 0x12, 0x78, 0x56, 0x34, 0x12},
		id)
}

func TestEncodeADTruncationPriority(t *testing.T) {
	// 12 manufacturer entries of 4 payload bytes: 8 bytes per AD
	// structure. Seven fit (56 bytes); the UUID list (4 bytes) still fits
	// (60); tx power (3 bytes) and the name no longer fit — they are
	// dropped, keeping manufacturer/UUID data highest priority.
	manuf := make(map[uint16][]byte)
	for i := 0; i < 12; i++ {
		manuf[uint16(i)] = []byte{0xaa, 0xbb, 0xcc, 0xdd}
	}
	p := &deviceProps{
		Address:          "AA:BB:CC:DD:EE:FF",
		RSSI:             -60,
		ManufacturerData: manuf,
		ServiceUUIDs:     []string{"0000180f-0000-1000-8000-00805f9b34fb"},
		TxPower:          -12,
		HasTxPower:       true,
		Name:             "abcdefghijklmnop",
	}
	got := encodeAD(p)
	require.Len(t, got, 60)

	want := []byte{}
	for i := 0; i < 7; i++ {
		want = append(want, 0x07, 0xff, byte(i), 0x00, 0xaa, 0xbb, 0xcc, 0xdd)
	}
	want = append(want, 0x03, 0x03, 0x0f, 0x18)
	assert.Equal(t, want, got)
	assert.NotContains(t, got, byte(0x0a), "tx power dropped")
	assert.NotContains(t, got, byte(0x09), "name dropped")
}

func TestEncodeADNameClipped(t *testing.T) {
	// Five manufacturer structures (40) + uuid list (4) + tx power (3)
	// leave 13 bytes for the name structure: 2 header + 11 payload.
	manuf := make(map[uint16][]byte)
	for i := 0; i < 5; i++ {
		manuf[uint16(i)] = []byte{0xaa, 0xbb, 0xcc, 0xdd}
	}
	p := &deviceProps{
		ManufacturerData: manuf,
		ServiceUUIDs:     []string{"0000180f-0000-1000-8000-00805f9b34fb"},
		TxPower:          -12,
		HasTxPower:       true,
		Name:             "abcdefghijklmnop",
	}
	got := encodeAD(p)
	require.Len(t, got, 62)
	assert.Equal(t, []byte{0x0e, 0x09, 'a', 'b', 'c', 'd', 'e', 'f', 'g', 'h', 'i', 'j', 'k', 'l', 'm'}, got[47:])
}

func TestServiceData128Bit(t *testing.T) {
	p := &deviceProps{
		ServiceData: map[string][]byte{
			"12345678-1234-1234-1234-123456789abc": {0x01},
		},
	}
	got := encodeAD(p)
	// 2 header + 16 little-endian uuid bytes + 1 byte payload.
	want := []byte{0x12, 0x21,
		0xbc, 0x9a, 0x78, 0x56, 0x34, 0x12, 0x34, 0x12,
		0x34, 0x12, 0x34, 0x12, 0x78, 0x56, 0x34, 0x12,
		0x01}
	assert.Equal(t, want, got)
}

func TestVariantAccessors(t *testing.T) {
	assert.Equal(t, "hello", stringOf(mustV(t, "hello")))
	assert.Equal(t, int16(-71), int16Of(mustV(t, int16(-71))))
	assert.Equal(t, int16(-42), int16Of(mustV(t, int32(-42))))

	// a{qv}: BlueZ's ManufacturerData shape.
	v := mustV(t, map[uint16]dbus.Variant{
		0x004C: mustV(t, []byte{1, 2, 3}),
	})
	md := manufacturerDataOf(v)
	require.Len(t, md, 1)
	assert.Equal(t, []byte{1, 2, 3}, md[0x004C])

	// a{qay} shape some godbus versions produce.
	v = mustV(t, map[uint16][]byte{0x0171: {9}})
	md = manufacturerDataOf(v)
	require.Len(t, md, 1)
	assert.Equal(t, []byte{9}, md[0x0171])

	// ServiceData a{sv}.
	sd := serviceDataOf(mustV(t, map[string]dbus.Variant{
		"0000fe2c-0000-1000-8000-00805f9b34fb": mustV(t, []byte{4, 5}),
	}))
	require.Len(t, sd, 1)
	assert.Equal(t, []byte{4, 5}, sd["0000fe2c-0000-1000-8000-00805f9b34fb"])

	// ServiceUUIDs.
	uuids := serviceUUIDsOf(mustV(t, []string{"b", "a"}))
	assert.Equal(t, []string{"a", "b"}, uuids)
}

// --- signal handling (fake dbus.Signals, no bus needed) ---

func newTestScanner() *Scanner {
	return &Scanner{
		log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		adapterPath: "/org/bluez/hci0",
		advs:        make(chan Adv, 16),
		devices:     make(map[string]*deviceProps),
	}
}

func propsChangedSignal(path string, changed map[string]dbus.Variant) *dbus.Signal {
	return &dbus.Signal{
		Name: "org.freedesktop.DBus.Properties.PropertiesChanged",
		Path: dbus.ObjectPath(path),
		Body: []any{deviceIface, changed, []string{}},
	}
}

func TestHandlePropertiesChanged(t *testing.T) {
	s := newTestScanner()

	s.handleSignal(propsChangedSignal("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF", map[string]dbus.Variant{
		"Address":          mustV(t, "AA:BB:CC:DD:EE:FF"),
		"AddressType":      mustV(t, "random"),
		"RSSI":             mustV(t, int16(-71)),
		"ManufacturerData": mustV(t, map[uint16]dbus.Variant{0x004C: mustV(t, []byte{0x10, 0x05})}),
	}))

	adv := <-s.advs
	assert.Equal(t, "AA:BB:CC:DD:EE:FF", adv.Address)
	assert.Equal(t, uint32(1), adv.AddressType)
	assert.Equal(t, int32(-71), adv.RSSI)
	assert.Equal(t, []byte{0x05, 0xff, 0x4c, 0x00, 0x10, 0x05}, adv.Data)

	// The exact same update must not emit a duplicate.
	s.handleSignal(propsChangedSignal("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF", map[string]dbus.Variant{
		"RSSI":             mustV(t, int16(-71)),
		"ManufacturerData": mustV(t, map[uint16]dbus.Variant{0x004C: mustV(t, []byte{0x10, 0x05})}),
	}))
	select {
	case adv := <-s.advs:
		t.Fatalf("unexpected duplicate advertisement: %+v", adv)
	default:
	}

	// A changed RSSI must emit again.
	s.handleSignal(propsChangedSignal("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF", map[string]dbus.Variant{
		"RSSI": mustV(t, int16(-80)),
	}))
	adv = <-s.advs
	assert.Equal(t, int32(-80), adv.RSSI)
}

func TestHandleInterfacesAdded(t *testing.T) {
	s := newTestScanner()
	sig := &dbus.Signal{
		Name: "org.freedesktop.DBus.ObjectManager.InterfacesAdded",
		Path: dbus.ObjectPath("/org/bluez/hci0/dev_11_22_33_44_55_66"),
		Body: []any{dbus.ObjectPath("/org/bluez/hci0/dev_11_22_33_44_55_66"), map[string]map[string]dbus.Variant{
			deviceIface: {
				"Address":      mustV(t, "11:22:33:44:55:66"),
				"AddressType":  mustV(t, "public"),
				"RSSI":         mustV(t, int16(-59)),
				"Name":         mustV(t, "TestDev"),
				"ServiceUUIDs": mustV(t, []string{"0000180f-0000-1000-8000-00805f9b34fb"}),
			},
		}},
	}
	s.handleSignal(sig)

	adv := <-s.advs
	assert.Equal(t, "11:22:33:44:55:66", adv.Address)
	assert.Equal(t, uint32(0), adv.AddressType)
	assert.Equal(t, int32(-59), adv.RSSI)
	assert.Contains(t, string(adv.Data), "\x03\x03\x0f\x18", "16-bit uuid list")
	assert.Contains(t, string(adv.Data), "\x08\x09TestDev", "name")
}

func TestInterfacesRemovedEvictsCache(t *testing.T) {
	s := newTestScanner()
	addPath := "/org/bluez/hci0/dev_11_22_33_44_55_66"
	s.handleSignal(&dbus.Signal{
		Name: "org.freedesktop.DBus.ObjectManager.InterfacesAdded",
		Path: dbus.ObjectPath(addPath),
		Body: []any{dbus.ObjectPath(addPath), map[string]map[string]dbus.Variant{
			deviceIface: {
				"Address": mustV(t, "11:22:33:44:55:66"),
				"RSSI":    mustV(t, int16(-59)),
			},
		}},
	})
	<-s.advs

	s.handleSignal(&dbus.Signal{
		Name: "org.freedesktop.DBus.ObjectManager.InterfacesRemoved",
		Path: dbus.ObjectPath(addPath),
		Body: []any{dbus.ObjectPath(addPath), map[string]dbus.Variant{
			deviceIface: mustV(t, true),
		}},
	})

	// After eviction, the next advertisement for the same path is emitted
	// as a fresh device (not deduplicated).
	s.handleSignal(propsChangedSignal(addPath, map[string]dbus.Variant{
		"RSSI": mustV(t, int16(-61)),
	}))
	select {
	case adv := <-s.advs:
		assert.Equal(t, int32(-61), adv.RSSI)
	default:
		t.Fatal("advertisement after cache eviction not emitted")
	}
}

func TestIgnoreSignalsForOtherAdapterOrInterface(t *testing.T) {
	s := newTestScanner()

	// Another adapter's device.
	s.handleSignal(propsChangedSignal("/org/bluez/hci1/dev_AA_BB_CC_DD_EE_FF", map[string]dbus.Variant{
		"Address": mustV(t, "AA:BB:CC:DD:EE:FF"),
		"RSSI":    mustV(t, int16(-71)),
	}))
	// Wrong interface.
	s.handleSignal(&dbus.Signal{
		Name: "org.freedesktop.DBus.Properties.PropertiesChanged",
		Path: dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF"),
		Body: []any{adapterIface, map[string]dbus.Variant{"Powered": mustV(t, true)}, []string{}},
	})

	select {
	case adv := <-s.advs:
		t.Fatalf("unexpected advertisement: %+v", adv)
	default:
	}
}

func TestPathToAddress(t *testing.T) {
	// BlueZ device paths use uppercase hex.
	assert.Equal(t, "E4:52:96:0C:5B:65", pathToAddress("/org/bluez/hci0/dev_E4_52_96_0C_5B_65"))
	assert.Equal(t, "11:22:33:44:55:66", pathToAddress("/org/bluez/hci1/dev_11_22_33_44_55_66"))
	assert.Equal(t, "", pathToAddress("/org/bluez/hci0/dev_gG_22_33_44_55_66"))
	assert.Equal(t, "", pathToAddress("/org/bluez/hci0"))
}

func TestCloneIsDeep(t *testing.T) {
	p := &deviceProps{
		Address:          "AA:BB:CC:DD:EE:FF",
		ManufacturerData: map[uint16][]byte{1: {1}},
		ServiceData:      map[string][]byte{"u": {2}},
		ServiceUUIDs:     []string{"a"},
	}
	c := p.clone()
	c.ManufacturerData[1] = []byte{9}
	c.ServiceData["u"] = []byte{9}
	c.ServiceUUIDs[0] = "b"
	assert.Equal(t, []byte{1}, p.ManufacturerData[1])
	assert.Equal(t, []byte{2}, p.ServiceData["u"])
	assert.Equal(t, []string{"a"}, p.ServiceUUIDs)
}
