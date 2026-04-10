package apm

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	// Database drivers — registered via side-effect import so the SDK
	// can open Postgres or MySQL connections without forcing the host
	// app to import a driver itself. These drivers are the reason the
	// SDK can be "zero-code integration" on the consumer side.
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/redis/go-redis/v9"
)

// autoDiscover wires the database and redis collectors on cfg basis
// without touching the host application. It reads standard connection
// strings — DATABASE_URL, REDIS_URL — or APM-specific overrides, opens
// its OWN pooled connections (one per resource), and attaches the
// matching collectors. The host app does nothing; adding the SDK to
// main.go and setting APM_TOKEN is the entire onboarding.
//
// Why dedicated connections and not reusing the app's pool? The APM
// collector needs at most one connection per metrics interval (every
// 60s by default). Reusing the app pool would steal a slot from real
// traffic. A dedicated connection limited to MaxOpenConns=2 is
// cheaper AND more isolated — INFO/SELECT queries from the collector
// cannot contend with the hot path.
func (a *APM) autoDiscover() {
	if a == nil || a.client == nil || !a.cfg.Enabled {
		return
	}

	// DB auto-discovery.
	if !a.cfg.DisableAutoDB {
		if dbCol, closer := tryBuildAutoDBCollector(a.cfg); dbCol != nil {
			a.AttachDBCollector(dbCol)
			a.autoCloseFns = append(a.autoCloseFns, closer)
		}
	}

	// Redis auto-discovery.
	if !a.cfg.DisableAutoRedis {
		if redisCol, closer := tryBuildAutoRedisCollector(a.cfg); redisCol != nil {
			a.AttachRedisCollector(redisCol)
			a.autoCloseFns = append(a.autoCloseFns, closer)
		}
	}
}

// tryBuildAutoDBCollector parses the configured URL, detects the SQL
// flavour by scheme, opens a dedicated *sql.DB with small pool
// settings, and returns a collector.SQLCollector-equivalent plus its
// Close function. Returns (nil, nil) on any step failure — auto-
// discovery is best-effort and must never block startup.
func tryBuildAutoDBCollector(cfg Config) (DBCollector, func()) {
	raw := firstNonEmpty(cfg.APMDatabaseURL, cfg.DatabaseURL)
	if raw == "" {
		return nil, nil
	}

	driver, dsn, sizeQuery, err := parseDatabaseURL(raw)
	if err != nil {
		return nil, nil // bad URL, don't crash the host app
	}

	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, nil
	}
	// Dedicated, tiny pool — the collector only needs one connection
	// per metrics interval. A 2-connection ceiling keeps idle overhead
	// negligible and prevents the collector from starving the host
	// app's own pool when they connect to the same PgBouncer.
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	db.SetConnMaxIdleTime(5 * time.Minute)

	// Try a ping with a short timeout. If the database isn't reachable
	// at startup we still attach the collector — it might come up
	// later — but we short-circuit if the URL is malformed on the
	// driver level.
	pingCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = db.PingContext(pingCtx)
	cancel()

	collector := newAutoSQLCollector(db, cfg.ServiceName, driver, sizeQuery, cfg.SlowQueryThresholdMs)
	closer := func() { _ = db.Close() }
	return collector, closer
}

// tryBuildAutoRedisCollector parses the configured URL, opens a
// dedicated go-redis client, and returns a Redis collector. Same
// best-effort policy as the DB variant.
func tryBuildAutoRedisCollector(cfg Config) (RedisCollector, func()) {
	raw := firstNonEmpty(cfg.APMRedisURL, cfg.RedisURL)
	if raw == "" {
		return nil, nil
	}

	opts, err := redis.ParseURL(raw)
	if err != nil {
		return nil, nil
	}
	// Tiny pool — only the INFO command runs against this client, once
	// per metrics interval. PoolSize=2 is generous.
	opts.PoolSize = 2
	opts.MinIdleConns = 0

	client := redis.NewClient(opts)

	// Soft ping — log nothing on failure, the Redis might come up later.
	pingCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = client.Ping(pingCtx).Err()
	cancel()

	collector := newAutoRedisCollector(client, cfg.ServiceName)
	closer := func() { _ = client.Close() }
	return collector, closer
}

// parseDatabaseURL examines the URL scheme to pick a driver name for
// database/sql, returns the DSN in the exact form that driver expects,
// and suggests a "size of current database in MB" query. Supported:
//
//	postgres://...   → driver "pgx", pg_database_size
//	postgresql://... → driver "pgx", pg_database_size
//	mysql://...      → driver "mysql" with user:pass@tcp(host:port)/db, information_schema.tables
//	mariadb://...    → same as mysql
//
// Unknown schemes return an error; the caller silently disables DB
// auto-discovery in that case.
func parseDatabaseURL(raw string) (driver, dsn, sizeQuery string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", "", err
	}
	switch strings.ToLower(u.Scheme) {
	case "postgres", "postgresql", "pgx":
		// pgx driver accepts the URL as-is through its stdlib shim.
		return "pgx", raw, "SELECT pg_database_size(current_database()) / 1024 / 1024", nil
	case "mysql", "mariadb":
		// go-sql-driver/mysql wants user:pass@tcp(host:port)/db, not a URL.
		dsn := mysqlDSNFromURL(u)
		return "mysql", dsn, "SELECT ROUND(SUM(data_length + index_length) / 1024 / 1024, 2) FROM information_schema.tables WHERE table_schema = DATABASE()", nil
	default:
		return "", "", "", errors.New("unsupported DB scheme: " + u.Scheme)
	}
}

