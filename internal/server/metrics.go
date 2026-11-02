package server

import (
	"fmt"
	"io"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/adeoluo/vectordb-go/internal/db"
)

var latencyBuckets = []float64{0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}

type histogram struct {
	counts []uint64
	sum    float64
	count  uint64
}

type routeKey struct {
	route string
	code  int
}

// metrics is a minimal Prometheus text-format registry. The standard
// library has no Prometheus client and the project takes no dependencies,
// so the handful of series needed are kept here directly.
type metrics struct {
	start time.Time

	mu       sync.Mutex
	requests map[routeKey]uint64
	latency  map[string]*histogram
}

func newMetrics() *metrics {
	return &metrics{
		start:    time.Now(),
		requests: map[routeKey]uint64{},
		latency:  map[string]*histogram{},
	}
}

func (m *metrics) observe(route string, code int, d time.Duration) {
	sec := d.Seconds()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[routeKey{route, code}]++
	h := m.latency[route]
	if h == nil {
		h = &histogram{counts: make([]uint64, len(latencyBuckets))}
		m.latency[route] = h
	}
	for i, b := range latencyBuckets {
		if sec <= b {
			h.counts[i]++
		}
	}
	h.sum += sec
	h.count++
}

func quote(s string) string { return strconv.Quote(s) }

func (m *metrics) write(w io.Writer, d *db.DB) {
	m.mu.Lock()
	keys := make([]routeKey, 0, len(m.requests))
	for k := range m.requests {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].route != keys[j].route {
			return keys[i].route < keys[j].route
		}
		return keys[i].code < keys[j].code
	})
	fmt.Fprintln(w, "# HELP vectordb_http_requests_total HTTP requests by route and status code.")
	fmt.Fprintln(w, "# TYPE vectordb_http_requests_total counter")
	for _, k := range keys {
		fmt.Fprintf(w, "vectordb_http_requests_total{route=%s,code=\"%d\"} %d\n", quote(k.route), k.code, m.requests[k])
	}
	routes := make([]string, 0, len(m.latency))
	for r := range m.latency {
		routes = append(routes, r)
	}
	sort.Strings(routes)
	fmt.Fprintln(w, "# HELP vectordb_http_request_duration_seconds HTTP request latency by route.")
	fmt.Fprintln(w, "# TYPE vectordb_http_request_duration_seconds histogram")
	for _, r := range routes {
		h := m.latency[r]
		for i, b := range latencyBuckets {
			fmt.Fprintf(w, "vectordb_http_request_duration_seconds_bucket{route=%s,le=\"%g\"} %d\n", quote(r), b, h.counts[i])
		}
		fmt.Fprintf(w, "vectordb_http_request_duration_seconds_bucket{route=%s,le=\"+Inf\"} %d\n", quote(r), h.count)
		fmt.Fprintf(w, "vectordb_http_request_duration_seconds_sum{route=%s} %g\n", quote(r), h.sum)
		fmt.Fprintf(w, "vectordb_http_request_duration_seconds_count{route=%s} %d\n", quote(r), h.count)
	}
	m.mu.Unlock()

	type series struct {
		name, help, typ string
		value           func(db.Stats) float64
	}
	all := []series{
		{"vectordb_collection_vectors", "Live vectors in the collection.", "gauge", func(s db.Stats) float64 { return float64(s.Vectors) }},
		{"vectordb_collection_tombstones", "Deleted or overwritten graph nodes not yet reclaimed.", "gauge", func(s db.Stats) float64 { return float64(s.Tombstones) }},
		{"vectordb_collection_last_lsn", "LSN of the most recent write-ahead log record.", "gauge", func(s db.Stats) float64 { return float64(s.LastLSN) }},
		{"vectordb_collection_snapshot_lsn", "LSN covered by the most recent snapshot.", "gauge", func(s db.Stats) float64 { return float64(s.SnapshotLSN) }},
		{"vectordb_collection_wal_segments", "Write-ahead log segment files on disk.", "gauge", func(s db.Stats) float64 { return float64(s.WALSegments) }},
		{"vectordb_wal_bytes_written_total", "Bytes appended to the write-ahead log since start.", "counter", func(s db.Stats) float64 { return float64(s.WALBytesWritten) }},
		{"vectordb_wal_fsyncs_total", "fsync calls on write-ahead log segments since start.", "counter", func(s db.Stats) float64 { return float64(s.WALFsyncs) }},
		{"vectordb_snapshots_total", "Snapshots written since start.", "counter", func(s db.Stats) float64 { return float64(s.Snapshots) }},
		{"vectordb_upserts_total", "Vectors upserted since start.", "counter", func(s db.Stats) float64 { return float64(s.Upserts) }},
		{"vectordb_deletes_total", "Vectors deleted since start.", "counter", func(s db.Stats) float64 { return float64(s.Deletes) }},
		{"vectordb_searches_total", "Searches served since start.", "counter", func(s db.Stats) float64 { return float64(s.Searches) }},
		{"vectordb_recovery_replayed_records", "Log records replayed when the collection was opened.", "gauge", func(s db.Stats) float64 { return float64(s.Recovery.ReplayedRecords) }},
		{"vectordb_recovery_truncated_bytes", "Bytes of torn or corrupt log tail truncated at open.", "gauge", func(s db.Stats) float64 { return float64(s.Recovery.TruncatedBytes) }},
	}
	cols := d.List()
	stats := make([]db.Stats, len(cols))
	for i, c := range cols {
		stats[i] = c.Stats()
	}
	for _, s := range all {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", s.name, s.help, s.name, s.typ)
		for _, st := range stats {
			fmt.Fprintf(w, "%s{collection=%s} %g\n", s.name, quote(st.Name), s.value(st))
		}
	}

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Fprintf(w, "# HELP vectordb_uptime_seconds Seconds since the server started.\n# TYPE vectordb_uptime_seconds gauge\nvectordb_uptime_seconds %g\n", time.Since(m.start).Seconds())
	fmt.Fprintf(w, "# HELP go_goroutines Number of goroutines.\n# TYPE go_goroutines gauge\ngo_goroutines %d\n", runtime.NumGoroutine())
	fmt.Fprintf(w, "# HELP go_memstats_heap_alloc_bytes Bytes of allocated heap objects.\n# TYPE go_memstats_heap_alloc_bytes gauge\ngo_memstats_heap_alloc_bytes %d\n", ms.HeapAlloc)
}
