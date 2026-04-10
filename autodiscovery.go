package apm

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
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

	// DB auto-discovery. Besides wiring the detailed collector we
	// install a cheap health probe so SystemCollector can set
	// db_ok / db_ping_ms on every tick — that's what drives the
	// "Uptime" gauge in the dashboard.
	if !a.cfg.DisableAutoDB {
		if dbCol, closer, probe := tryBuildAutoDBCollector(a.cfg); dbCol != nil {
			a.AttachDBCollector(dbCol)
			a.autoCloseFns = append(a.autoCloseFns, closer)
			a.dbProbe = probe
		}
	}

	// Redis auto-discovery, same pattern.
	if !a.cfg.DisableAutoRedis {
		if redisCol, closer, probe := tryBuildAutoRedisCollector(a.cfg); redisCol != nil {
			a.AttachRedisCollector(redisCol)
			a.autoCloseFns = append(a.autoCloseFns, closer)
			a.redisProbe = probe
		}
	}
}

// tryBuildAutoDBCollector parses the configured URL, detects the SQL
// flavour by scheme, opens a dedicated *sql.DB with small pool
// settings, and returns a collector.SQLCollector-equivalent plus its
// Close function. Returns (nil, nil) on any step failure — auto-
// discovery is best-effort and must never block startup.
func tryBuildAutoDBCollector(cfg Config) (DBCollector, func(), healthProbe) {
	raw := firstNonEmpty(cfg.APMDatabaseURL, cfg.DatabaseURL)
	if raw == "" {
		return nil, nil, nil
	}

	driver, dsn, sizeQuery, err := parseDatabaseURL(raw)
	if err != nil {
		return nil, nil, nil // bad URL, don't crash the host app
	}

	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, nil, nil
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

	// Health probe: called from SystemCollector every metrics tick.
	// One ping, hard 1-second timeout so a hung DB cannot block the
	// system metrics loop.
	probe := func(ctx context.Context) (bool, float32, string) {
		pingCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
		defer cancel()
		start := time.Now()
		if err := db.PingContext(pingCtx); err != nil {
			return false, 0, truncateErr(err.Error())
		}
		return true, float32(time.Since(start).Seconds() * 1000), ""
	}

	return collector, closer, probe
}

