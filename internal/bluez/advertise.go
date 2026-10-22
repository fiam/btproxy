// Package bluez implements a passive BLE scanner on top of the BlueZ D-Bus
// API. It coexists with a running system bluetoothd: discovery is driven
// through org.bluez.Adapter1, and advertisements are reconstructed from the
// parsed Device1 properties BlueZ reports on signals.
package bluez

import (
	"sort"
	"strings"
)

// maxADLength caps the re-encoded advertisement payload (31 + 31 bytes).
const maxADLength = 62

// deviceProps is the merged org.bluez.Device1 snapshot of one device.
type deviceProps struct {
	Address          string
	AddressType      uint32 // 0 public, 1 random
	RSSI             int32
	Name             string
	TxPower          int16
	HasTxPower       bool
	ManufacturerData map[uint16][]byte
	ServiceData      map[string][]byte
	ServiceUUIDs     []string
}

// txPowerInvalid is BlueZ's "no tx power in this advertisement" value.
const txPowerInvalid = 0x7FFF

// clone makes a deep copy so snapshots stay immutable once stored.
func (p *deviceProps) clone() *deviceProps {
	c := *p
	c.ManufacturerData = copyMap(p.ManufacturerData)
	c.ServiceData = copyMap(p.ServiceData)
	c.ServiceUUIDs = append([]string(nil), p.ServiceUUIDs...)
	return &c
}

func copyMap[K comparable, V any](m map[K]V) map[K]V {
	if m == nil {
		return nil
	}
	out := make(map[K]V, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// encodeAD re-encodes the snapshot into standard AD structures, capped at 62
// bytes. Priority when truncating: manufacturer and service data first, then
// UUIDs, then tx power, then the local name (clipped to fit).
func encodeAD(p *deviceProps) []byte {
	e := &adEncoder{}
	for _, key := range sortedKeys16(p.ManufacturerData) {
		e.append(0xFF, append([]byte{byte(key), byte(key >> 8)}, p.ManufacturerData[key]...))
	}
	for _, uuid := range sortedKeys(p.ServiceData) {
		id, n := uuidBytes(uuid)
		e.append(serviceDataType(n), append(id, p.ServiceData[uuid]...))
	}
	e.appendUUIDs(p.ServiceUUIDs)
	if p.HasTxPower && p.TxPower != txPowerInvalid {
		e.append(0x0A, []byte{byte(int8(p.TxPower))})
	}
	if p.Name != "" {
		e.appendClipped(0x09, []byte(p.Name))
	}
	return e.buf
}

func serviceDataType(n int) byte {
	switch n {
	case 2:
		return 0x16 // 16-bit service data
	case 4:
		return 0x20 // 32-bit service data
	default:
		return 0x21 // 128-bit service data
	}
}

type adEncoder struct {
	buf []byte
}

// append adds one complete AD structure if it fits.
func (e *adEncoder) append(typ byte, payload []byte) {
	if len(e.buf)+2+len(payload) > maxADLength {
		return
	}
	e.buf = append(e.buf, byte(len(payload)+1), typ)
	e.buf = append(e.buf, payload...)
}

// appendClipped adds a structure, truncating the payload to make it fit.
func (e *adEncoder) appendClipped(typ byte, payload []byte) {
	room := maxADLength - len(e.buf) - 2
	if room <= 0 {
		return
	}
	if len(payload) > room {
		payload = payload[:room]
	}
	e.append(typ, payload)
}

// appendUUIDs groups service UUIDs by size into the complete-list structures
// (0x03 16-bit, 0x07 32-bit, 0x05 128-bit).
func (e *adEncoder) appendUUIDs(uuids []string) {
	var u16, u32, u128 [][]byte
	for _, uuid := range uuids {
		id, n := uuidBytes(uuid)
		switch n {
		case 2:
			u16 = append(u16, id)
		case 4:
			u32 = append(u32, id)
		default:
			u128 = append(u128, id)
		}
	}
	e.appendList(0x03, u16)
	e.appendList(0x07, u32)
	e.appendList(0x05, u128)
}

func (e *adEncoder) appendList(typ byte, ids [][]byte) {
	if len(ids) == 0 {
		return
	}
	payload := make([]byte, 0, len(ids)*len(ids[0]))
	for _, id := range ids {
		payload = append(payload, id...)
	}
	e.append(typ, payload)
}

// uuidBytes converts a BlueZ UUID string into its AD representation,
// little-endian as the Bluetooth core spec encodes UUIDs in AD structures.
// Bluetooth-base UUIDs ("0000fe2c-0000-1000-8000-00805f9b34fb") compress to
// 2 bytes; 32-bit UUIDs to 4 bytes; everything else stays 16 bytes.
func uuidBytes(uuid string) (id []byte, size int) {
	uuid = strings.ToLower(strings.TrimSpace(uuid))
	uuid = strings.ReplaceAll(uuid, "-", "")
	switch len(uuid) {
	case 4, 8, 32:
		id = hexBytes(uuid)
		if id == nil {
			return nil, 0
		}
		if len(uuid) == 32 && strings.HasPrefix(uuid, "0000") &&
			strings.HasSuffix(uuid, "00001000800000805f9b34fb") {
			id = id[2:4]
		}
		reverse(id)
		return id, len(id)
	default:
		return nil, 0
	}
}

func reverse(b []byte) {
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
}

func hexBytes(s string) []byte {
	if len(s)%2 != 0 {
		return nil
	}
	out := make([]byte, len(s)/2)
	for i := 0; i < len(out); i++ {
		hi := hexVal(s[2*i])
		lo := hexVal(s[2*i+1])
		if hi < 0 || lo < 0 {
			return nil
		}
		out[i] = byte(hi<<4 | lo)
	}
	return out
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	default:
		return -1
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedKeys16[V any](m map[uint16]V) []uint16 {
	keys := make([]uint16, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}
