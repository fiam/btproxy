// Command dbusmon is a minimal D-Bus signal monitor for debugging btproxy's
// BlueZ integration: it adds signal match rules on the system bus and prints
// every matching signal for a given duration.
//
// Usage:
//
//	go run ./tools/dbusmon [-sender org.bluez] [-duration 10s]
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/godbus/dbus/v5"
)

func main() {
	var (
		sender   = flag.String("sender", "org.bluez", "sender match rule (empty: match all senders)")
		iface    = flag.String("interface", "", "interface match rule (empty: match all interfaces)")
		duration = flag.Duration("duration", 10*time.Second, "how long to listen")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	conn, err := dbus.SystemBus()
	if err != nil {
		logger.Error("connect failed", "err", err)
		os.Exit(1)
	}

	opts := []dbus.MatchOption{}
	if *sender != "" {
		opts = append(opts, dbus.WithMatchSender(*sender))
	}
	if *iface != "" {
		opts = append(opts, dbus.WithMatchInterface(*iface))
	}
	if err := conn.AddMatchSignal(opts...); err != nil {
		logger.Error("add match failed", "err", err)
		os.Exit(1)
	}

	signals := make(chan *dbus.Signal, 256)
	conn.Signal(signals)

	deadline := time.After(*duration)
	for {
		select {
		case <-deadline:
			return
		case sig := <-signals:
			fmt.Printf("signal path=%s member=%s body=%v\n",
				sig.Path, sig.Name, sig.Body)
		}
	}
}