// tryBuildAutoRedisCollector parses the configured URL, opens a
// dedicated go-redis client, and returns a Redis collector. Same
// best-effort policy as the DB variant.
func tryBuildAutoRedisCollector(cfg Config) (RedisCollector, func(), healthProbe) {
	raw := firstNonEmpty(cfg.APMRedisURL, cfg.RedisURL)
	if raw == "" {
		return nil, nil, nil
	}

	opts, err := redis.ParseURL(raw)
	if err != nil {
		return nil, nil, nil
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

	probe := func(ctx context.Context) (bool, float32, string) {
		pingCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
		defer cancel()
		start := time.Now()
		if err := client.Ping(pingCtx).Err(); err != nil {
			return false, 0, truncateErr(err.Error())
		}
		return true, float32(time.Since(start).Seconds() * 1000), ""
	}

	return collector, closer, probe
}

// truncateErr caps error messages at 300 chars — db_error / redis_error
// columns in ClickHouse are sized for short human messages.
func truncateErr(s string) string {
	if len(s) <= 300 {
		return s
	}
	return s[:300]
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
//
// Beyond the trivial pool stats (active/idle/max, db size), this
// collector periodically probes the server-side query statistics so
// the dashboard can show queries_per_min, avg_query_ms, p95_query_ms
// and slow_queries WITHOUT any instrumentation on the host app —
// which is the whole point of zero-code integration.
//
// Postgres path uses the `pg_stat_statements` extension when it's
// installed; the collector silently falls back to "all zeros" if
// it's missing, so dashboards degrade gracefully.
//
// MySQL path uses `SHOW GLOBAL STATUS` counters (Questions, Com_select,
// Com_insert, Slow_queries) which are always present — no extension
// required.
//
// Both paths remember the previous cumulative counters so Collect()
// returns the DELTA per minute, not the monotonic total. The first
// call seeds the baseline and reports zeros; every subsequent call
// produces a real-per-minute number.
type autoSQLCollector struct {
	db        *sql.DB
	service   string
	driver    string
	sizeQuery string
	slowMs    int

	// Previous-tick counters for delta calculation. Guarded by mu
	// because Collect() can race with itself if called from two
	// metrics loops (shouldn't happen, but cheap insurance).
	mu           sync.Mutex
	prevAt       time.Time
	prevCalls    int64
	prevReads    int64
	prevWrites   int64
	prevSlow     int64
	prevTotalMs  float64
}

func newAutoSQLCollector(db *sql.DB, service, driver, sizeQuery string, slowMs int) *autoSQLCollector {
	return &autoSQLCollector{
		db:        db,
		service:   service,
		driver:    driver,
		sizeQuery: sizeQuery,
		slowMs:    slowMs,
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

	// Query statistics — delta from previous tick.
	c.fillQueryStats(ctx, &m)

	return m, true
}

// fillQueryStats populates queries_per_min, slow_queries, avg_query_ms,
// p95_query_ms, max_query_ms, reads_per_min and writes_per_min based
// on the driver-specific statistics source. It is intentionally best-
// effort: any failure leaves the fields at zero and does not affect
// the pool-stats part of the payload.
func (c *autoSQLCollector) fillQueryStats(ctx context.Context, m *DBMetric) {
	switch c.driver {
	case "pgx":
		c.fillPostgresQueryStats(ctx, m)
	case "mysql":
		c.fillMySQLQueryStats(ctx, m)
	}
}

// fillPostgresQueryStats reads from pg_stat_statements. If the
// extension is missing, the query returns an error and we silently
// skip — the database still shows pool stats and size.
//
// The aggregated row covers ALL queries in the current database for
// the current user, summed across all statements:
//
//	calls            → total query count
//	total_exec_time  → cumulative execution time in ms
//	max_exec_time    → max single-query time in ms
//	slow (calc'd)    → sum of queries above the slow threshold
//
// mean_exec_time is derived from total/calls, and a rough p95 comes
// from ordering by mean_exec_time descending and taking the 5%-top
// row's time. Accurate enough for dashboards, cheap to compute.
func (c *autoSQLCollector) fillPostgresQueryStats(ctx context.Context, m *DBMetric) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	// pg_stat_statements aggregate. The cast to bigint protects us
	// from int overflow on very long-running instances.
	var calls, slow int64
	var totalMs, maxMs float64
	err := c.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(calls), 0)::bigint,
			COALESCE(SUM(total_exec_time), 0)::float,
			COALESCE(MAX(max_exec_time), 0)::float,
			COALESCE(SUM(CASE WHEN mean_exec_time >= $1 THEN calls ELSE 0 END), 0)::bigint
		FROM pg_stat_statements
	`, float64(c.slowMs)).Scan(&calls, &totalMs, &maxMs, &slow)
	if err != nil {
		return // extension missing or permission denied — degrade gracefully
	}

	// p95 approximation: the slowest mean_exec_time in the top-5%
	// bucket of statements by call count. Not a true percentile over
	// individual queries but correlates well with the real p95 on
	// realistic dashboards.
	var p95 float64
	_ = c.db.QueryRowContext(ctx, `
		SELECT COALESCE(
		  (SELECT mean_exec_time
		   FROM pg_stat_statements
		   ORDER BY mean_exec_time DESC
		   LIMIT 1 OFFSET GREATEST((SELECT count(*)/20 FROM pg_stat_statements), 0)),
		  0
		)::float
	`).Scan(&p95)

	// Compute per-minute delta from the previous snapshot.
	c.applyDelta(m, calls, 0, 0, slow, totalMs, maxMs, p95)
}

// fillMySQLQueryStats uses SHOW GLOBAL STATUS — always available, no
// extensions needed. We read:
//
//	Questions     → total queries
//	Com_select    → read operations
//	Com_insert/update/delete/replace → write operations
//	Slow_queries  → slow query count
//
// MySQL does not expose average / p95 query time through STATUS, so
// those stay zero unless the host later wires a manual collector.
// That's a deliberate trade-off: zero-config wins out over feature
// completeness for now.
func (c *autoSQLCollector) fillMySQLQueryStats(ctx context.Context, m *DBMetric) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	status, err := c.readMySQLStatusMap(ctx,
		"Questions", "Com_select", "Com_insert", "Com_update",
		"Com_delete", "Com_replace", "Slow_queries")
	if err != nil {
		return
	}

	calls := status["Questions"]
	reads := status["Com_select"]
	writes := status["Com_insert"] + status["Com_update"] +
		status["Com_delete"] + status["Com_replace"]
	slow := status["Slow_queries"]

	c.applyDelta(m, calls, reads, writes, slow, 0, 0, 0)
}

// readMySQLStatusMap executes SHOW GLOBAL STATUS WHERE Variable_name
// IN (...) and collects the results into a map. Missing names are
// silently absent from the map — the caller treats missing as 0.
func (c *autoSQLCollector) readMySQLStatusMap(ctx context.Context, names ...string) (map[string]int64, error) {
	if len(names) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(names))
	args := make([]any, len(names))
	for i, n := range names {
		placeholders[i] = "?"
		args[i] = n
	}
	query := "SHOW GLOBAL STATUS WHERE Variable_name IN (" +
		strings.Join(placeholders, ",") + ")"

	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]int64, len(names))
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return nil, err
		}
		n, _ := parseInt64(value)
		out[name] = n
	}
	return out, rows.Err()
}

// parseInt64 is a tiny strconv-free int parser. Used for MySQL STATUS
// values which are decimal integers as strings. We ignore errors and
// treat any non-numeric value as zero.
func parseInt64(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	var v int64
	neg := false
	i := 0
	if s[0] == '-' {
		neg = true
		i++
	}
	for ; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, nil
		}
		v = v*10 + int64(c-'0')
	}
	if neg {
		v = -v
	}
	return v, nil
}

// applyDelta converts cumulative counters into per-minute values.
// The first call seeds the baseline and reports zeros; every
// subsequent call computes (now - prev) / elapsed_min.
func (c *autoSQLCollector) applyDelta(m *DBMetric, calls, reads, writes, slow int64, totalMs, maxMs, p95 float64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	if c.prevAt.IsZero() {
		c.prevAt = now
		c.prevCalls = calls
		c.prevReads = reads
		c.prevWrites = writes
		c.prevSlow = slow
		c.prevTotalMs = totalMs
		// First call: report max/p95 directly (they're instantaneous
		// snapshot values, not rates) but leave rates at zero so we
		// don't show garbage.
		m.MaxQueryMs = float32(maxMs)
		m.P95QueryMs = float32(p95)
		return
	}

	elapsed := now.Sub(c.prevAt).Minutes()
	if elapsed <= 0 {
		return
	}

	callsDelta := calls - c.prevCalls
	readsDelta := reads - c.prevReads
	writesDelta := writes - c.prevWrites
	slowDelta := slow - c.prevSlow
	msDelta := totalMs - c.prevTotalMs

	// Counter wrap-around protection: if any delta is negative it
	// usually means the server restarted and reset its counters.
	// Re-seed the baseline, report zero this tick.
	if callsDelta < 0 || readsDelta < 0 || writesDelta < 0 || slowDelta < 0 || msDelta < 0 {
		c.prevAt = now
		c.prevCalls = calls
		c.prevReads = reads
		c.prevWrites = writes
		c.prevSlow = slow
		c.prevTotalMs = totalMs
		return
	}

	m.QueriesPerMin = uint32(float64(callsDelta) / elapsed)
	m.ReadsPerMin = uint32(float64(readsDelta) / elapsed)
	m.WritesPerMin = uint32(float64(writesDelta) / elapsed)
	m.SlowQueries = uint32(slowDelta)
	if callsDelta > 0 {
		m.AvgQueryMs = float32(msDelta / float64(callsDelta))
	}
	m.MaxQueryMs = float32(maxMs)
	m.P95QueryMs = float32(p95)

	c.prevAt = now
	c.prevCalls = calls
	c.prevReads = reads
	c.prevWrites = writes
	c.prevSlow = slow
	c.prevTotalMs = totalMs
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
