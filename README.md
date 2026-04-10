# Yammi APM Go SDK

Go client for the Yammi APM backend. Sister package of `sdk/laravel/`.

Ships request traces, system metrics and (optionally) Prometheus
metrics to the APM ingest API. Designed to drop into a yammi-style
microservice (gRPC + chi + pgx + redis) with three lines of wiring.

## Install

```bash
go get github.com/RomaLytar/yammiApmSdkGo
```

The module is private (lives next to `sdk/laravel/` and is published from
the same repo). To consume it from outside:

```bash
go env -w GOPRIVATE=github.com/RomaLytar/*
git config --global url."git@github.com:".insteadOf "https://github.com/"
go get github.com/RomaLytar/yammiApmSdkGo@latest
```

## Quick start (HTTP / chi)

```go
import (
    apm   "github.com/RomaLytar/yammiApmSdkGo"
    apmmw "github.com/RomaLytar/yammiApmSdkGo/middleware"
)

func main() {
    cfg := apm.LoadConfigFromEnv()
    cfg.ServiceName = "board"

    a, err := apm.New(cfg)
    if err != nil { log.Fatal(err) }
    defer a.Shutdown(context.Background())

    r := chi.NewRouter()
    r.Use(apmmw.HTTP(a))
    // ... your routes
    http.ListenAndServe(":8080", r)
}
```

## Quick start (gRPC)

```go
srv := grpc.NewServer(
    grpc.UnaryInterceptor(apmmw.GRPCUnary(a)),
    grpc.StreamInterceptor(apmmw.GRPCStream(a)),
)
```

gRPC traces are recorded as `apm.Event` with:

