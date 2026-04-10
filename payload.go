package apm

// The structs below mirror the JSON contracts of the APM backend
// (internal/application/command/ingest_*.go). Field names, types and
// json tags MUST stay in sync — divergence is silently dropped on the
// server side. When you add a field on the backend, add it here too.

// Event is one HTTP (or grpc, mapped) request trace.
// Backend: command.EventInput. Endpoint: POST /ingest.
type Event struct {
	Service    string  `json:"service"`
	Endpoint   string  `json:"endpoint"`
	Method     string  `json:"method"`
	StatusCode uint16  `json:"status_code"`
	DurationMs float32 `json:"duration_ms"`
	Error      string  `json:"error"`
	ClientIP   string  `json:"client_ip"`
	UserAgent  string  `json:"user_agent"`
	Timestamp  int64   `json:"timestamp"`
}

// SystemMetric — host + container + service health snapshot.
// Backend: command.SystemMetricInput. Endpoint: POST /ingest/system.
type SystemMetric struct {
	Service              string  `json:"service"`
	CPUCores             uint8   `json:"cpu_cores"`
	CPULoad1             float32 `json:"cpu_load_1"`
	CPULoad5             float32 `json:"cpu_load_5"`
	CPULoad15            float32 `json:"cpu_load_15"`
	MemUsedMB            float32 `json:"mem_used_mb"`
	MemTotalMB           float32 `json:"mem_total_mb"`
	DiskUsedMB           float32 `json:"disk_used_mb"`
	DiskTotalMB          float32 `json:"disk_total_mb"`
	DBOK                 bool    `json:"db_ok"`
	DBPingMs             float32 `json:"db_ping_ms"`
	DBError              string  `json:"db_error"`
	RedisOK              bool    `json:"redis_ok"`
	RedisPingMs          float32 `json:"redis_ping_ms"`
	RedisError           string  `json:"redis_error"`
	S3OK                 bool    `json:"s3_ok"`
	S3Error              string  `json:"s3_error"`
	QueuePending         uint32  `json:"queue_pending"`
	QueueFailed          uint32  `json:"queue_failed"`
	ContainerMemUsageMB  float32 `json:"container_mem_usage_mb"`
	ContainerMemLimitMB  float32 `json:"container_mem_limit_mb"`
	ContainerCPUUsagePct float32 `json:"container_cpu_usage_pct"`
	ContainerCPULimit    float32 `json:"container_cpu_limit"`
	Features             string  `json:"features"`
	Timestamp            int64   `json:"timestamp"`
}

// DBMetric — detailed database metrics (connections, query stats, size).
// Backend: command.DbMetricInput. Endpoint: POST /ingest/db-metrics.
type DBMetric struct {
	Service           string  `json:"service"`
	ConnectionsActive uint32  `json:"connections_active"`
	ConnectionsIdle   uint32  `json:"connections_idle"`
	ConnectionsMax    uint32  `json:"connections_max"`
	QueriesPerMin     uint32  `json:"queries_per_min"`
	SlowQueries       uint32  `json:"slow_queries"`
	AvgQueryMs        float32 `json:"avg_query_ms"`
	P95QueryMs        float32 `json:"p95_query_ms"`
	MaxQueryMs        float32 `json:"max_query_ms"`
	ReadsPerMin       uint32  `json:"reads_per_min"`
	WritesPerMin      uint32  `json:"writes_per_min"`
	SizeMB            float32 `json:"size_mb"`
	Timestamp         int64   `json:"timestamp"`
}

// RedisMetric — detailed Redis info parsed from INFO output.
// Backend: command.RedisMetricInput. Endpoint: POST /ingest/redis-metrics.
type RedisMetric struct {
	Service          string  `json:"service"`
	MemoryUsedMB     float32 `json:"memory_used_mb"`
	MemoryPeakMB     float32 `json:"memory_peak_mb"`
	ConnectedClients uint32  `json:"connected_clients"`
	OpsPerSec        uint32  `json:"ops_per_sec"`
	HitRatio         float32 `json:"hit_ratio"`
	EvictedKeys      uint64  `json:"evicted_keys"`
	Timestamp        int64   `json:"timestamp"`
}

// FPMMetric — PHP-FPM pool snapshot. Mostly irrelevant for Go services
// but kept for parity with the Laravel SDK and future polyglot use.
// Backend: command.FpmMetricInput. Endpoint: POST /ingest/fpm-metrics.
type FPMMetric struct {
	Service            string `json:"service"`
	ActiveProcesses    uint32 `json:"active_processes"`
	IdleProcesses      uint32 `json:"idle_processes"`
	TotalProcesses     uint32 `json:"total_processes"`
	ListenQueue        uint32 `json:"listen_queue"`
	SlowRequests       uint32 `json:"slow_requests"`
	MaxChildrenReached uint32 `json:"max_children_reached"`
	Timestamp          int64  `json:"timestamp"`
}

// NginxMetric — nginx stub_status snapshot (or values pulled from
// nginx-prometheus-exporter via PrometheusCollector).
// Backend: command.NginxMetricInput. Endpoint: POST /ingest/nginx-metrics.
type NginxMetric struct {
	Service       string  `json:"service"`
	Active        uint32  `json:"active"`
	Reading       uint32  `json:"reading"`
	Writing       uint32  `json:"writing"`
	Waiting       uint32  `json:"waiting"`
	RequestsTotal uint64  `json:"requests_total"`
	RPS           float32 `json:"rps"`
	Timestamp     int64   `json:"timestamp"`
}

// QueueJob — one queue job lifecycle event.
// Backend: command.QueueJobInput. Endpoint: POST /ingest/queue.
//
// Status codes used by the backend (uint8, 1..5):
//   1 = pending
//   2 = processing
//   3 = done
//   4 = failed
//   5 = done with error
type QueueJob struct {
	Service    string  `json:"service"`
	JobID      string  `json:"job_id"`
	Name       string  `json:"name"`
	Queue      string  `json:"queue"`
	Status     uint8   `json:"status"`
	Attempts   uint8   `json:"attempts"`
	Payload    string  `json:"payload"`
	Exception  string  `json:"exception"`
	DurationMs float32 `json:"duration_ms"`
	CreatedAt  int64   `json:"created_at"`
	FailedAt   int64   `json:"failed_at"`
}

// JobStatus — symbolic constants for QueueJob.Status.
const (
	JobStatusPending       uint8 = 1
	JobStatusProcessing    uint8 = 2
	JobStatusDone          uint8 = 3
	JobStatusFailed        uint8 = 4
	JobStatusDoneWithError uint8 = 5
)

// CustomMetric is a Group D metric pulled from Prometheus exposition
// format and not (yet) handled by a typed backend endpoint. The SDK
// buffers and logs these so the user can see what would be shipped;
// once the backend grows a /ingest/custom endpoint, the Client method
// will start posting them.
type CustomMetric struct {
	Service   string            `json:"service"`
	Name      string            `json:"name"`
	Type      string            `json:"type"` // counter, gauge, histogram, summary
	Labels    map[string]string `json:"labels"`
	Value     float64           `json:"value"`
	Timestamp int64             `json:"timestamp"`
}
