package collector

import (
	"context"
	"testing"
)

// fakeInfoSource lets us drive the collector without a real Redis.
type fakeInfoSource struct {
	body string
	err  error
}

func (f fakeInfoSource) InfoString(_ context.Context) (string, error) {
	return f.body, f.err
}

func TestRedisCollector_ParsesRealisticInfo(t *testing.T) {
	body := "# Server\r\n" +
		"redis_version:7.2.4\r\n" +
		"\r\n" +
		"# Clients\r\n" +
		"connected_clients:42\r\n" +
		"\r\n" +
		"# Memory\r\n" +
		"used_memory:5242880\r\n" +
		"used_memory_peak:10485760\r\n" +
		"\r\n" +
		"# Stats\r\n" +
		"instantaneous_ops_per_sec:1234\r\n" +
		"keyspace_hits:900\r\n" +
		"keyspace_misses:100\r\n" +
		"evicted_keys:7\r\n"

	c := NewRedisCollector("test", fakeInfoSource{body: body})
	m, ok := c.Collect(context.Background())
	if !ok {
		t.Fatal("expected ok=true")
	}

	if m.Service != "test" {
		t.Errorf("service: got %q, want test", m.Service)
	}
	if m.MemoryUsedMB != 5 {
		t.Errorf("MemoryUsedMB: got %v, want 5", m.MemoryUsedMB)
	}
	if m.MemoryPeakMB != 10 {
		t.Errorf("MemoryPeakMB: got %v, want 10", m.MemoryPeakMB)
	}
	if m.ConnectedClients != 42 {
		t.Errorf("ConnectedClients: got %v, want 42", m.ConnectedClients)
	}
	if m.OpsPerSec != 1234 {
		t.Errorf("OpsPerSec: got %v, want 1234", m.OpsPerSec)
	}
	if m.HitRatio != 90 {
		t.Errorf("HitRatio: got %v, want 90", m.HitRatio)
	}
	if m.EvictedKeys != 7 {
		t.Errorf("EvictedKeys: got %v, want 7", m.EvictedKeys)
	}
}

func TestRedisCollector_NoSourceReturnsFalse(t *testing.T) {
	c := NewRedisCollector("test", nil)
	if _, ok := c.Collect(context.Background()); ok {
		t.Error("expected ok=false when source is nil")
	}
}

func TestRedisCollector_HitRatioZeroDivision(t *testing.T) {
	body := "connected_clients:1\r\nused_memory:0\r\n"
	c := NewRedisCollector("t", fakeInfoSource{body: body})
	m, ok := c.Collect(context.Background())
	if !ok {
		t.Fatal("expected ok")
	}
	if m.HitRatio != 0 {
		t.Errorf("HitRatio with no hits/misses must be 0, got %v", m.HitRatio)
	}
}
