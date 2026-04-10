package apm

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const sampleMetrics = `# HELP board_grpc_requests_total Total gRPC requests by method and code
# TYPE board_grpc_requests_total counter
board_grpc_requests_total{method="Create",code="OK"} 42
board_grpc_requests_total{method="Get",code="OK"} 17
board_grpc_requests_total{method="Get",code="NOT_FOUND"} 3

# HELP board_grpc_request_duration_seconds gRPC request duration
# TYPE board_grpc_request_duration_seconds histogram
board_grpc_request_duration_seconds_bucket{method="Create",le="0.005"} 30
board_grpc_request_duration_seconds_bucket{method="Create",le="+Inf"} 42
board_grpc_request_duration_seconds_sum{method="Create"} 1.234
board_grpc_request_duration_seconds_count{method="Create"} 42

# HELP node_load1 1m load average
# TYPE node_load1 gauge
node_load1 0.42

# HELP unrelated_metric should not appear in custom output
# TYPE unrelated_metric gauge
unrelated_metric 99
`

func TestPrometheusCollector_CollectCustom_PrefixMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sampleMetrics))
	}))
	defer srv.Close()

	p := NewPrometheusCollector(PrometheusConfig{
		Enabled:       true,
		LocalURL:      srv.URL + "/metrics",
		CustomMetrics: []string{"board_grpc_"},
	})

	customs := p.CollectCustom("board")
	if len(customs) == 0 {
		t.Fatal("expected at least one custom metric")
	}

	// Counter family: 3 series (method×code combinations).
	counterCount := 0
	for _, c := range customs {
		if c.Name == "board_grpc_requests_total" && c.Type == "counter" {
			counterCount++
		}
	}
	if counterCount != 3 {
		t.Errorf("expected 3 counter series, got %d", counterCount)
	}

	// Histogram family: emitted as _count and _sum (one each per series).
	hasCount, hasSum := false, false
	for _, c := range customs {
		if strings.HasSuffix(c.Name, "_count") {
			hasCount = true
		}
		if strings.HasSuffix(c.Name, "_sum") {
			hasSum = true
		}
	}
	if !hasCount || !hasSum {
		t.Errorf("histogram should produce _count and _sum entries: count=%v sum=%v", hasCount, hasSum)
	}

	// node_load1 must NOT be in custom output — its prefix doesn't match.
	for _, c := range customs {
		if strings.HasPrefix(c.Name, "node_load") || c.Name == "unrelated_metric" {
			t.Errorf("non-matching metric leaked into custom output: %s", c.Name)
		}
	}
}

func TestPrometheusCollector_FillHostMetrics_LocalNodeExporter(t *testing.T) {
	body := `# TYPE node_load1 gauge
node_load1 1.5
# TYPE node_load5 gauge
node_load5 1.2
# TYPE node_load15 gauge
node_load15 1.1
# TYPE node_memory_MemTotal_bytes gauge
node_memory_MemTotal_bytes 8589934592
# TYPE node_memory_MemAvailable_bytes gauge
node_memory_MemAvailable_bytes 4294967296
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	p := NewPrometheusCollector(PrometheusConfig{
		Enabled:  true,
		LocalURL: srv.URL + "/metrics",
	})

	var m SystemMetric
	if !p.FillHostMetrics(&m) {
		t.Fatal("expected FillHostMetrics to populate something")
	}

	if m.CPULoad1 != 1.5 {
		t.Errorf("CPULoad1: got %v, want 1.5", m.CPULoad1)
	}
	// 8 GB total, 4 GB available → 4 GB used → 4096 MB.
	if m.MemTotalMB != 8192 {
		t.Errorf("MemTotalMB: got %v, want 8192", m.MemTotalMB)
	}
	if m.MemUsedMB != 4096 {
		t.Errorf("MemUsedMB: got %v, want 4096", m.MemUsedMB)
	}
}

func TestPrometheusCollector_LabelSelector(t *testing.T) {
	cases := []struct {
		job, instance, want string
	}{
		{"", "", ""},
		{"board", "", `,job="board"`},
		{"", "board-0", `,instance="board-0"`},
		{"board", "board-0", `,job="board",instance="board-0"`},
	}
	for _, tc := range cases {
		p := NewPrometheusCollector(PrometheusConfig{
			JobLabel:      tc.job,
			InstanceLabel: tc.instance,
		})
		got := p.labelSelector()
		if got != tc.want {
			t.Errorf("job=%q instance=%q: got %q, want %q", tc.job, tc.instance, got, tc.want)
		}
	}
}
