# btproxy wire-protocol reference

Extracted from the esphome-linux C implementation and cross-verified against
`github.com/esphome/aioesphomeapi` (Home Assistant's ESPHome client). This is
the complete surface btproxy needs to implement.

## Framing

Verified against `aioesphomeapi/_frame_helper/plain_text.py` (receive) and
`_frame_helper/packets.py` (send):

```
frame = 0x00 || varuint(len(payload)) || varuint(msg_type) || payload
```

- The length varuint counts the **payload only** — the type varint is NOT
  included (matches esphome-linux's `esphome_frame_message`).
- Max frame size 65535 (enforced by aioesphomeapi).
- Preamble `0x01` = "connection requires encryption" (close immediately);
  anything else is a protocol error.

## Message types (subset)

| Type | Message | Notes |
|-----:|----------|-------|
| 1/2  | HelloRequest / HelloResponse | resp: 1 api_version_major u32, 2 api_version_minor u32, 3 server_info str, 4 name str |
| 3/4  | ConnectRequest / ConnectResponse | resp: 1 invalid_password bool (send false) |
| 5    | DisconnectRequest | close connection after finishing |
| 7/8  | PingRequest / PingResponse | response is empty |
| 9/10 | DeviceInfoRequest / DeviceInfoResponse | see below |
| 11   | ListEntitiesRequest | reply with ListEntitiesSwitchResponse then 19 |
| 17   | ListEntitiesSwitchResponse | 1 object_id str, 2 key **fixed32**, 3 name str, 5 icon str, 6 assumed_state bool, 7 disabled_by_default bool, 8 entity_category u32, 9 device_class str |
| 19   | ListEntitiesDoneResponse | empty |
| 20/26 | SubscribeStatesRequest / SwitchStateResponse | state resp: 1 key fixed32, 2 state bool |
| 33   | SwitchCommandRequest | 1 key fixed32, 2 state bool |
| 36/37 | GetTimeRequest / GetTimeResponse | resp: 1 epoch_seconds u32 |
| 66   | SubscribeBluetoothLEAdvertisementsRequest | 1 flags u32: 1=RAW, 2=NON_RAW |
| 87   | UnsubscribeBluetoothLEAdvertisementsRequest | |
| 93   | BluetoothLERawAdvertisementsResponse | 1: repeated nested BluetoothLERawAdvertisement |

Ignore-and-log is acceptable for everything else (SubscribeLogs 28,
SubscribeHomeassistantStates 38, ScannerSetMode 127, ...).

## DeviceInfoResponse fields

```
 1 uses_password bool            (false)
 2 name str                      (hostname / --name)
 3 mac_address str               (primary NIC MAC)
 4 esphome_version str           (e.g. "2025.12.0" — what HA displays)
 5 compilation_time str
 6 model str
 7 has_deep_sleep bool           (false)
10 webserver_port u32            (omit / 0)
12 manufacturer str
13 friendly_name str
15 bluetooth_proxy_feature_flags u32
16 suggested_area str
18 bluetooth_mac_address str     (adapter MAC from BlueZ)
19 api_encryption_supported bool (false)
```

Bluetooth proxy feature flags (field 15):
`PASSIVE_SCAN=0x01, ACTIVE_CONNECTIONS=0x02, REMOTE_CACHING=0x04, PAIRING=0x08,
CACHE_CLEARING=0x10, RAW_ADVERTISEMENTS=0x20` → btproxy sends **0x21**.

## BluetoothLERawAdvertisement (nested in type 93, field 1)

```
1 address u64 (varint)   — MAC packed big-endian: mac[0]<<40 | mac[1]<<32 | ... | mac[5]
2 rssi sint32 (zigzag)
3 address_type u32       — 0 public, 1 random
4 data bytes             — raw AD structures, max 62 bytes
```

Batch up to 16 advertisements per type-93 message; flush at least every 100 ms
(esphome-linux constants: `BLE_MAX_ADV_BATCH 16`, `BLE_BATCH_FLUSH_INTERVAL_MS
100`). Send only to clients that subscribed with flags=RAW (1).

## Handshake flow (what HA does)

```
client → HelloRequest              server → HelloResponse (api version 1.12)
client → ConnectRequest            server → ConnectResponse (invalid_password=false)
client → DeviceInfoRequest         server → DeviceInfoResponse (flags 0x21)
client → ListEntitiesRequest       server → ListEntitiesSwitchResponse ×N + ListEntitiesDone
client → SubscribeStatesRequest    server → SwitchStateResponse(100, current)
client → SubscribeBluetoothLEAdvertisements(flags=1)
                                   server → starts scanning, streams type 93
client → PingRequest (periodic)   server → PingResponse
client → SwitchCommand(100, on/off)  server toggles discovery, replies SwitchStateResponse
```

BLE Scanning switch: key = 100 (fixed32), object_id `ble_scanning`,
name `BLE Scanning`.

## Protobuf primitives needed

- varint (u32/u64/bool/enum), zigzag sint32 (RSSI), fixed32 (entity keys),
  fixed64, length-delimited (string/bytes/nested message)
- skip-unknown-field on decode (HA may send fields we don't parse)
- empty-string/zero fields: omit (esphome-linux omits optionals)

## BlueZ D-Bus surface (scanner backend)

- System bus → `org.bluez`, adapter at `/org/bluez/hci0` (interface
  `org.bluez.Adapter1`); enumerate via ObjectManager `GetManagedObjects`.
- `SetDiscoveryFilter({"Transport": "le", "DuplicateData": false})` then
  `StartDiscovery()` / `StopDiscovery()`.
- Signals:
  - `org.freedesktop.DBus.ObjectManager.InterfacesAdded` — new temporary
    devices arrive with full properties from one advertisement.
  - `org.freedesktop.DBus.Properties.PropertiesChanged` with interface
    `org.bluez.Device1` — RSSI/ManufacturerData/ServiceData/ServiceUUIDs/
    TxPower updates.
- `org.bluez.Device1` properties: `Address` "AA:BB:CC:DD:EE:FF",
  `AddressType` "public"/"random", `Name`/`Alias` str, `RSSI` int16,
  `ManufacturerData` map[uint16][]byte, `ServiceData` map[uuid][]byte,
  `ServiceUUIDs` []string, `TxPower` int16, `Paired` bool.
- Keep a per-MAC snapshot; on any event merge changed props, emit an
  Advertisement (address, address_type, rssi, re-encoded AD TLVs).

### AD TLV re-encoding (parsed BlueZ props → raw 62-byte payload)

```
0x03 complete 16-bit service UUID list   (ServiceUUIDs, 4-hex-digit UUIDs)
0x05 complete 128-bit UUID list
0x07 complete 32-bit UUID list
0x09 complete local name                 (Name)
0x0A tx power                            (TxPower, int8)
0x16 16-bit service data                 (ServiceData, 4-hex-digit keys)
0x21 128-bit service data                (ServiceData, long keys)
0xFF manufacturer data                   (ManufacturerData, one entry per key)
```

Priority when truncating to 62 bytes: manufacturer/service data first, then
UUIDs, then tx power, then name.

### avahi mDNS publish (optional, reuses godbus)

`org.freedesktop.Avahi.Server` → `EntryGroupNew()` → `AddService(-1, -1, 0,
name, "_esphomelib._tcp", "", [], 6053, TXT)` with TXT
`["version=2025.12.0", "mac=<nic mac>"]`, then `Commit()`.

## Golden vectors (C-produced)

Generated by compiling `esphome-linux/src/esphome_proto.c` + `switch_proto.c`
with a small harness that encodes representative messages and prints hex.
Used as-is in `internal/pb`/`internal/frame` tests. Regenerate with a harness
that fills the structs below and prints the encoded bytes.

```
HELLO_PAYLOAD 0801100c1a11627470726f787920323032362e31302e302207627470726f7879
HELLO_FRAME 0020020801100c1a11627470726f787920323032362e31302e302207627470726f7879
CONNECT_PAYLOAD 0800
DEVICEINFO_PAYLOAD 08001207627470726f78791a1161613a62623a63633a64643a65653a6666
  2209323032362e31302e302a144f637420323220323032362032323a33393a30303207627470726f7879
  38006207627470726f78796a07627470726f7879782192011161613a62623a63633a64643a65653a6666
BLEADVS_PAYLOAD 0a1d08ffddf7e6bcd72a108d011800220e020106020a0007ff4c0010050f12
  0a1808e6aa919aa3a40410751801220a05094142436403160f18
BLEADVS_FRAME 00395d0a1d08ffddf7e6bcd72a108d011800220e020106020a0007ff4c0010050f12
  0a1808e6aa919aa3a40410751801220a05094142436403160f18
SWITCHSTATE_PAYLOAD 0d640000001001
SWITCHSTATE_FRAME 00071a0d640000001001
LISTSWITCH_PAYLOAD 0a0c626c655f7363616e6e696e6715640000001a0c424c45205363616e6e696e67
  2a136d64693a626c7565746f6f74682d617564696f
```

Inputs: hello = v1.12, server_info "btproxy 2026.10.0", name "btproxy";
device info name "btproxy", MAC aa:bb:cc:dd:ee:ff, flags 0x21; BLE batch of
two advertisements (AA:BB:CC:DD:EE:FF public RSSI -71 with flags/txpower/
Apple-style manufacturer data, and 11:22:33:44:55:66 random RSSI -59 with
name "ABCd" and service data 0x0f18); switch state key 100 state true;
list-entities switch key 100, object_id "ble_scanning", icon
"mdi:bluetooth-audio".
