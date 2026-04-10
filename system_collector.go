package apm

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// SystemCollector builds a SystemMetric snapshot for /ingest/system.
//
// Sources of data, in order of preference:
//
//  1. Prometheus (if cfg.Prometheus.Enabled and SourcePolicy.Host says so),
//  2. Linux /proc and /sys/fs/cgroup (cgroup v2 only),
//  3. Go's syscall.Sysinfo / runtime fallback.
//
// The collector is stateless except for the previous CPU snapshot used to
// turn cpu.stat counters into a percentage. That state is guarded by a
// mutex so the collector is safe to call from multiple goroutines.
type SystemCollector struct {
	cfg Config

	mu          sync.Mutex
	prevCPUUsec int64
	prevCPUTime time.Time
}

// NewSystemCollector wires the collector. It does not perform any I/O —
// the first read happens when Collect is called.
func NewSystemCollector(cfg Config) *SystemCollector {
	return &SystemCollector{cfg: cfg}
}

// Collect builds one SystemMetric snapshot. If a non-nil PrometheusCollector
// is passed and the SourcePolicy says Prometheus owns the host group, the
// host fields are filled from Prometheus and local /proc reads are skipped.
//
// This is the place where the "no x2" rule is enforced for Group A: we
// never read /proc and Prometheus for the same field within one snapshot.
func (s *SystemCollector) Collect(prom *PrometheusCollector) SystemMetric {
	m := SystemMetric{
		Service:   s.cfg.ServiceName,
		Timestamp: time.Now().Unix(),
	}

	hostFromProm := s.cfg.Prometheus.Enabled && prom != nil &&
		policyAllowsProm(s.cfg.Prometheus.SourcePolicy.Host)

	var features []string

	if hostFromProm {
		// Prometheus owns the host group: pull cpu/mem/disk via PromQL or
		// scraped node_exporter values. Local /proc is NOT touched, so a
		// single SystemMetric row never carries two sources for the same
		// field.
		if prom.FillHostMetrics(&m) {
			features = append(features, "prometheus")
		} else {
			// Prometheus enabled but returned nothing useful; fall back to
			// local. The user explicitly opted into AUTO behaviour and we
			// don't want a flat zero in their dashboard.
			s.fillHostFromProc(&m)
		}
	} else {
		s.fillHostFromProc(&m)
	}

	// Container metrics live outside the host group — cgroup v2 reflects
	// the bounds set by Docker / k8s and the SDK is the natural source
	// because it runs inside the container. Prometheus + cadvisor would
	// double here only on Kubernetes; for now we keep this SDK-only.
	s.fillContainerMetrics(&m)

	// Service health: db_ok, redis_ok. These are not pulled from
	// Prometheus by SystemCollector — they come from the dedicated
	// DBCollector / RedisCollector if attached. SystemMetric only carries
	// the boolean health flag, which we leave at zero unless someone
	// explicitly fills it via the optional setters below.

	// Runtime feature flag (so the dashboard can render Go-specific tabs).
	if s.cfg.Monitor.Runtime == nil || *s.cfg.Monitor.Runtime {
		features = append(features, "go-runtime")
	}

	m.Features = strings.Join(features, ",")
	return m
}

// policyAllowsProm decides whether a given Source value should result in
// reading from Prometheus. SourceAuto and SourcePrometheus both do; only
// SourceSDK explicitly opts out.
func policyAllowsProm(s Source) bool {
	return s == SourceAuto || s == SourcePrometheus
}

// fillHostFromProc reads CPU load, memory and disk from /proc and the
// filesystem. This is the SDK's own source for the host group.
func (s *SystemCollector) fillHostFromProc(m *SystemMetric) {
	m.CPUCores = uint8(runtime.NumCPU())

	if l1, l5, l15, ok := readLoadAvg(); ok {
		m.CPULoad1 = l1
		m.CPULoad5 = l5
		m.CPULoad15 = l15
	}

	if used, total, ok := readMemInfo(); ok {
		m.MemUsedMB = used
		m.MemTotalMB = total
	}

	if used, total, ok := readDiskUsage("/"); ok {
		m.DiskUsedMB = used
		m.DiskTotalMB = total
	}
}