// mysqlDSNFromURL converts a mysql://user:pass@host:port/db?opt=1
// URL into the DSN format go-sql-driver/mysql expects.
func mysqlDSNFromURL(u *url.URL) string {
	user := ""
	if u.User != nil {
		user = u.User.Username()
		if pw, ok := u.User.Password(); ok {
			user += ":" + pw
		}
		user += "@"
	}
	host := u.Host
	if host == "" {
		host = "127.0.0.1:3306"
	}
	db := strings.TrimPrefix(u.Path, "/")
	q := u.RawQuery
	suffix := ""
	if q != "" {
		suffix = "?" + q
	}
	return fmt.Sprintf("%stcp(%s)/%s%s", user, host, db, suffix)
}

// firstNonEmpty is a tiny helper for "override or default" env lookup.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// autoSQLCollector is a minimal in-package SQL collector so the main
// `apm` package does not depend on the `collector/` subpackage (which
// would create an import cycle — collector/ already depends on apm).
// The behaviour matches collector.SQLCollector exactly.
type autoSQLCollector struct {
	db        *sql.DB
	service   string
	driver    string
	sizeQuery string
}

func newAutoSQLCollector(db *sql.DB, service, driver, sizeQuery string, slowMs int) *autoSQLCollector {
	return &autoSQLCollector{
		db:        db,
		service:   service,
		driver:    driver,
		sizeQuery: sizeQuery,
	}
}

func (c *autoSQLCollector) Collect(ctx context.Context) (DBMetric, bool) {
	if c.db == nil {
		return DBMetric{}, false
	}

	stats := c.db.Stats()
	m := DBMetric{
		Service:           c.service,
		ConnectionsActive: uint32(stats.InUse),
		ConnectionsIdle:   uint32(stats.Idle),
		ConnectionsMax:    uint32(stats.MaxOpenConnections),
		Timestamp:         time.Now().Unix(),
	}

	// Pull the server-side max_connections for Postgres and MySQL —
	// the database/sql pool cap is rarely the same as the server cap,
	// and the dashboard wants the server view.
	if serverMax, ok := c.queryInt(ctx, serverMaxConnQuery(c.driver)); ok && serverMax > 0 {
		m.ConnectionsMax = uint32(serverMax)
	}

	// DB size.
	if c.sizeQuery != "" {
		if v, ok := c.queryFloat(ctx, c.sizeQuery); ok {
			m.SizeMB = float32(v)
		}
	}

	return m, true
}

func (c *autoSQLCollector) queryInt(ctx context.Context, q string) (int64, bool) {
	if q == "" {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var v int64
	if err := c.db.QueryRowContext(ctx, q).Scan(&v); err != nil {
		return 0, false
	}
	return v, true
}

func (c *autoSQLCollector) queryFloat(ctx context.Context, q string) (float64, bool) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var v float64
	if err := c.db.QueryRowContext(ctx, q).Scan(&v); err != nil {
		return 0, false
	}
	return v, true
}

// serverMaxConnQuery returns the SQL that exposes the server-wide
// max_connections value for the given driver. Empty string = unknown.
func serverMaxConnQuery(driver string) string {
	switch driver {
	case "pgx":
		return "SELECT setting::int FROM pg_settings WHERE name = 'max_connections'"
	case "mysql":
		return "SELECT @@max_connections"
	}
	return ""
}

// autoRedisCollector is the in-package Redis collector. Mirrors
// collector.RedisCollector but lives here to avoid a circular import.
type autoRedisCollector struct {
	client  *redis.Client
	service string
}

func newAutoRedisCollector(client *redis.Client, service string) *autoRedisCollector {
	return &autoRedisCollector{client: client, service: service}
}

func (c *autoRedisCollector) Collect(ctx context.Context) (RedisMetric, bool) {
	if c.client == nil {
		return RedisMetric{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	info, err := c.client.Info(ctx).Result()
	if err != nil {
		return RedisMetric{}, false
	}

	parsed := parseRedisInfo(info)
	hits := atoi64(parsed["keyspace_hits"])
	misses := atoi64(parsed["keyspace_misses"])
	hitRatio := float32(0)
	if hits+misses > 0 {
		hitRatio = float32(float64(hits) / float64(hits+misses) * 100)
	}

	return RedisMetric{
		Service:          c.service,
		MemoryUsedMB:     bytesToMB(parsed["used_memory"]),
		MemoryPeakMB:     bytesToMB(parsed["used_memory_peak"]),
		ConnectedClients: uint32(atoi64(parsed["connected_clients"])),
		OpsPerSec:        uint32(atoi64(parsed["instantaneous_ops_per_sec"])),
		HitRatio:         hitRatio,
		EvictedKeys:      uint64(atoi64(parsed["evicted_keys"])),
		Timestamp:        time.Now().Unix(),
	}, true
}

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

func atoi64(s string) int64 {
	if s == "" {
		return 0
	}
	var v int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		v = v*10 + int64(r-'0')
	}
	return v
}

func bytesToMB(s string) float32 {
	v := atoi64(s)
	if v == 0 {
		return 0
	}
	return float32(v) / 1024 / 1024
}
