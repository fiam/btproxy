//go:generate go run ../../tinypb/cmd/tinypbc -in api.pidl -out api_gen.go

package esphome

import (
	"fmt"
)

// BLE advertisement subscription flags (SubscribeBluetoothLEAdvertisementsRequest.flags).
const (
	SubFlagRaw    = 1
	SubFlagNonRaw = 2
)

// Bluetooth proxy feature flags (DeviceInfoResponse.bluetooth_proxy_feature_flags).
const (
	FeaturePassiveScan       = 0x01
	FeatureActiveConnection  = 0x02
	FeatureRemoteCaching     = 0x04
	FeaturePairing           = 0x08
	FeatureCacheClearing     = 0x10
	FeatureRawAdvertisements = 0x20
)

// DefaultFeatureFlags is what a passive raw-advertisement proxy advertises.
const DefaultFeatureFlags = FeaturePassiveScan | FeatureRawAdvertisements // 0x21

// SwitchKeyBLEScanning is the entity key of the BLE Scanning switch.
const SwitchKeyBLEScanning uint32 = 100

// TextSensorKeyUptime is the entity key of the Uptime text sensor.
const TextSensorKeyUptime uint32 = 101

// SensorKeyAdvertisements is the entity key of the Advertisements counter
// sensor.
const SensorKeyAdvertisements uint32 = 102

// APIVersionMajor/Minor are claimed in HelloResponse. 1.12 matches the C
// reference implementation and current Home Assistant clients.
const (
	APIVersionMajor = 1
	APIVersionMinor = 12
)

// PackMAC converts "AA:BB:CC:DD:EE:FF" into the big-endian packed form used
// by BluetoothLERawAdvertisement.address.
func PackMAC(mac string) (uint64, error) {
	var b [6]byte
	n := 0
	for i := 0; i < len(mac); i++ {
		c := mac[i]
		var v byte
		switch {
		case c >= '0' && c <= '9':
			v = c - '0'
		case c >= 'a' && c <= 'f':
			v = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			v = c - 'A' + 10
		case c == ':':
			continue
		default:
			return 0, fmt.Errorf("invalid MAC address %q", mac)
		}
		if n >= 12 {
			return 0, fmt.Errorf("invalid MAC address %q", mac)
		}
		b[n/2] = b[n/2]<<4 | v
		n++
	}
	if n != 12 {
		return 0, fmt.Errorf("invalid MAC address %q", mac)
	}
	return uint64(b[0])<<40 | uint64(b[1])<<32 | uint64(b[2])<<24 |
		uint64(b[3])<<16 | uint64(b[4])<<8 | uint64(b[5]), nil
}