// fillContainerMetrics reads cgroup v2 files. cgroup v1 hosts simply
// leave the four container_* fields at zero, which the dashboard knows
// to interpret as "no container limits".
func (s *SystemCollector) fillContainerMetrics(m *SystemMetric) {
	// memory.current — bytes
	if v, ok := readUint64File("/sys/fs/cgroup/memory.current"); ok {
		m.ContainerMemUsageMB = float32(v) / 1024 / 1024
	}
	// memory.max — bytes or "max"
	if data, err := os.ReadFile("/sys/fs/cgroup/memory.max"); err == nil {
		raw := strings.TrimSpace(string(data))
		if raw != "max" {
			if v, err := strconv.ParseUint(raw, 10, 64); err == nil {
				m.ContainerMemLimitMB = float32(v) / 1024 / 1024
			}
		}
	}
	// cpu.max — "<quota> <period>" or "max <period>"
	if data, err := os.ReadFile("/sys/fs/cgroup/cpu.max"); err == nil {
		parts := strings.Fields(strings.TrimSpace(string(data)))
		if len(parts) == 2 && parts[0] != "max" {
			quota, qErr := strconv.ParseInt(parts[0], 10, 64)
			period, pErr := strconv.ParseInt(parts[1], 10, 64)
			if qErr == nil && pErr == nil && period > 0 {
				m.ContainerCPULimit = float32(quota) / float32(period)
			}
		}
	}
	// cpu.stat → usage_usec → percent over the previous interval
	if data, err := os.ReadFile("/sys/fs/cgroup/cpu.stat"); err == nil {
		var currentUsec int64
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "usage_usec") {
				fields := strings.Fields(line)
				if len(fields) == 2 {
					currentUsec, _ = strconv.ParseInt(fields[1], 10, 64)
				}
				break
			}
		}
		if currentUsec > 0 {
			now := time.Now()
			s.mu.Lock()
			if !s.prevCPUTime.IsZero() {
				deltaUsec := currentUsec - s.prevCPUUsec
				deltaTime := now.Sub(s.prevCPUTime).Seconds()
				if deltaTime > 0 && deltaUsec > 0 {
					cpuSeconds := float64(deltaUsec) / 1_000_000
					pct := (cpuSeconds / deltaTime) * 100
					if pct < 0 {
						pct = 0
					}
					if pct > 10000 {
						pct = 10000
					}
					m.ContainerCPUUsagePct = float32(pct)
				}
			}
			s.prevCPUUsec = currentUsec
			s.prevCPUTime = now
			s.mu.Unlock()
		}
	}
}

// readLoadAvg reads /proc/loadavg. On non-Linux it returns ok=false.
func readLoadAvg() (float32, float32, float32, bool) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, 0, 0, false
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return 0, 0, 0, false
	}
	l1, e1 := strconv.ParseFloat(fields[0], 32)
	l5, e5 := strconv.ParseFloat(fields[1], 32)
	l15, e15 := strconv.ParseFloat(fields[2], 32)
	if e1 != nil || e5 != nil || e15 != nil {
		return 0, 0, 0, false
	}
	return float32(l1), float32(l5), float32(l15), true
}

// readMemInfo parses /proc/meminfo for MemTotal and MemAvailable in MB.
// Used = Total - Available, matching the Laravel SDK formula.
func readMemInfo() (used float32, total float32, ok bool) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	var totalKB, availKB uint64
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			totalKB, _ = strconv.ParseUint(fields[1], 10, 64)
		case "MemAvailable:":
			availKB, _ = strconv.ParseUint(fields[1], 10, 64)
		}
		if totalKB > 0 && availKB > 0 {
			break
		}
	}
	if totalKB == 0 {
		return 0, 0, false
	}
	totalMB := float32(totalKB) / 1024
	usedMB := totalMB - float32(availKB)/1024
	return usedMB, totalMB, true
}

// readDiskUsage uses statfs(2) to get total/used in MB for the given path.
// On non-Linux this still works; only the cgroup helpers are Linux-only.
func readDiskUsage(path string) (used float32, total float32, ok bool) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, false
	}
	totalBytes := stat.Blocks * uint64(stat.Bsize)
	freeBytes := stat.Bavail * uint64(stat.Bsize)
	usedBytes := totalBytes - freeBytes
	const mb = float32(1024 * 1024)
	return float32(usedBytes) / mb, float32(totalBytes) / mb, true
}

// readUint64File reads a single integer from a sysfs / procfs file.
// Returns ok=false if the file is missing or unparsable.
func readUint64File(path string) (uint64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
