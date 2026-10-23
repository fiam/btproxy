# btproxy — ESPHome-native Bluetooth proxy in Go

A single Go binary that makes a Linux host act as a **passive ESPHome
Bluetooth proxy** for Home Assistant, without taking over the Bluetooth
adapter: it scans through the BlueZ D-Bus API, so the system `bluetoothd`
keeps running normally (desktop pairing, `bluetoothctl`, audio — all
unaffected).

Home Assistant's ESPHome integration sees the host as a native ESPHome
device on TCP 6053, discovers it via mDNS (avahi) if available, and receives
raw BLE advertisement batches (`BluetoothLERawAdvertisementsResponse`) —
the same message flow the C reference implementation uses.

```
Home Assistant ──TCP 6053, ESPHome Native API──▶ btproxy ──D-Bus──▶ bluetoothd ─▶ hci0
```

## How it works

- **`tinypb/`** — a tiny protobuf wire-format runtime (varint, zigzag,
  fixed32/64, length-delimited fields) with golden-vector tests against the
  reference C implementation, plus `tinypb/cmd/tinypbc`, a `protoc`-style
  compiler for a minimal proto-subset IDL (`internal/esphome/api.pidl`).
  Message structs, encoders and decoders are generated from the IDL via
  `go generate`.
- **`internal/frame/`** — ESPHome framing:
  `0x00 || varint(len payload) || varint(type) || payload` (length counts
  the payload only) with a streaming decoder.
- **`internal/esphome/`** — TCP server, per-client sessions, handshake
  (Hello/Connect/DeviceInfo/ListEntities/SubscribeStates), the `BLE Scanning`
  switch entity (key 100), advertisement batching (16 per batch, 100 ms
  flush interval) and broadcast to RAW-flag subscribers.
- **`internal/bluez/`** — passive scanner: `SetDiscoveryFilter
  {Transport: le, DuplicateData: false}` + `StartDiscovery`, PropertiesChanged
  / InterfacesAdded signals, per-MAC snapshot cache, re-encoding of the
  parsed BlueZ properties into standard AD structures (manufacturer data,
  service data, UUIDs, tx power, name — capped at 62 bytes, little-endian).
  A watchdog restarts discovery if something stops it externally.
- **`internal/mdns/`** — announces `_esphomelib._tcp` through the avahi
  daemon's D-Bus API (reuses godbus, no extra dependency).

Non-stdlib dependency: **`github.com/godbus/dbus/v5`** only.

## Build

Requirements: `mise` (pins Go; see `mise.toml`), a Linux host with BlueZ
(`bluetoothd`), and optionally `avahi-daemon`.

```sh
make bootstrap    # mise install (Go toolchain)
make build        # bin/btproxy (tools are run via 'go run ./tools/...')
make test         # go test ./...
```

## Run

```sh
./bin/btproxy [--port 6053] [--name hostname] [--adapter hci0] [--mdns] [--debug]
```

- `--port` — ESPHome native API TCP port (default 6053)
- `--name` — device name reported to Home Assistant (default: hostname)
- `--adapter` — BlueZ adapter to scan (default: hci0)
- `--mdns` — announce via avahi (requires avahi-daemon; otherwise point HA
  at the host manually)
- `--password` — require an API password from clients (default:
  `$BTPROXY_PASSWORD`; unset/empty: no password). Home Assistant prompts
  for this password when you add the integration, and refuses everything
  but the handshake until it is presented. The API remains plaintext, so
  this prevents unauthorized enrollment on the LAN but is not encryption.
- `--debug` — verbose logging

Scanning starts when the first client subscribes with the RAW flag and the
`BLE Scanning` switch (exposed as an entity) is on; it stops when the last
subscriber leaves or the switch is toggled off from HA.

Home Assistant sees three entities on the device:

| Entity | Type | Description |
|---|---|---|
| `BLE Scanning` | switch | gates discovery (on by default) |
| `Uptime` | text sensor | instance uptime, ESPHome-style (`1d 2h 3m 45s`), refreshed every minute |
| `Advertisements` | sensor (`adv`, total-increasing) | raw advertisements forwarded since process start |

## Home Assistant setup

1. Run btproxy on a host in the same LAN as the HA VM, with `--mdns`
   (avahi running), or note its IP.
2. In Home Assistant: Settings → Devices & Services → Add Integration →
   **ESPHome**.
3. Either pick the discovered device (mDNS) or enter `<ip>:6053` manually.
   When btproxy runs with `--password`, enter the same password in the
   integration's password field — without it, the server answers the
   handshake with `invalid_password` and closes any connection that tries
   to read entities or subscribe to advertisements.
4. The integration shows a switch entity `BLE Scanning`. BLE advertisements
   (for trackers, e.g. ESPresense-style, BTHome or room-presence sensors)
   start flowing as soon as HA subscribes.

## systemd

A hardened unit template ships in `systemd/btproxy.service` (dynamic user,
`ProtectSystem=strict`, no capabilities). BlueZ's default D-Bus policy
allows any system-bus user to use `org.bluez`, so no root is required:

