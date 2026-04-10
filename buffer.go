package apm

import (
	"context"
	"sync"
	"time"
)

// Buffer batches events and flushes them either when full or on a timer.
//
// Differences from the Laravel Buffer.php:
//   - thread-safe (Go HTTP servers are concurrent),
//   - has its own goroutine that ticks every cfg.FlushInterval, so we
//     don't depend on a request lifecycle hook to drain the buffer.
//
// The flush callback is intentionally synchronous on the goroutine
// holding no lock — long network calls will not block Push().
type Buffer struct {
	maxSize int
	service string

	mu     sync.Mutex
	events []Event

	flushFn func(context.Context, []Event)

	stopCh chan struct{}
	doneCh chan struct{}
}

// NewBuffer creates a Buffer and starts its background flush goroutine.
// Call Stop to shut it down (drains everything and sends one last batch).
func NewBuffer(cfg Config, flushFn func(context.Context, []Event)) *Buffer {
	b := &Buffer{
		maxSize: cfg.BufferSize,
		service: cfg.ServiceName,
		flushFn: flushFn,
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
	}

	go b.run(cfg.FlushInterval)
	return b
}

// Push adds an event to the buffer. The service field is filled in
// automatically so callers don't need to remember it. If the buffer
// reaches max size after this push, an immediate flush is triggered
// in the background goroutine via a non-blocking signal.
func (b *Buffer) Push(e Event) {
	if e.Service == "" {
		e.Service = b.service
	}

	b.mu.Lock()
	b.events = append(b.events, e)
	full := len(b.events) >= b.maxSize
	b.mu.Unlock()

	if full {
		// Drain immediately on the caller's goroutine. The HTTP middleware
		// is already off the request hot path (defer), and the flushFn has
		// its own timeout via the Client. Doing it here avoids inventing a
		// signaling channel just to wake up the ticker loop.
		go b.flush(context.Background())
	}
}

// drain atomically swaps the slice out and returns the captured batch.
func (b *Buffer) drain() []Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.events) == 0 {
		return nil
	}
	out := b.events
	b.events = make([]Event, 0, b.maxSize)
	return out
}

// flush captures whatever is in the buffer and hands it to flushFn.
// Errors inside flushFn are the Client's problem (it logs and swallows).
func (b *Buffer) flush(ctx context.Context) {
	batch := b.drain()
	if len(batch) == 0 {
		return
	}
	b.flushFn(ctx, batch)
}

// run is the periodic flush loop. It exits when stopCh is closed and
// performs one final flush so nothing is lost on shutdown.
func (b *Buffer) run(interval time.Duration) {
	defer close(b.doneCh)

	if interval <= 0 {
		interval = 5 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-t.C:
			b.flush(context.Background())
		case <-b.stopCh:
			b.flush(context.Background())
			return
		}
	}
}

// Stop shuts down the background goroutine and drains any remaining events.
// It blocks until the final flush completes (or the goroutine exits).
func (b *Buffer) Stop() {
	close(b.stopCh)
	<-b.doneCh
}
