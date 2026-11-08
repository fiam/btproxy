# Podman Quadlet example

This example uses the host BlueZ/Avahi services through D-Bus, an unprivileged
container user, a read-only filesystem, and a dedicated AppArmor profile.
It has been exercised with Podman 5.7 on Ubuntu 26.04 and a Raspberry Pi 5.

1. Check out the desired release commit and run `go generate ./...`,
   `go test -timeout 30s ./...`, and `go vet ./...`.
2. Build a static binary for your host. For ARM64:

   ```sh
   CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o containers/btproxy ./cmd/btproxy
   cp LICENSE containers/LICENSE
   sudo podman build --pull=never -t localhost/btproxy:pinned -f containers/Containerfile containers
   ```

3. Create `/etc/btproxy/api.env` owned by root with mode 0600. Set
   `BTPROXY_NOISE_KEY=` to a random 32-byte base64 key (`openssl rand -base64 32`).
   Keep this file private. The same key goes in HA's ESPHome integration.
4. Replace the example MAC in `btproxy.container` with your scale's address.
   Remove `--allow-address` for unfiltered forwarding.
5. Install the AppArmor profile as `/etc/apparmor.d/btproxy`, load it using
   `sudo apparmor_parser -r /etc/apparmor.d/btproxy`, and install the Quadlet as
   `/etc/containers/systemd/btproxy.container`.
6. Run `sudo systemctl daemon-reload` and `sudo systemctl start btproxy`.
   The Quadlet's Install section enables startup at boot.
7. Add the device through HA's **ESPHome** integration, using port 6053 and the
   encryption key. HA then registers it as a remote Bluetooth scanner.

On systems without AppArmor, omit the AppArmor-specific PodmanArgs setting.
After changing a loaded AppArmor profile, restart the container. The example
includes BlueZ broadcast permissions and Go's self-signalling under crun's stacked
profile. Investigate kernel AppArmor denials if the scanner registers but receives
no updates. A running scanner alone does not prove advertisement delivery.

The service restarts on process failure. It does not repair a wedged physical
Bluetooth controller or persist sensor measurements; HA handles the latter.
