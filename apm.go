// Package apm is the Go SDK for the Yammi APM backend.
//
// It collects three things and ships them to an APM ingest endpoint:
//
//  1. Request traces — via HTTP middleware or gRPC interceptors,
//  2. System / DB / Redis health snapshots — on a fixed interval,
//  3. Optionally, metrics from a co-located Prometheus exposition or a
//     Prometheus HTTP API ("Group D" custom app metrics).
//
// Typical usage:
//
//	import (
//	    apm "github.com/RomaLytar/yammiApmSdkGo"
//	    apmmw "github.com/RomaLytar/yammiApmSdkGo/middleware"
//	)
//
//	cfg := apm.LoadConfigFromEnv()
//	cfg.ServiceName = "board"
//	a, err := apm.New(cfg)
//	if err != nil { log.Fatal(err) }
//	defer a.Shutdown(context.Background())
//
//	// HTTP:
//	mux := chi.NewRouter()
//	mux.Use(apmmw.HTTP(a))
//
//	// gRPC:
//	srv := grpc.NewServer(grpc.UnaryInterceptor(apmmw.GRPCUnary(a)))
//
// All collection happens off the request hot path: traces are buffered
// and shipped from a goroutine, system metrics run on their own ticker,
// and the Prometheus scraper (if enabled) shares the metrics ticker.
package apm

import (
	"context"
	"errors"
	"sync"
	"time"
)

// APM is the runtime entry point. Construct it with New, register its
// HTTP middleware or gRPC interceptors on your server, and call Shutdown
// during graceful termination so the last batch is not lost.
type APM struct {
	cfg    Config
	client *Client
	buffer *Buffer

	// Pluggable collectors. Any of these may be nil if the user did not
	// wire one in. The metrics loop simply skips nil entries.
	system *SystemCollector
	dbs    []DBCollector
	redis  []RedisCollector
	prom   *PrometheusCollector

	// autoCloseFns holds close functions for resources the SDK opened
	// itself during auto-discovery (dedicated *sql.DB, redis.Client).
	// Shutdown() calls them so the SDK does not leak connections.
	autoCloseFns []func()

	stopCh chan struct{}
	doneCh chan struct{}
	once   sync.Once
}

// New initialises the SDK with the given config. If cfg.Enabled is false,
// New returns a no-op APM that satisfies the same API but does nothing
// — safe to embed in tests or local dev where you don't want network calls.
func New(cfg Config) (*APM, error) {
	if cfg.ServiceName == "" {
		return nil, errors.New("apm: ServiceName is required")
	}
	if !cfg.Enabled {
		return &APM{cfg: cfg}, nil
	}

	client := NewClient(cfg)
	a := &APM{
		cfg:    cfg,
		client: client,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
	a.buffer = NewBuffer(cfg, client.SendEvents)
	a.system = NewSystemCollector(cfg)

	if cfg.Prometheus.Enabled {
		a.prom = NewPrometheusCollector(cfg.Prometheus)
	}

	// Auto-discover DB and Redis based on env vars (DATABASE_URL /
	// REDIS_URL or APM-specific overrides). This is what makes the
	// SDK truly zero-code: the host app imports apm and calls New,
	// and everything else is derived from existing env.
	a.autoDiscover()

	go a.metricsLoop()
	return a, nil
}

// AttachDBCollector wires a database collector. The SDK calls Collect on
// it from the metrics loop and ships the result to /ingest/db-metrics.
// You can attach more than one (e.g. one per logical database).
func (a *APM) AttachDBCollector(c DBCollector) {
	if a == nil || a.client == nil {
		return
	}
	a.dbs = append(a.dbs, c)
}

// AttachRedisCollector wires a redis collector. Same model as DB.
func (a *APM) AttachRedisCollector(c RedisCollector) {
	if a == nil || a.client == nil {
		return
	}
	a.redis = append(a.redis, c)
}

// PushEvent enqueues a custom trace event. Most users will not call this
// directly — use the HTTP middleware or gRPC interceptor instead.
func (a *APM) PushEvent(e Event) {
	if a == nil || a.buffer == nil {
		return
	}
	a.buffer.Push(e)
}

// Client returns the underlying transport. Useful for advanced users who
// want to send batches directly (e.g. queue jobs from a custom worker).
func (a *APM) Client() *Client {
	return a.client
}

// Config returns the loaded configuration.
func (a *APM) Config() Config {
	return a.cfg
}

// Shutdown drains the buffer, runs one final metrics collection and stops
// background goroutines. Safe to call multiple times.
func (a *APM) Shutdown(ctx context.Context) {
	if a == nil || a.client == nil {
		return
	}
	a.once.Do(func() {
		close(a.stopCh)
		<-a.doneCh
		if a.buffer != nil {
			a.buffer.Stop()
		}
		// Close any SDK-owned resources created by auto-discovery
		// (dedicated *sql.DB, redis.Client). Done LAST so the final
		// metrics collection in metricsLoop can still use them.
		for _, fn := range a.autoCloseFns {
			fn()
		}
	})
}

// metricsLoop runs the periodic system / db / redis / prom collection.
// It is the equivalent of Laravel's terminating() throttle, but instead
// of riding on the request lifecycle it has its own ticker.
func (a *APM) metricsLoop() {
	defer close(a.doneCh)

	interval := a.cfg.MetricsInterval
	if interval <= 0 {
		interval = 60 * time.Second
	}

	// First collection happens immediately so the dashboard has a value
	// before the first interval elapses.
	a.collectAndShip(context.Background())

	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-t.C:
			a.collectAndShip(context.Background())
		case <-a.stopCh:
			a.collectAndShip(context.Background())
			return
		}
	}
}

// collectAndShip runs every collector and posts the results. Each call is
// independent — a failure in one collector never blocks the others.
//
// It also emits a one-line SDK self-stats summary when cfg.Debug is on.
// These numbers (events sent, dropped, sender errors) are the only
// log output the SDK produces under load. Printing payloads was the
// original debug mode and it cost us the first load test — never again.
func (a *APM) collectAndShip(ctx context.Context) {
	if a.system != nil {
		snap := a.system.Collect(a.prom)
		a.client.SendSystemMetric(ctx, snap)
	}

	for _, c := range a.dbs {
		if m, ok := c.Collect(ctx); ok {
			a.client.SendDBMetrics(ctx, []DBMetric{m})
		}
	}

	for _, c := range a.redis {
		if m, ok := c.Collect(ctx); ok {
			a.client.SendRedisMetrics(ctx, []RedisMetric{m})
		}
	}

	if a.prom != nil {
		if customs := a.prom.CollectCustom(a.cfg.ServiceName); len(customs) > 0 {
			a.client.LogCustomMetrics(customs)
		}
	}

	if a.cfg.Debug && a.buffer != nil {
		bs := a.buffer.Stats()
		cs := a.client.Stats()
		a.client.logger.Printf("stats service=%s sent=%d errors=%d reject=%d dropped_events=%d dropped_batches=%d queued_events=%d queued_batches=%d",
			a.cfg.ServiceName, cs.Success, cs.Errors, cs.Reject,
			bs.DroppedEvents, bs.DroppedBatches, bs.QueuedEvents, bs.QueuedBatches)
	}
}
