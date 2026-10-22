package esphome

import (
	"context"
	"sync"
	"time"
)

// Batcher accumulates raw advertisements and flushes them in batches of at
// most max items, at least every interval — matching the C reference
// constants (16 per batch, 100 ms flush interval).
type Batcher struct {
	max      int
	interval time.Duration
	send     func(advs []*BluetoothLERawAdvertisement)

	mu  sync.Mutex
	buf []*BluetoothLERawAdvertisement
}

// NewBatcher creates a Batcher. Run must be called to start periodic
// flushes.
func NewBatcher(max int, interval time.Duration, send func(advs []*BluetoothLERawAdvertisement)) *Batcher {
	return &Batcher{max: max, interval: interval, send: send}
}

// Add appends an advertisement, flushing immediately when the batch is full.
func (b *Batcher) Add(adv *BluetoothLERawAdvertisement) {
	b.mu.Lock()
	b.buf = append(b.buf, adv)
	full := len(b.buf) >= b.max
	b.mu.Unlock()
	if full {
		b.Flush()
	}
}

// Flush sends everything buffered.
func (b *Batcher) Flush() {
	b.mu.Lock()
	advs := b.buf
	b.buf = nil
	b.mu.Unlock()
	if len(advs) > 0 {
		b.send(advs)
	}
}

// Run flushes periodically until ctx is done (with a final flush).
func (b *Batcher) Run(ctx context.Context) {
	t := time.NewTicker(b.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			b.Flush()
			return
		case <-t.C:
			b.Flush()
		}
	}
}
