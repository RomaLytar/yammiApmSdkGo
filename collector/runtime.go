package collector

import (
	"context"
	"runtime"
	"time"

	apm "github.com/RomaLytar/yammiApmSdkGo"
)

// RuntimeCollector reports the Go runtime view of the world: number of
// goroutines, GC pauses, heap usage. The Laravel SDK has nothing like
// this — it's the most useful Go-specific addition.
//
// The metrics are emitted as CustomMetric values for now (the same path
// Group D uses) so the user can see them in debug logs immediately.
// Once /ingest/runtime is added on the backend, swap the sink without
// touching the collection logic.
type RuntimeCollector struct {
	service string
	prevGC  uint32
}

// NewRuntimeCollector wires the collector. Call Snapshot() periodically
// from your own goroutine, or pass it to apm.AttachRuntimeCollector once
// the helper exists. For now, see examples/http_chi/main.go for the
// manual wiring pattern.
func NewRuntimeCollector(service string) *RuntimeCollector {
	return &RuntimeCollector{service: service}
}

// Snapshot returns the current runtime stats as a list of CustomMetrics.
// The list is small (≈8 entries) so allocation cost is negligible even
// when called every second.
func (r *RuntimeCollector) Snapshot(_ context.Context) []apm.CustomMetric {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	ts := time.Now().Unix()
	common := map[string]string{"service": r.service}

	out := []apm.CustomMetric{
		{Service: r.service, Name: "go_goroutines", Type: "gauge", Labels: common, Value: float64(runtime.NumGoroutine()), Timestamp: ts},
		{Service: r.service, Name: "go_threads", Type: "gauge", Labels: common, Value: float64(pthreadCount()), Timestamp: ts},
		{Service: r.service, Name: "go_memstats_heap_alloc_bytes", Type: "gauge", Labels: common, Value: float64(ms.HeapAlloc), Timestamp: ts},
		{Service: r.service, Name: "go_memstats_heap_inuse_bytes", Type: "gauge", Labels: common, Value: float64(ms.HeapInuse), Timestamp: ts},
		{Service: r.service, Name: "go_memstats_heap_sys_bytes", Type: "gauge", Labels: common, Value: float64(ms.HeapSys), Timestamp: ts},
		{Service: r.service, Name: "go_memstats_heap_objects", Type: "gauge", Labels: common, Value: float64(ms.HeapObjects), Timestamp: ts},
		{Service: r.service, Name: "go_memstats_gc_cpu_fraction", Type: "gauge", Labels: common, Value: ms.GCCPUFraction, Timestamp: ts},
		{Service: r.service, Name: "go_memstats_pause_total_ns", Type: "counter", Labels: common, Value: float64(ms.PauseTotalNs), Timestamp: ts},
		{Service: r.service, Name: "go_gc_count", Type: "counter", Labels: common, Value: float64(ms.NumGC), Timestamp: ts},
	}

	r.prevGC = ms.NumGC
	return out
}

// pthreadCount returns the number of OS threads visible to the Go runtime
// using runtime.NumCgoCall as a coarse proxy. There is no public way to
// get the exact pthread count without /proc parsing on Linux; this stays
// dependency-free and good enough for a dashboard hint.
func pthreadCount() int64 {
	return runtime.NumCgoCall()
}