- `endpoint = info.FullMethod` (e.g. `/board.BoardService/Create`)
- `method = "POST"` (gRPC has no HTTP verbs; backend's allow-list forces this)
- `status_code` = HTTP equivalent of the gRPC code (`OK→200`, `NOT_FOUND→404`, …)

## Environment variables

| Variable | Default | Notes |
|---|---|---|
| `APM_ENABLED` | `true` | Master switch. `false` = no-op. |
| `APM_ENDPOINT` | `http://localhost:8890/ingest` | Base URL of the APM backend. |
| `APM_TOKEN` | `` | `X-APM-Token` header. Empty disables sending. |
| `APM_SERVICE_NAME` | `default` | Logical service id (set this!). |
| `APM_FLUSH_INTERVAL` | `5` | Trace buffer flush, seconds. |
| `APM_BUFFER_SIZE` | `100` | Force-flush threshold (events). |
| `APM_METRICS_INTERVAL` | `60` | System / DB / Redis tick, seconds. |
| `APM_TIMEOUT` | `5` | HTTP timeout, seconds. |
| `APM_DEBUG` | `false` | Log every payload before sending. |
| `APM_PROMETHEUS_ENABLED` | `false` | Group D + E switch. |
| `APM_PROMETHEUS_LOCAL_URL` | `` | Local `/metrics` endpoint to scrape. |
| `APM_PROMETHEUS_URL` | `` | Prometheus HTTP API base URL. |
| `APM_PROMETHEUS_JOB` | `` | Optional `job=` label for PromQL templates. |
| `APM_PROMETHEUS_INSTANCE` | `` | Optional `instance=` label. |
| `APM_PROMETHEUS_CUSTOM_METRICS` | `` | Comma-separated metric name **prefixes** to forward as Group D, e.g. `board_grpc_,board_events_`. |

## What gets collected

| Group | What | Source by default | Endpoint |
|---|---|---|---|
| **A** | host (cpu/mem/disk/load) | SDK (`/proc`) | `/ingest/system` |
| **A** | container (cgroup v2) | SDK (`/sys/fs/cgroup`) | `/ingest/system` |
| **B** | db_ok / db_ping_ms | DB collector you attach | `/ingest/system` (boolean) |
| **B** | db detailed (conns, p95) | `collector.SQLCollector` | `/ingest/db-metrics` |
| **B** | redis info | `collector.RedisCollector` | `/ingest/redis-metrics` |
| **C** | HTTP / gRPC traces | `middleware.HTTP` / `middleware.GRPCUnary` | `/ingest` |
| **D** | custom Prometheus metrics | local `/metrics` scrape | logged in debug mode |
| **E** | host metrics from Prometheus | server query / local scrape | overrides Group A when configured |

## How duplicates are avoided

The "no x2" rule lives in `system_collector.go` and is enforced per **group**:

- If `APM_PROMETHEUS_ENABLED=true` and `SourcePolicy.Host` is `Auto` or
  `Prometheus`, the SDK pulls host metrics from Prometheus and **does
  not call `/proc` for the same fields**. The result is one row per
  metrics interval, populated from one source.

- If Prometheus is enabled but returns nothing useful for a field, the
  SDK falls back to its own collection so the dashboard does not show a
  flat zero. This fallback only happens within a single tick — there is
  never a tick where both sources contribute to the same field.

- Group C (traces, queue jobs) and queue/s3 are SDK-only by design;
  Prometheus does not know them and there is nothing to deduplicate.

- Group D (custom app metrics) is **only** sourced from Prometheus —
  the SDK never tries to invent counters that already live in your
  `prometheus.client_golang` registry.

## Postgres detailed metrics example

```go
import (
    "database/sql"
    "github.com/RomaLytar/yammiApmSdkGo/collector"
    _ "github.com/jackc/pgx/v5/stdlib"
)

db, _ := sql.Open("pgx", os.Getenv("DATABASE_URL"))

dbColl := collector.NewSQLCollector(collector.SQLCollectorOptions{
    DB:      db,
    Service: "board",
    Driver:  "postgres",
    SizeQuery: "SELECT pg_database_size(current_database()) / 1024 / 1024",
})
a.AttachDBCollector(dbColl)

// Wherever you run queries:
start := time.Now()
rows, err := db.Query("SELECT ...")
dbColl.Observe(float64(time.Since(start).Milliseconds()), false)
```

## Redis detailed metrics example

```go
import (
    "github.com/RomaLytar/yammiApmSdkGo/collector"
    "github.com/redis/go-redis/v9"
)

rdb := redis.NewClient(&redis.Options{Addr: "redis:6379"})

type infoAdapter struct{ c *redis.Client }
func (a infoAdapter) InfoString(ctx context.Context) (string, error) {
    return a.c.Info(ctx).Result()
}

a.AttachRedisCollector(collector.NewRedisCollector("board", infoAdapter{rdb}))
```

## Custom Prometheus metrics (Group D)

If your service already exposes `/metrics` (any service in `~/PetProject/yammi`
does — they all use `promauto`), point the SDK at it:

```bash
APM_PROMETHEUS_ENABLED=true
APM_PROMETHEUS_LOCAL_URL=http://localhost:2112/metrics
APM_PROMETHEUS_CUSTOM_METRICS=board_grpc_,board_events_
APM_DEBUG=true
```

Every metric whose name starts with `board_grpc_` or `board_events_`
gets parsed and (in debug mode) printed to the SDK log:

```
[apm] custom metric: service=board name=board_grpc_requests_total type=counter value=42 labels=map[code:OK method:Create]
```

When the backend grows a `/ingest/custom` endpoint, the SDK will
automatically POST these instead of (or in addition to) logging.

## Smoke test on yammi

1. Bring up the APM backend: `cd ~/PetProject/APM && docker compose up -d`.
2. Add the SDK to `~/PetProject/yammi/services/board`:
   `go get github.com/RomaLytar/yammiApmSdkGo`
3. Wire `apmmw.GRPCUnary(a)` into `services/board/cmd/main.go`.
4. Set the env vars in `docker-compose.yml`:
   ```yaml
   APM_ENDPOINT: http://host.docker.internal:8890/ingest
   APM_TOKEN: <token from APM dashboard>
   APM_SERVICE_NAME: board
   APM_PROMETHEUS_ENABLED: "true"
   APM_PROMETHEUS_LOCAL_URL: http://board:2112/metrics
   APM_PROMETHEUS_CUSTOM_METRICS: board_grpc_,board_events_
   APM_DEBUG: "true"
   ```
5. `docker compose up -d board` and exercise it manually first
   (`grpcurl board:50053 board.BoardService/...`).
6. Once the dashboard shows traces and the SDK log shows custom metrics,
   run `tests/load/realistic_2000_users.js` to load test the path.

## Layout

```
sdk/go/
├── apm.go              entry point: New, Shutdown, metricsLoop
├── config.go           Config + LoadConfigFromEnv
├── client.go           HTTP transport, retry, X-APM-Token
├── buffer.go           thread-safe trace buffer + ticker
├── payload.go          DTOs that mirror backend ingest_*.go
├── collector.go        DBCollector / RedisCollector interfaces
├── system_collector.go host + container, with Prometheus override
├── prom_collector.go   local /metrics scrape + Prometheus HTTP API
├── collector/
│   ├── db_sql.go       database/sql + Observe()
│   ├── redis.go        INFO parser
│   ├── runtime.go      Go runtime stats as CustomMetric
│   └── sort.go
├── middleware/
│   ├── http.go         net/http handler wrapper
│   └── grpc.go         unary + stream interceptors
└── examples/
    ├── http_chi/main.go
    └── grpc_server/main.go
```
# YammiSdkApmGo
