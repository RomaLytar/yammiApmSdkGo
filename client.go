package apm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Client is the HTTP transport that ships payloads to the APM backend.
//
// Design rules after the first load-test experience:
//
//  1. NEVER log payloads. Debug mode logs counters, not bytes. Printing
//     a 500-byte JSON per POST under 1000 POST/s generates 500 KB/s of
//     stderr traffic and the Docker json-file driver serializes that
//     synchronously, stalling the caller goroutine.
//
//  2. Hard timeout 2s. Long timeouts turn "one slow request" into "all
//     sender workers stuck on the same dead backend". A short timeout
//     plus the buffer's internal channel is the correct back-pressure.
//
//  3. No sleep between retries. We retry at most once, immediately, on
//     a transport error. Anything more sophisticated (exponential
//     back-off, circuit breaker) would amplify a slow backend into a
//     slower backend by holding sender workers longer.
//
//  4. Errors are counted, not printed. Printing every failure floods
//     the log when the backend is down. We expose the error count via
//     ErrorCount(); the host app can emit one summary line on a timer
//     if it wants visibility.
type Client struct {
	endpoint string
	token    string
	http     *http.Client
	logger   *log.Logger
	debug    bool

	// Counters for self-observability. Atomic because sender workers
	// update them in parallel.
	successCount atomic.Uint64
	errorCount   atomic.Uint64
	rejectCount  atomic.Uint64 // 4xx from backend — token wrong, schema bad
}

// NewClient builds a Client from a Config. The HTTP client is reused
// across all calls so TCP connections are pooled, keeping the per-POST
// cost at "already-open socket" instead of "new TCP handshake".
func NewClient(cfg Config) *Client {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return &Client{
		endpoint: strings.TrimRight(cfg.Endpoint, "/"),
		token:    cfg.Token,
		http: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				// Pool sized for the typical SenderWorkers=4 plus some
				// headroom for the metrics loop.
				MaxIdleConns:        32,
				MaxIdleConnsPerHost: 16,
				IdleConnTimeout:     90 * time.Second,
				DisableCompression:  true, // JSON compresses nicely but gzip CPU is not free
			},
		},
		logger: log.New(log.Writer(), "[apm] ", log.LstdFlags),
		debug:  cfg.Debug,
	}
}

// ClientStats is a snapshot of the counters the Client exposes.
type ClientStats struct {
	Success uint64
	Errors  uint64
	Reject  uint64
}

// Stats returns a snapshot of the counters. Safe from any goroutine.
func (c *Client) Stats() ClientStats {
	return ClientStats{
		Success: c.successCount.Load(),
		Errors:  c.errorCount.Load(),
		Reject:  c.rejectCount.Load(),
	}
}

// SendEvents posts a batch of HTTP/gRPC traces to /ingest.
func (c *Client) SendEvents(ctx context.Context, events []Event) {
	if len(events) == 0 {
		return
	}
	c.send(ctx, c.endpoint, map[string]any{"events": events}, "events")
}

// SendSystemMetric posts a single system snapshot to /ingest/system.
func (c *Client) SendSystemMetric(ctx context.Context, m SystemMetric) {
	c.send(ctx, c.url("/ingest/system"), map[string]any{
		"metrics": []SystemMetric{m},
	}, "system")
}

// SendDBMetrics posts detailed DB metrics to /ingest/db-metrics.
func (c *Client) SendDBMetrics(ctx context.Context, metrics []DBMetric) {
	if len(metrics) == 0 {
		return
	}
	c.send(ctx, c.url("/ingest/db-metrics"), map[string]any{"metrics": metrics}, "db")
}

// SendRedisMetrics posts detailed Redis metrics to /ingest/redis-metrics.
func (c *Client) SendRedisMetrics(ctx context.Context, metrics []RedisMetric) {
	if len(metrics) == 0 {
		return
	}
	c.send(ctx, c.url("/ingest/redis-metrics"), map[string]any{"metrics": metrics}, "redis")
}

