package apm

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all knobs for the APM SDK. Construct it manually or use
// LoadConfigFromEnv() to read APM_* variables (same convention as the
// Laravel SDK so a single .env can drive both).
type Config struct {
	// Enabled — master switch. If false, New() returns a no-op client.
	Enabled bool

	// Endpoint — base URL of the APM ingest API, e.g. http://apm:8890/ingest.
	// Trailing /ingest is stripped automatically when building sub-paths.
	Endpoint string

	// Token — value sent in the X-APM-Token header.
	Token string

	// ServiceName — logical service identifier (board, auth, …).
	ServiceName string

	// FlushInterval — how often the trace buffer is force-flushed even if
	// it has not reached BufferSize.
	FlushInterval time.Duration

	// BufferSize — max number of pending events before an immediate flush.
	BufferSize int

	// MetricsInterval — how often system / db / redis / prom collectors run.
	MetricsInterval time.Duration

	// Timeout — HTTP timeout for ingest requests.
	Timeout time.Duration

	// IgnoreEndpoints — request paths the HTTP middleware should skip.
	IgnoreEndpoints []string

	// SlowQueryThresholdMs — DB query duration above which it counts as slow.
	SlowQueryThresholdMs int

	// Monitor — per-feature toggles. nil pointer = auto-detect.
	Monitor MonitorConfig

	// Prometheus — optional source of metrics that overlap or extend the
	// SDK's own collection. See PrometheusConfig.
	Prometheus PrometheusConfig

	// Debug — when true, every payload is also logged to stderr before
	// being sent. Useful for the first manual smoke-test on yammi.
	Debug bool
}

// MonitorConfig lets the user force a feature on or off. nil = auto.
type MonitorConfig struct {
	DB      *bool
	Redis   *bool
	S3      *bool
	Queue   *bool
	FPM     *bool
	Nginx   *bool
	Runtime *bool // Go runtime metrics (goroutines, GC, heap)
}

// PrometheusConfig configures the optional Prometheus integration.
//
// Two modes are supported and they may be combined:
//
//   - Local scrape: SDK pulls /metrics from the same process or a sidecar.
//     Cheapest, lowest latency, sees only this instance.
//
//   - Server query: SDK runs PromQL against a Prometheus HTTP API. Sees the
//     full fleet but pays one network hop and is bound by scrape_interval.
//
// SourcePolicy controls how Prometheus values interact with values the SDK
// would otherwise compute itself (the "no x2" rule).
type PrometheusConfig struct {
	Enabled bool

	// LocalURL — full URL of a /metrics endpoint to scrape, e.g.
	// "http://localhost:2112/metrics". Empty disables local mode.
	LocalURL string

	// ServerURL — base URL of a Prometheus HTTP API, e.g.
	// "http://prometheus:9090". Empty disables server mode.
	ServerURL string

	// Timeout — HTTP timeout for both modes.
	Timeout time.Duration

	// JobLabel / InstanceLabel — used to fill in default PromQL templates
	// (e.g. {job="$JOB",instance="$INSTANCE"}).
	JobLabel      string
	InstanceLabel string

	// SourcePolicy decides who wins per metric group when both SDK and
	// Prometheus could provide a value.
	//
	//   - SourceAuto:        if Prometheus is enabled and reachable, it wins
	//                        and the SDK skips its own collection for that
	//                        group entirely. This is what kills duplicates.
	//   - SourceSDK:         the SDK always collects, Prometheus is ignored.
	//   - SourcePrometheus:  Prometheus is the only source; if unavailable
	//                        the field stays at its zero value.
	SourcePolicy SourcePolicy

	// CustomMetrics — list of metric names (or substrings) the local scraper
	// should collect verbatim and forward as Group D ("custom app metrics").
	// Example: []string{"board_grpc_requests_total", "board_events_"}
	// An empty list disables Group D collection.
	CustomMetrics []string
}

// SourcePolicy controls overlap resolution between SDK collectors and Prometheus.
type SourcePolicy struct {
	Host      Source // cpu/mem/disk/load/container
	DBHealth  Source // db_ok, db_ping_ms
	Redis     Source // redis_ok + detailed (memory, ops, hit ratio)
	FPM       Source // fpm metrics
	Nginx     Source // nginx metrics
	// Note: traces, jobs, slow queries, S3 are SDK-only by design and have
	// no toggle here — Prometheus does not know them.
}

// Source enumerates where a metric group comes from.
type Source int

const (
	SourceAuto Source = iota
	SourceSDK
	SourcePrometheus
)

// LoadConfigFromEnv reads APM_* variables. Anything missing falls back to
// a sane default. The full list mirrors the Laravel SDK's apm.php.
func LoadConfigFromEnv() Config {
	cfg := Config{
		Enabled:              envBool("APM_ENABLED", true),
		Endpoint:             envStr("APM_ENDPOINT", "http://localhost:8890/ingest"),
		Token:                envStr("APM_TOKEN", ""),
		ServiceName:          envStr("APM_SERVICE_NAME", "default"),
		FlushInterval:        time.Duration(envInt("APM_FLUSH_INTERVAL", 5)) * time.Second,
		BufferSize:           envInt("APM_BUFFER_SIZE", 100),
		MetricsInterval:      time.Duration(envInt("APM_METRICS_INTERVAL", 60)) * time.Second,
		Timeout:              time.Duration(envInt("APM_TIMEOUT", 5)) * time.Second,
		IgnoreEndpoints:      envList("APM_IGNORE_ENDPOINTS", []string{"/health", "/favicon.ico"}),
		SlowQueryThresholdMs: envInt("APM_SLOW_QUERY_THRESHOLD_MS", 1000),
		Debug:                envBool("APM_DEBUG", false),

		Prometheus: PrometheusConfig{
			Enabled:       envBool("APM_PROMETHEUS_ENABLED", false),
			LocalURL:      envStr("APM_PROMETHEUS_LOCAL_URL", ""),
			ServerURL:     envStr("APM_PROMETHEUS_URL", ""),
			Timeout:       time.Duration(envInt("APM_PROMETHEUS_TIMEOUT", 3)) * time.Second,
			JobLabel:      envStr("APM_PROMETHEUS_JOB", ""),
			InstanceLabel: envStr("APM_PROMETHEUS_INSTANCE", ""),
			CustomMetrics: envList("APM_PROMETHEUS_CUSTOM_METRICS", nil),
			// SourcePolicy fields default to SourceAuto, which is the right
			// behaviour: Prometheus wins where it can, SDK fills the rest.
		},
	}
	return cfg
}

func envStr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func envInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envList(key string, def []string) []string {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
