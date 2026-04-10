package apm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// Client is the HTTP transport that ships payloads to the APM backend.
//
// It mirrors the semantics of the Laravel SDK's Client.php:
//   - 1 retry on transport / 5xx errors with a 50ms back-off,
//   - 4xx is logged once and never retried,
//   - empty token short-circuits to a no-op,
//   - failures never bubble up to the caller (APM must not break the app).
type Client struct {
	endpoint string
	token    string
	http     *http.Client
	logger   *log.Logger
	debug    bool
}

// NewClient builds a Client from a Config. The HTTP client is reused for
// all calls so connections to the ingest backend are pooled.
func NewClient(cfg Config) *Client {
	return &Client{
		endpoint: strings.TrimRight(cfg.Endpoint, "/"),
		token:    cfg.Token,
		http: &http.Client{
			Timeout: cfg.Timeout,
			Transport: &http.Transport{
				MaxIdleConns:        20,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		logger: log.New(log.Writer(), "[apm] ", log.LstdFlags),
		debug:  cfg.Debug,
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
// The backend wraps single payloads in an array on its side, but this
// method always batches into a one-element array to match the existing
// Laravel client's wire format.
func (c *Client) SendSystemMetric(ctx context.Context, m SystemMetric) {
	c.send(ctx, c.url("/ingest/system"), map[string]any{
		"metrics": []SystemMetric{m},
	}, "system metrics")
}

// SendDBMetrics posts detailed DB metrics to /ingest/db-metrics.
func (c *Client) SendDBMetrics(ctx context.Context, metrics []DBMetric) {
	if len(metrics) == 0 {
		return
	}
	c.send(ctx, c.url("/ingest/db-metrics"), map[string]any{
		"metrics": metrics,
	}, "db metrics")
}

// SendRedisMetrics posts detailed Redis metrics to /ingest/redis-metrics.
func (c *Client) SendRedisMetrics(ctx context.Context, metrics []RedisMetric) {
	if len(metrics) == 0 {
		return
	}
	c.send(ctx, c.url("/ingest/redis-metrics"), map[string]any{
		"metrics": metrics,
	}, "redis metrics")
}

// SendFPMMetrics posts PHP-FPM metrics to /ingest/fpm-metrics.
func (c *Client) SendFPMMetrics(ctx context.Context, metrics []FPMMetric) {
	if len(metrics) == 0 {
		return
	}
	c.send(ctx, c.url("/ingest/fpm-metrics"), map[string]any{
		"metrics": metrics,
	}, "fpm metrics")
}

// SendNginxMetrics posts nginx metrics to /ingest/nginx-metrics.
func (c *Client) SendNginxMetrics(ctx context.Context, metrics []NginxMetric) {
	if len(metrics) == 0 {
		return
	}
	c.send(ctx, c.url("/ingest/nginx-metrics"), map[string]any{
		"metrics": metrics,
	}, "nginx metrics")
}

// SendQueueJobs posts queue job lifecycle events to /ingest/queue.
func (c *Client) SendQueueJobs(ctx context.Context, jobs []QueueJob) {
	if len(jobs) == 0 {
		return
	}
	c.send(ctx, c.url("/ingest/queue"), map[string]any{
		"jobs": jobs,
	}, "queue jobs")
}

// LogCustomMetrics is a temporary sink for Group D (custom Prometheus
// metrics) until the backend grows a typed endpoint. When debug is on,
// the metrics are dumped to the log so you can verify what would be sent.
func (c *Client) LogCustomMetrics(metrics []CustomMetric) {
	if len(metrics) == 0 || !c.debug {
		return
	}
	for _, m := range metrics {
		c.logger.Printf("custom metric: service=%s name=%s type=%s value=%v labels=%v",
			m.Service, m.Name, m.Type, m.Value, m.Labels)
	}
}

// url builds an absolute URL for an /ingest sub-path. The base endpoint
// may be configured with or without a trailing /ingest — both work.
func (c *Client) url(path string) string {
	base := strings.TrimSuffix(c.endpoint, "/ingest")
	return base + path
}

// send is the shared transport with the same retry policy as Laravel's
// Client.php sendWithRetry: 1 retry on 5xx / transport, 4xx is fatal.
func (c *Client) send(ctx context.Context, url string, payload any, label string) {
	if c.token == "" {
		return // mirror PHP behaviour: no token = no-op
	}

	body, err := json.Marshal(payload)
	if err != nil {
		c.logger.Printf("marshal %s failed: %v", label, err)
		return
	}

	if c.debug {
		c.logger.Printf("POST %s (%d bytes): %s", url, len(body), truncate(body, 500))
	}

	const maxRetries = 1
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
		}

		status, respBody, err := c.do(ctx, url, body)
		if err == nil && status >= 200 && status < 300 {
			return // success
		}
		if err == nil && status >= 400 && status < 500 {
			c.logger.Printf("rejected %s: HTTP %d %s", label, status, truncate(respBody, 200))
			return // 4xx, not retriable
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("HTTP %d", status)
		}
	}

	c.logger.Printf("failed to send %s after %d attempts: %v (url=%s)",
		label, maxRetries+1, lastErr, url)
}

// do performs one HTTP POST with the auth header set.
func (c *Client) do(ctx context.Context, url string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-APM-Token", c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, respBody, nil
}

func truncate(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return append(b[:n:n], []byte("...")...)
}
