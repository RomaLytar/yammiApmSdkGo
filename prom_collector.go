package apm

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
)

// PrometheusCollector is the Group D + E source. It can:
//
//   - scrape a local /metrics endpoint (Prometheus exposition format)
//     and forward selected metrics as CustomMetric values, and/or
//   - query a Prometheus HTTP API to fill standard SystemMetric fields
//     (host CPU/mem/disk via node_exporter, etc.).
//
// Both modes are independent; either, both or neither may be configured.
type PrometheusCollector struct {
	cfg  PrometheusConfig
	http *http.Client

	// Cached results from the most recent local scrape so the host
	// metrics filler does not double-scrape on the same tick.
	lastScrape    map[string]*dto.MetricFamily
	lastScrapeAt  time.Time
}

// NewPrometheusCollector wires the collector. No I/O happens here.
func NewPrometheusCollector(cfg PrometheusConfig) *PrometheusCollector {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 3 * time.Second
	}
	return &PrometheusCollector{
		cfg: cfg,
		http: &http.Client{
			Timeout: cfg.Timeout,
		},
	}
}

// FillHostMetrics is called by SystemCollector when SourcePolicy.Host
// allows Prometheus. It returns true if at least one host field was
// populated, signalling that the local /proc fallback should be skipped.
//
// Strategy:
//   - If a server URL is configured, run PromQL against node_exporter
//     metrics. PromQL is the right tool here because the user's instance
//     label may differ from the SDK's hostname.
//   - Otherwise, fall back to a local scrape of node_exporter style
//     metric names. Useful when the user runs node_exporter as a sidecar.
func (p *PrometheusCollector) FillHostMetrics(m *SystemMetric) bool {
	filled := false

	// Server-mode: ask Prometheus.
	if p.cfg.ServerURL != "" {
		labelSel := p.labelSelector()

		if v, ok := p.instantQuery(`100 - (avg(rate(node_cpu_seconds_total{mode="idle"` + labelSel + `}[1m])) * 100)`); ok {
			m.ContainerCPUUsagePct = float32(v)
			filled = true
		}
		if v, ok := p.instantQuery(`(node_memory_MemTotal_bytes` + braced(labelSel) + ` - node_memory_MemAvailable_bytes` + braced(labelSel) + `) / 1024 / 1024`); ok {
			m.MemUsedMB = float32(v)
			filled = true
		}
		if v, ok := p.instantQuery(`node_memory_MemTotal_bytes` + braced(labelSel) + ` / 1024 / 1024`); ok {
			m.MemTotalMB = float32(v)
			filled = true
		}
		if v, ok := p.instantQuery(`(node_filesystem_size_bytes{mountpoint="/"` + labelSel + `} - node_filesystem_avail_bytes{mountpoint="/"` + labelSel + `}) / 1024 / 1024`); ok {
			m.DiskUsedMB = float32(v)
			filled = true
		}
		if v, ok := p.instantQuery(`node_filesystem_size_bytes{mountpoint="/"` + labelSel + `} / 1024 / 1024`); ok {
			m.DiskTotalMB = float32(v)
			filled = true
		}
		if v, ok := p.instantQuery(`node_load1` + braced(labelSel)); ok {
			m.CPULoad1 = float32(v)
			filled = true
		}
		if v, ok := p.instantQuery(`node_load5` + braced(labelSel)); ok {
			m.CPULoad5 = float32(v)
			filled = true
		}
		if v, ok := p.instantQuery(`node_load15` + braced(labelSel)); ok {
			m.CPULoad15 = float32(v)
			filled = true
		}
	}

	// Local-mode: scrape /metrics and look for known node_exporter names.
	// Only if server-mode did not already fill the field.
	if p.cfg.LocalURL != "" && !filled {
		families, err := p.scrapeLocal()
		if err == nil {
			if v, ok := gauge(families, "node_load1"); ok {
				m.CPULoad1 = float32(v)
				filled = true
			}
			if v, ok := gauge(families, "node_load5"); ok {
				m.CPULoad5 = float32(v)
			}
			if v, ok := gauge(families, "node_load15"); ok {
				m.CPULoad15 = float32(v)
			}
			if total, ok := gauge(families, "node_memory_MemTotal_bytes"); ok {
				m.MemTotalMB = float32(total / 1024 / 1024)
				if avail, ok2 := gauge(families, "node_memory_MemAvailable_bytes"); ok2 {
					m.MemUsedMB = float32((total - avail) / 1024 / 1024)
				}
				filled = true
			}
		}
	}

	return filled
}