```sh
make install          # binary → /usr/local/bin, unit → /etc/systemd/system,
                      # firewalld definition → /etc/firewalld/services (if firewalld exists)
sudo systemctl daemon-reload
sudo systemctl enable --now btproxy
journalctl -u btproxy -f
```

On firewalld hosts, allow the API port — the ESPHome dialog's "Unable to
connect" error is usually this, and the shipped systemd unit prints a
journal warning on startup when it detects this state (the check is
informational: btproxy starts and listens regardless, and opening the
firewall takes effect immediately — no service restart needed). Reload
first so firewalld loads the freshly installed service definition, then
add it:

```sh
sudo firewall-cmd --reload
sudo firewall-cmd --permanent --add-service=btproxy && sudo firewall-cmd --reload
```

or restricted to the Home Assistant host only:

```sh
sudo firewall-cmd --permanent --add-rich-rule='rule family=ipv4 source address=<ha-ip> port port=6053 protocol=tcp accept'
sudo firewall-cmd --reload
```

## RPM packaging (Fedora)

`btproxy.spec` packages the binary, the systemd unit and the firewalld
service definition; the installed firewalld XML lands in
`/usr/lib/firewalld/services`, so `firewall-cmd --add-service=btproxy`
works without copying anything to `/etc`. From a git checkout, the
easiest path is:

```sh
make rpm          # builds rpm/RPMS/<arch>/btproxy-<version>-0.git<hash>.rpm
sudo dnf install rpm/RPMS/$(uname -m)/btproxy-*.rpm
```

`make rpm` creates the source tarball with `git archive HEAD`, so the RPM
contains exactly the commit referenced by the embedded hash (a dirty
working tree prints a warning — commit first to include your changes).
The snapshot release `0.git<hash>` sorts below a formal release `1`, so a
later released version upgrades cleanly via `dnf upgrade`.

Manual build, if you prefer:

```sh
git archive --prefix=btproxy-<version>/ -o ~/rpmbuild/SOURCES/btproxy-<version>.tar.gz HEAD
rpmbuild -bb btproxy.spec
sudo dnf install ~/rpmbuild/RPMS/$(uname -m)/btproxy-<version>*.rpm
```

Notes:

- Bump `Version:` in the spec and add a `%changelog` entry per release;
  the RPM version-release is stamped into the binary and shows up in
  logs and Home Assistant's device info.
- The build runs `go generate` (the generated codec is not in the
  tarball) and `%check` runs the test suite.
- Module downloads happen at build time; for fully offline builds run
  `go mod vendor` first and add `-mod=vendor` to the go commands in the
  spec.

## Development tools (`tools/`)

- `btprobe` (in `tools/`) — HA-style client: full handshake, prints
  DeviceInfo/entities, toggles the switch, dumps decoded advertisement
  batches:
  ```sh
  go run ./tools/btprobe --addr 127.0.0.1:6053 --duration 10s [--switch on|off]
  ```
- `tools/scandbg` — runs the BlueZ scanner directly and dumps raw
  re-encoded advertisements (no ESPHome layer). Useful when debugging
  discovery issues.
- `tools/dbusmon` — minimal D-Bus signal monitor (default: all `org.bluez`
  signals), for comparing what BlueZ emits against what btproxy sees.

## Protocol reference

Wire format details, message field numbers, and C-produced golden vectors
live in `docs/protocol-notes.md`. The message set is defined in
`internal/esphome/api.pidl`; regenerate the codec with `make generate`
(`go generate ./...`).

## Design notes

- **Coexistence with the desktop:** BlueZ reference-counts discovery
  sessions, so a user running `bluetoothctl scan on/off` (or any other
  discovery client) neither breaks btproxy nor gets broken by it — verified
  with concurrent scans and uninterrupted advertisement flow. A watchdog
  still re-arms discovery if something stops the adapter outright.
- **D-Bus permissions:** BlueZ's default bus policy allows any system-bus
  user to use `org.bluez`, and avahi publishes are unrestricted too, so the
  daemon needs neither root nor polkit rules; the shipped systemd unit runs
  under a dynamic user.
- **Fidelity:** BlueZ hands us parsed device properties, not raw packets.
  Device snapshots (seeded from the ObjectManager at discovery start so
  cached devices carry their full view, not just RSSI) are re-encoded into
  standard AD structures — known loss: the original Flags AD and the
  adv/scan-response split.

## Credits

btproxy owes its protocol behavior to
[esphome-linux](https://github.com/yinzara/esphome-linux), a C
implementation of the same idea whose vendored BLE stack motivated this
rewrite: its message encoders served as the reference for the wire format
(and the source of the golden test vectors), and the framing was
cross-verified against the
[aioesphomeapi](https://github.com/esphome/aioesphomeapi) client used by
Home Assistant.

## Limitations

- Passive proxy only: no GATT connections (feature flags 0x21).
- No API encryption (plaintext frames). Optional password authentication
  (`--password` / `$BTPROXY_PASSWORD`) gates everything but the handshake;
  the password itself still crosses the network in cleartext, so pair this
  with a firewalled LAN or an isolated VLAN.
- Advertisement re-encoding loses the original Flags AD and the
  adv/scan-response split; BlueZ hands us parsed properties, which we
  re-encode into standard AD structures.
