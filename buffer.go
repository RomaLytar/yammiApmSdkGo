package apm

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Buffer is a non-blocking, bounded, batched event pipeline.
//
// The single hard rule: APM must NEVER slow down the host application.
// Everything in this file is shaped by that rule.
//
// Pipeline stages:
//
//	Push(event) ────► events chan ────► collector goroutine
//	                  (bounded,                │
//	                   drop on full)           │  batches up
//	                                           ▼
//	                                    batches chan ────► sender workers
//	                                    (bounded,          (fixed pool)
//	                                     drop on full)
//
// Two independent back-pressure valves:
//
//   - events chan: if the application produces events faster than the
//     collector can batch them, new events are DROPPED. The application
//     goroutine never blocks.
//
//   - batches chan: if the sender workers can't keep up (backend slow),
//     fresh batches are DROPPED. The collector goroutine never blocks.
//
// In both cases a counter is incremented so we can expose "dropped" as
// a metric. Dropping is a first-class feature, not an error. It's the
// reason the host app survives a backend outage or a traffic spike.
type Buffer struct {
	service string

	// events is the ingress channel. One slot per event capacity.
	events chan Event

	// batches is the egress channel feeding the sender pool. Each slot
	// carries a finalized batch ready to POST.
	batches chan []Event

	batchSize     int
	flushInterval time.Duration

	flushFn func(context.Context, []Event)

	// Counters, exposed via Stats() for observability of the SDK itself.
	droppedEvents  atomic.Uint64
	droppedBatches atomic.Uint64
	sentBatches    atomic.Uint64

	stopCh      chan struct{}
	collectorDone chan struct{}
	sendersDone   sync.WaitGroup
}

// BufferStats is a snapshot of the counters the buffer exposes. Useful
// for a /metrics endpoint in the host application or for debug logs.
type BufferStats struct {
	DroppedEvents  uint64
	DroppedBatches uint64
	SentBatches    uint64
	QueuedEvents   int
	QueuedBatches  int
}

// NewBuffer creates a Buffer with the given config and flushFn. It spins
// up a single collector goroutine and cfg.SenderWorkers sender goroutines.
// The pool is fixed-size — no goroutine is ever spawned on a hot path.
//
// Defaults are tuned for "do not lose data under normal load, survive a
// backend outage without OOM'ing the host app":
//
//   - ChannelSize = 100 000. At 1000 events/s that's 100 seconds of
//     buffered headroom; at 10 000 events/s it's 10 seconds. Enough to
//     ride out a GC pause or a slow batch-write in the backend without
//     dropping anything.
//
//   - BatchSize = 500. One HTTP POST per 500 events keeps the request
//     rate at ~20 POST/s under 10k events/s, which any HTTP server
//     handles comfortably.
//
//   - SenderWorkers = 4. A pool big enough to hide one slow request
//     behind fast ones, small enough not to overwhelm a weak backend.
//
//   - FlushInterval = 1s. A batch that isn't full yet still ships
//     within 1s, so the dashboard latency is bounded.
func NewBuffer(cfg Config, flushFn func(context.Context, []Event)) *Buffer {
	channelSize := cfg.ChannelSize
	if channelSize <= 0 {
		channelSize = 100000
	}
	batchSize := cfg.BufferSize
	if batchSize <= 0 {
		batchSize = 500
	}
	workers := cfg.SenderWorkers
	if workers <= 0 {
		workers = 4
	}
	flushInterval := cfg.FlushInterval
	if flushInterval <= 0 {
		flushInterval = 1 * time.Second
	}

	b := &Buffer{
		service:       cfg.ServiceName,
		events:        make(chan Event, channelSize),
		batches:       make(chan []Event, workers*2),
		batchSize:     batchSize,
		flushInterval: flushInterval,
		flushFn:       flushFn,
		stopCh:        make(chan struct{}),
		collectorDone: make(chan struct{}),
	}

	b.sendersDone.Add(workers)
	for i := 0; i < workers; i++ {
		go b.sender()
	}
	go b.collector()

	return b
}