// CollectCustom returns Group D metrics: a snapshot of all metrics from
// the local /metrics endpoint whose name matches one of cfg.CustomMetrics.
// Match is by prefix so "board_grpc_" picks up every histogram bucket
// and counter under that prefix in one config entry.
func (p *PrometheusCollector) CollectCustom(service string) []CustomMetric {
	if p.cfg.LocalURL == "" || len(p.cfg.CustomMetrics) == 0 {
		return nil
	}
	families, err := p.scrapeLocal()
	if err != nil {
		return nil
	}

	now := time.Now().Unix()
	var out []CustomMetric

	for name, fam := range families {
		if !p.matchesCustom(name) {
			continue
		}
		typeName := strings.ToLower(fam.GetType().String())
		for _, met := range fam.GetMetric() {
			labels := make(map[string]string, len(met.GetLabel()))
			for _, lp := range met.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}

			switch fam.GetType() {
			case dto.MetricType_COUNTER:
				out = append(out, CustomMetric{
					Service: service, Name: name, Type: typeName,
					Labels: labels, Value: met.GetCounter().GetValue(),
					Timestamp: now,
				})
			case dto.MetricType_GAUGE:
				out = append(out, CustomMetric{
					Service: service, Name: name, Type: typeName,
					Labels: labels, Value: met.GetGauge().GetValue(),
					Timestamp: now,
				})
			case dto.MetricType_HISTOGRAM:
				h := met.GetHistogram()
				// Emit count + sum as two synthetic metrics so the
				// dashboard can compute averages without parsing buckets.
				out = append(out,
					CustomMetric{
						Service: service, Name: name + "_count", Type: "counter",
						Labels: labels, Value: float64(h.GetSampleCount()),
						Timestamp: now,
					},
					CustomMetric{
						Service: service, Name: name + "_sum", Type: "counter",
						Labels: labels, Value: h.GetSampleSum(),
						Timestamp: now,
					},
				)
			case dto.MetricType_SUMMARY:
				s := met.GetSummary()
				out = append(out,
					CustomMetric{
						Service: service, Name: name + "_count", Type: "counter",
						Labels: labels, Value: float64(s.GetSampleCount()),
						Timestamp: now,
					},
					CustomMetric{
						Service: service, Name: name + "_sum", Type: "counter",
						Labels: labels, Value: s.GetSampleSum(),
						Timestamp: now,
					},
				)
			}
		}
	}
	return out
}

// matchesCustom returns true if the metric name starts with any of the
// configured prefixes. Prefix matching is intentionally permissive so
// the user can write `board_` and pick up the whole family.
func (p *PrometheusCollector) matchesCustom(name string) bool {
	for _, prefix := range p.cfg.CustomMetrics {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// scrapeLocal hits the configured /metrics URL and parses the body using
// the canonical Prometheus exposition parser. Results are cached for the
// duration of one scrape interval to avoid double-fetching when both
// FillHostMetrics and CollectCustom run from the same tick.
func (p *PrometheusCollector) scrapeLocal() (map[string]*dto.MetricFamily, error) {
	if !p.lastScrapeAt.IsZero() && time.Since(p.lastScrapeAt) < 500*time.Millisecond {
		return p.lastScrape, nil
	}

	resp, err := p.http.Get(p.cfg.LocalURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var parser expfmt.TextParser
	families, err := parser.TextToMetricFamilies(strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}

	p.lastScrape = families
	p.lastScrapeAt = time.Now()
	return families, nil
}

// instantQuery runs one PromQL query against the configured server URL
// and returns the first scalar result.
func (p *PrometheusCollector) instantQuery(query string) (float64, bool) {
	endpoint := strings.TrimRight(p.cfg.ServerURL, "/") + "/api/v1/query?query=" + url.QueryEscape(query)
	resp, err := p.http.Get(endpoint)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}

	// The instant-vector response shape is:
	//   { status, data: { resultType: "vector",
	//                     result: [ {metric:{}, value:[ts,"val"]} ] } }
	// `value` is a heterogeneous 2-element array (number, string), so we
	// decode it as []json.RawMessage and pull the string out manually.
	var parsed struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  []json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return 0, false
	}
	if parsed.Status != "success" || len(parsed.Data.Result) == 0 {
		return 0, false
	}

	val := parsed.Data.Result[0].Value
	if len(val) != 2 {
		return 0, false
	}
	var raw string
	if err := json.Unmarshal(val[1], &raw); err != nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// labelSelector returns the comma-prefixed Prometheus label selector
// fragment built from JobLabel / InstanceLabel. The leading comma is
// important because it gets concatenated inside an existing { ... } block.
func (p *PrometheusCollector) labelSelector() string {
	parts := []string{}
	if p.cfg.JobLabel != "" {
		parts = append(parts, `job="`+p.cfg.JobLabel+`"`)
	}
	if p.cfg.InstanceLabel != "" {
		parts = append(parts, `instance="`+p.cfg.InstanceLabel+`"`)
	}
	if len(parts) == 0 {
		return ""
	}
	return "," + strings.Join(parts, ",")
}

// braced wraps a label selector fragment in {} so it can be appended to
// a metric name that has no other labels. If the fragment is empty,
// nothing is added — saves emitting `metric{}` which would be valid but
// uglier in logs.
func braced(sel string) string {
	if sel == "" {
		return ""
	}
	return "{" + strings.TrimPrefix(sel, ",") + "}"
}

// gauge looks up a single-value gauge by name from a parsed family map.
func gauge(families map[string]*dto.MetricFamily, name string) (float64, bool) {
	fam, ok := families[name]
	if !ok || fam.GetType() != dto.MetricType_GAUGE {
		return 0, false
	}
	mets := fam.GetMetric()
	if len(mets) == 0 {
		return 0, false
	}
	return mets[0].GetGauge().GetValue(), true
}

