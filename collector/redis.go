package collector

import (
	"context"
	"strconv"
	"strings"
	"time"

	apm "github.com/RomaLytar/yammiApmSdkGo"
)

// RedisInfoSource is the only thing the collector needs from your Redis
// client: a way to call INFO and get the raw text body. Adapters are
// trivial. For go-redis/v9:
//
//	type adapter struct{ rdb *redis.Client }
//	func (a adapter) InfoString(ctx context.Context) (string, error) {
//	    return a.rdb.Info(ctx).Result()
//	}
//
// For rueidis: client.Do(ctx, client.B().Info().Build()).ToString().
//
// This indirection is what keeps the SDK's go.mod free of any specific
// Redis driver.
type RedisInfoSource interface {
	InfoString(ctx context.Context) (string, error)
}

// RedisCollector parses an INFO response into a typed RedisMetric.
type RedisCollector struct {
	src     RedisInfoSource
	service string
}

// NewRedisCollector wires the collector. service is the logical service
// name that will appear in the dashboard.
func NewRedisCollector(service string, src RedisInfoSource) *RedisCollector {
	return &RedisCollector{src: src, service: service}
}

// Collect implements apm.RedisCollector.
func (c *RedisCollector) Collect(ctx context.Context) (apm.RedisMetric, bool) {
	if c.src == nil {
		return apm.RedisMetric{}, false
	}
	info, err := c.src.InfoString(ctx)
	if err != nil {
		return apm.RedisMetric{}, false
	}

	parsed := parseRedisInfo(info)
	hits := atoi(parsed["keyspace_hits"])
	misses := atoi(parsed["keyspace_misses"])
	hitRatio := float32(0)
	if hits+misses > 0 {
		hitRatio = float32(float64(hits) / float64(hits+misses) * 100)
	}

	return apm.RedisMetric{
		Service:          c.service,
		MemoryUsedMB:     bytesToMB(parsed["used_memory"]),
		MemoryPeakMB:     bytesToMB(parsed["used_memory_peak"]),
		ConnectedClients: uint32(atoi(parsed["connected_clients"])),
		OpsPerSec:        uint32(atoi(parsed["instantaneous_ops_per_sec"])),
		HitRatio:         hitRatio,
		EvictedKeys:      uint64(atoi(parsed["evicted_keys"])),
		Timestamp:        time.Now().Unix(),
	}, true
}

// parseRedisInfo turns the line-oriented INFO body into a flat key→value
// map. Section headers (lines starting with #) are skipped.
func parseRedisInfo(info string) map[string]string {
	out := make(map[string]string, 64)
	for _, line := range strings.Split(info, "\r\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		out[strings.TrimSpace(line[:idx])] = strings.TrimSpace(line[idx+1:])
	}
	return out
}

func atoi(s string) int64 {
	if s == "" {
		return 0
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}

func bytesToMB(s string) float32 {
	v := atoi(s)
	if v == 0 {
		return 0
	}
	return float32(v) / 1024 / 1024
}
