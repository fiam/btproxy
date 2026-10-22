package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/ankon/btproxy/internal/bluez"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s, err := bluez.New(logger, "hci0")
	if err != nil {
		panic(err)
	}
	fmt.Println("adapter:", s.AdapterAddress())
	go func() {
		for adv := range s.Advertisements() {
			fmt.Printf("ADV %s rssi=%d type=%d data=%x\n", adv.Address, adv.RSSI, adv.AddressType, adv.Data)
		}
	}()
	if err := s.Start(context.Background()); err != nil {
		panic(err)
	}
	time.Sleep(15 * time.Second)
	fmt.Println("done")
}
