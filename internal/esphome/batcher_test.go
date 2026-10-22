package esphome

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBatcherFlushesWhenFull(t *testing.T) {
	var mu sync.Mutex
	var got int
	b := NewBatcher(16, time.Hour, func(advs []*BluetoothLERawAdvertisement) {
		mu.Lock()
		got += len(advs)
		mu.Unlock()
	})

	for i := 0; i < 15; i++ {
		b.Add(&BluetoothLERawAdvertisement{Address: uint64(i)})
	}
	mu.Lock()
	assert.Zero(t, got, "15 advertisements must not flush yet")
	mu.Unlock()

	b.Add(&BluetoothLERawAdvertisement{Address: 15})
	assert.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return got == 16
	}, time.Second, 10*time.Millisecond, "batch must flush at 16")
}

func TestBatcherFlushesOnInterval(t *testing.T) {
	var mu sync.Mutex
	var got int
	b := NewBatcher(16, 20*time.Millisecond, func(advs []*BluetoothLERawAdvertisement) {
		mu.Lock()
		got += len(advs)
		mu.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())
	go b.Run(ctx)

	b.Add(&BluetoothLERawAdvertisement{Address: 1})
	assert.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return got == 1
	}, time.Second, 5*time.Millisecond, "partial batch must flush on interval")

	cancel()
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return got == 1
	}, time.Second, 10*time.Millisecond)
}

func TestBatcherRunFinalFlush(t *testing.T) {
	var mu sync.Mutex
	var got int
	b := NewBatcher(100, time.Hour, func(advs []*BluetoothLERawAdvertisement) {
		mu.Lock()
		got += len(advs)
		mu.Unlock()
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.Run(ctx); close(done) }()

	b.Add(&BluetoothLERawAdvertisement{Address: 1})
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, got, "Run must flush on shutdown")
}