// SendFPMMetrics posts PHP-FPM metrics to /ingest/fpm-metrics.
func (c *Client) SendFPMMetrics(ctx context.Context, metrics []FPMMetric) {
	if len(metrics) == 0 {
		return
	}
	c.send(ctx, c.url("/ingest/fpm-metrics"), map[string]any{"metrics": metrics}, "fpm")
}

// SendNginxMetrics posts nginx metrics to /ingest/nginx-metrics.
func (c *Client) SendNginxMetrics(ctx context.Context, metrics []NginxMetric) {
	if len(metrics) == 0 {
		return
	}
	c.send(ctx, c.url("/ingest/nginx-metrics"), map[string]any{"metrics": metrics}, "nginx")
}

// SendQueueJobs posts queue job lifecycle events to /ingest/queue.
func (c *Client) SendQueueJobs(ctx context.Context, jobs []QueueJob) {
	if len(jobs) == 0 {
		return
	}
	c.send(ctx, c.url("/ingest/queue"), map[string]any{"jobs": jobs}, "queue")
}

// LogCustomMetrics is a temporary sink for Group D (custom Prometheus
// metrics). The backend does not have a typed endpoint yet, so we just
// count them and optionally log a one-line summary when debug is on.
// When /ingest/custom lands, swap this for a real POST.
func (c *Client) LogCustomMetrics(metrics []CustomMetric) {
	if len(metrics) == 0 {
		return
	}
	if c.debug {
		c.logger.Printf("custom metrics: %d items buffered (sink not implemented)", len(metrics))
	}
}

// url builds an absolute URL for an /ingest sub-path. The base endpoint
// may be configured with or without a trailing /ingest — both work.
func (c *Client) url(path string) string {
	base := strings.TrimSuffix(c.endpoint, "/ingest")
	return base + path
}

// send is the shared transport. It performs at most one retry on
// transport errors and never sleeps between attempts. Errors are
// counted, not printed per-event — only a summary line is emitted
// every 30s when debug is on (see APM.metricsLoop).
func (c *Client) send(ctx context.Context, url string, payload any, label string) {
	if c.token == "" {
		return // no token = explicit opt-out
	}

	body, err := json.Marshal(payload)
	if err != nil {
		c.errorCount.Add(1)
		return
	}

	// Two attempts total: one real request, one retry on transport error.
	// No sleep between them — 50ms back-off would tie up the sender
	// goroutine for nothing when the pool is already saturated.
	for attempt := 0; attempt < 2; attempt++ {
		status, err := c.do(ctx, url, body)
		if err == nil && status >= 200 && status < 300 {
			c.successCount.Add(1)
			return
		}
		if err == nil && status >= 400 && status < 500 {
			// 4xx means the backend rejected us — wrong token, bad
			// schema. Retrying won't help. Count and give up.
			c.rejectCount.Add(1)
			return
		}
		// 5xx or transport error: try once more, then count as error.
	}
	c.errorCount.Add(1)
}

// do performs one HTTP POST with the auth header set.
// The request carries a per-call context with the configured timeout
// so a single stuck connection does not hold a sender worker hostage.
func (c *Client) do(ctx context.Context, url string, body []byte) (int, error) {
	reqCtx, cancel := context.WithTimeout(ctx, c.http.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-APM-Token", c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	// Drain and discard the body so the connection can be reused by
	// the HTTP/1.1 connection pool. We read a bounded amount to avoid
	// a pathological backend keeping the connection open with a huge
	// response.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, nil
}

// logStatsLine is called from the metrics loop to print one summary
// line per metrics interval when debug is on. It is the only form of
// SDK self-logging that stays enabled under load.
func (c *Client) logStatsLine(prefix string) {
	if !c.debug {
		return
	}
	s := c.Stats()
	c.logger.Printf("%s client: sent=%d errors=%d reject=%d",
		prefix, s.Success, s.Errors, s.Reject)
}
