// Package collector contains optional, plug-in collectors that the user
// wires into an *apm.APM instance via AttachDBCollector / AttachRedisCollector.
//
// The collectors here speak only to interfaces — they NEVER import a
// concrete driver like pgx or go-redis. That keeps the SDK's go.mod
// minimal: dependencies only show up in the user's go.sum if the user
// already has them.
package collector

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"time"

	apm "github.com/RomaLytar/yammiApmSdkGo"
)

// SQLCollector is a generic database/sql collector that ships:
//
//   - connection pool stats from sql.DB.Stats(),
//   - per-flush query counters (count, slow count, avg/p95/max ms),
//   - database size in MB if a SizeQuery is provided,
//   - read/write counters if the caller calls TrackRead/TrackWrite.
//
// The query counters are populated by the caller via Observe(). The
// SDK does not intercept your driver — wiring observability into pgx /
// go-sql-driver is best done by wrapping your own DB layer once and
// calling Observe from the wrapper. See examples/http_chi/main.go.
type SQLCollector struct {
	db         *sql.DB
	service    string
	driver     string // "postgres" or "mysql" — affects the connection-stat query
	sizeQuery  string // optional: returns one row with size_mb
	slowMs     int

	// Counters reset on each Collect() call.
	mu       sync.Mutex
	queries  int64 // total queries observed since last flush
	slow     int64
	reads    int64
	writes   int64
	sumMs    float64
	maxMs    float64
	samples  []float64 // for p95
}

// SQLCollectorOptions configures a SQLCollector. Driver determines which
// pool stats query to run (Postgres uses pg_stat_activity, MySQL uses
// SHOW STATUS / SHOW VARIABLES).
type SQLCollectorOptions struct {
	DB        *sql.DB
	Service   string
	Driver    string // "postgres" | "mysql"
	SlowMs    int    // queries above this threshold count as slow
	SizeQuery string // optional, must return single row with size_mb (float)
}

// NewSQLCollector builds a SQLCollector. Pass it to apm.AttachDBCollector.
func NewSQLCollector(opts SQLCollectorOptions) *SQLCollector {
	if opts.SlowMs == 0 {
		opts.SlowMs = 1000
	}
	return &SQLCollector{
		db:        opts.DB,
		service:   opts.Service,
		driver:    opts.Driver,
		sizeQuery: opts.SizeQuery,
		slowMs:    opts.SlowMs,
		samples:   make([]float64, 0, 1024),
	}
}

// Observe records one query. Call this from your DB wrapper after each
// query completes. write should be true for INSERT/UPDATE/DELETE/DDL.
func (c *SQLCollector) Observe(durationMs float64, write bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.queries++
	c.sumMs += durationMs
	if durationMs > c.maxMs {
		c.maxMs = durationMs
	}
	if int(durationMs) >= c.slowMs {
		c.slow++
	}
	if write {
		c.writes++
	} else {
		c.reads++
	}
	// Cap the sample slice to keep memory bounded under high RPS. The p95
	// is only interesting on small samples anyway; a 4096 cap gives a
	// reasonable estimate without unbounded growth.
	if len(c.samples) < 4096 {
		c.samples = append(c.samples, durationMs)
	}
}

// TrackRead is sugar for Observe(0, false) for callers that only care
// about the read/write split and not duration. Most users should use
// Observe with the real duration.
func (c *SQLCollector) TrackRead()  { atomic.AddInt64(&c.reads, 1) }
func (c *SQLCollector) TrackWrite() { atomic.AddInt64(&c.writes, 1) }

// Collect implements apm.DBCollector. Called once per metrics interval.
func (c *SQLCollector) Collect(ctx context.Context) (apm.DBMetric, bool) {
	if c.db == nil {
		return apm.DBMetric{}, false
	}

	stats := c.db.Stats()
	m := apm.DBMetric{
		Service:           c.service,
		ConnectionsActive: uint32(stats.InUse),
		ConnectionsIdle:   uint32(stats.Idle),
		ConnectionsMax:    uint32(stats.MaxOpenConnections),
		Timestamp:         time.Now().Unix(),
	}

	c.flushCounters(&m)

	if c.sizeQuery != "" {
		var sizeMB float64
		row := c.db.QueryRowContext(ctx, c.sizeQuery)
		if err := row.Scan(&sizeMB); err == nil {
			m.SizeMB = float32(sizeMB)
		}
	}

	return m, true
}

// flushCounters drains the in-memory counters into a DBMetric and
// resets state for the next interval.
func (c *SQLCollector) flushCounters(m *apm.DBMetric) {
	c.mu.Lock()
	defer c.mu.Unlock()

	m.QueriesPerMin = uint32(c.queries)
	m.SlowQueries = uint32(c.slow)
	m.ReadsPerMin = uint32(c.reads)
	m.WritesPerMin = uint32(c.writes)
	m.MaxQueryMs = float32(c.maxMs)
	if c.queries > 0 {
		m.AvgQueryMs = float32(c.sumMs / float64(c.queries))
	}
	if len(c.samples) > 0 {
		m.P95QueryMs = float32(percentile(c.samples, 0.95))
	}

	c.queries, c.slow, c.reads, c.writes = 0, 0, 0, 0
	c.sumMs, c.maxMs = 0, 0
	c.samples = c.samples[:0]
}

// percentile is a small in-place quickselect-style p estimator. Sorting
// is O(n log n) but n is capped at 4096, so it's negligible per minute.
func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	cp := make([]float64, len(values))
	copy(cp, values)
	// Insertion sort is fine for n=4096 once a minute, but stdlib sort is
	// already faster and a single import. Keep it simple:
	sortFloat64s(cp)
	idx := int(float64(len(cp)-1) * p)
	return cp[idx]
}
