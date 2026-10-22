// Command btproxy runs the ESPHome-native Bluetooth proxy server: it scans
// for BLE advertisements through BlueZ (system bluetoothd stays untouched)
// and serves them to Home Assistant over the ESPHome native API.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ankon/btproxy/internal/bluez"
	"github.com/ankon/btproxy/internal/esphome"
	"github.com/ankon/btproxy/internal/mdns"
)

var version = "dev"

// Batch parameters matching the C reference implementation.
const (
	advBatchMax      = 16
	advFlushInterval = 100 * time.Millisecond
)

func main() {
	var (
		port        = flag.Int("port", 6053, "TCP port for the ESPHome native API")
		name        = flag.String("name", "", "device name reported to Home Assistant (default: hostname)")
		adapter     = flag.String("adapter", "hci0", "BlueZ adapter to scan on")
		mdnsEnabled = flag.Bool("mdns", false, "announce via avahi/mDNS (requires avahi-daemon)")
		debug       = flag.Bool("debug", false, "verbose logging")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	host, err := os.Hostname()
	if err != nil {
		host = "btproxy"
	}
	// The kernel hostname may be an FQDN; use the short form as the
	// default device name (Home Assistant displays it, and the mDNS
	// instance name should be a single label).
	if i := strings.IndexByte(host, '.'); i > 0 {
		host = host[:i]
	}
	if *name == "" {
		*name = host
	}

	mac, err := primaryMAC()
	if err != nil {
		logger.Warn("could not determine host MAC", "err", err)
		mac = "00:00:00:00:00:00"
	}

	cfg := esphome.Config{
		Name:            *name,
		MACAddress:      mac,
		BluetoothMAC:    mac, // replaced by the adapter MAC below
		ESPHomeVersion:  "2026.10.0",
		CompilationTime: time.Now().Format(time.RFC3339),
		Model:           "btproxy " + version,
		Manufacturer:    "btproxy",
		FriendlyName:    *name,
		Port:            *port,
	}

	// Scanner (optional: the server still serves entities without it).
	var scanner *bluez.Scanner
	scanner, err = bluez.New(logger, *adapter)
	if err != nil {
		logger.Error("bluetooth scanner unavailable; running without BLE forwarding", "err", err)
		scanner = nil
	} else {
		if addr := scanner.AdapterAddress(); addr != "" {
			cfg.BluetoothMAC = addr
		}
		defer scanner.Close()
	}

	srv := esphome.NewServer(cfg, logger)
	if scanner != nil {
		scanCtx, cancelScan := context.WithCancel(ctx)
		defer cancelScan()
		srv.ScanControl = func(on bool) {
			if on {
				if err := scanner.Start(scanCtx); err != nil {
					logger.Error("start discovery failed", "err", err)
				}
			} else {
				scanner.Stop()
			}
		}

		// Advertisement pipeline: scanner → batcher → subscribed clients.
		batcher := esphome.NewBatcher(advBatchMax, advFlushInterval, srv.SendAdvertisements)
		go batcher.Run(ctx)
		go func() {
			for adv := range scanner.Advertisements() {
				addr, err := esphome.PackMAC(adv.Address)
				if err != nil {
					logger.Warn("dropping advertisement with bad address", "address", adv.Address)
					continue
				}
				batcher.Add(&esphome.BluetoothLERawAdvertisement{
					Address:     addr,
					RSSI:        adv.RSSI,
					AddressType: adv.AddressType,
					Data:        adv.Data,
				})
			}
		}()
	} else {
		srv.ScanControl = func(on bool) {
			logger.Warn("scan request ignored (no scanner)", "on", on)
		}
	}

	l, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		logger.Error("listen failed", "err", err)
		os.Exit(1)
	}

	// mDNS announcement via avahi (optional).
	if *mdnsEnabled {
		txt := []string{
			"version=" + cfg.ESPHomeVersion,
			"mac=" + cfg.MACAddress,
		}
		pub, err := mdns.Publish(logger, cfg.Name, *port, txt)
		if err != nil {
			logger.Warn("mDNS announcement failed; HA can still connect via ip:port", "err", err)
		} else {
			defer pub.Unpublish()
		}
	}
	logger.Info("btproxy listening",
		"version", version, "addr", l.Addr().String(), "name", *name, "adapter", *adapter)

	serveDone := make(chan struct{})
	go func() {
		if err := srv.Serve(l); err != nil {
			logger.Error("serve failed", "err", err)
		}
		close(serveDone)
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	srv.Close()
	<-serveDone
}

// primaryMAC returns the MAC address of the first non-loopback interface
// with a hardware address.
func primaryMAC() (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || len(iface.HardwareAddr) != 6 {
			continue
		}
		return iface.HardwareAddr.String(), nil
	}
	return "", fmt.Errorf("no interface with a hardware address found")
}
