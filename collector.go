package apm

import "context"

// DBCollector returns a snapshot of database metrics. ok=false means
// the collector chose to skip this round (e.g. dependency not ready).
//
// The SDK ships with NewPostgresCollector / NewMySQLCollector under the
// collector/ subdirectory, but anyone can implement this interface for
// a custom data source — the SDK never assumes a particular driver.
type DBCollector interface {
	Collect(ctx context.Context) (DBMetric, bool)
}

// RedisCollector mirrors DBCollector but for Redis. The shipped
// implementation lives in collector/redis.go and works with any client
// that exposes an INFO command.
type RedisCollector interface {
	Collect(ctx context.Context) (RedisMetric, bool)
}