// Push is the hot path. It must be O(1), lock-free, and NEVER block the
// caller. If the channel is full (traffic spike or backend outage), the
// event is dropped and a counter is incremented. That's the point.
func (b *Buffer) Push(e Event) {
	if e.Service == "" {
		e.Service = b.service
	}
	select {
	case b.events <- e:
	default:
		// Channel full. Drop. The application lives, the trace doesn't.
		// This is the core guarantee of the whole file.
		b.droppedEvents.Add(1)
	}
}

// collector batches events and hands finished batches to the sender pool.
// It is the only goroutine that reads from `events`, so no locking is
// needed on the batch slice.
func (b *Buffer) collector() {
	defer close(b.collectorDone)

	batch := make([]Event, 0, b.batchSize)
	ticker := time.NewTicker(b.flushInterval)
	defer ticker.Stop()

	// dispatch hands the current batch to the sender pool.
	//
	// Under normal load this is a non-blocking channel send: the sender
	// pool drains batches faster than the collector produces them, so
	// the batches channel is always near-empty.
	//
	// When the backend is temporarily slow (restart, brief network blip,
	// ClickHouse merge pause), the sender pool fills up. We then BLOCK
	// the collector goroutine until a slot frees up. Blocking the
	// collector is fine: the ingress channel keeps absorbing events
	// from the hot path — Push() still returns in nanoseconds.
	//
	// Only when BOTH channels are full — meaning the backend has been
	// unreachable long enough to consume all 100k ingress slots —
	// do we start dropping. At that point the host app would OOM if we
	// didn't, and "drop a batch" is strictly better than "kill the app".
	// This is the OOM-guard, not the happy path.
	dispatch := func() {
		if len(batch) == 0 {
			return
		}
		out := make([]Event, len(batch))
		copy(out, batch)
		batch = batch[:0]

		select {
		case b.batches <- out:
			// Fast path: sender pool has room.
		case <-b.stopCh:
			// Shutdown raced with dispatch; nothing to do.
		default:
			// Sender pool saturated. Try to WAIT, not drop, as long as
			// we are not on the verge of OOM. "Verge of OOM" is defined
			// as "ingress channel more than 90% full" — once we cross
			// that line, blocking further would starve Push() callers.
			if len(b.events) < cap(b.events)*9/10 {
				select {
				case b.batches <- out:
				case <-b.stopCh:
				}
			} else {
				b.droppedBatches.Add(1)
			}
		}
	}

	for {
		select {
		case e := <-b.events:
			batch = append(batch, e)
			if len(batch) >= b.batchSize {
				dispatch()
			}

		case <-ticker.C:
			dispatch()

		case <-b.stopCh:
			// Drain the ingress channel without blocking. Whatever we can
			// grab goes into the final batch; any remainder is lost, which
			// is fine — shutdown is the only time we accept that.
			drain := true
			for drain {
				select {
				case e := <-b.events:
					batch = append(batch, e)
					if len(batch) >= b.batchSize {
						dispatch()
					}
				default:
					drain = false
				}
			}
			dispatch()
			close(b.batches)
			return
		}
	}
}

// sender is the worker that actually posts to the APM backend. It has
// its own context.Background — the host application's request context
// must never reach here, because the host app's cancellation would abort
// in-flight APM uploads and we'd lose data for no reason.
func (b *Buffer) sender() {
	defer b.sendersDone.Done()
	for batch := range b.batches {
		b.flushFn(context.Background(), batch)
		b.sentBatches.Add(1)
	}
}

// Stats returns a snapshot of the buffer's internal counters. Safe to
// call from any goroutine.
func (b *Buffer) Stats() BufferStats {
	return BufferStats{
		DroppedEvents:  b.droppedEvents.Load(),
		DroppedBatches: b.droppedBatches.Load(),
		SentBatches:    b.sentBatches.Load(),
		QueuedEvents:   len(b.events),
		QueuedBatches:  len(b.batches),
	}
}

// Stop signals the collector to drain and exit, then waits for the
// sender pool to finish in-flight uploads. It's safe to call multiple
// times only from the same goroutine.
func (b *Buffer) Stop() {
	select {
	case <-b.stopCh:
		return // already stopped
	default:
		close(b.stopCh)
	}
	<-b.collectorDone
	b.sendersDone.Wait()
}
