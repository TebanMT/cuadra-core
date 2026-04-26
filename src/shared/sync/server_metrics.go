//go:build server

package sync

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Metrics is a tiny, dependency-free Prometheus exposition. We expose the
// counters/histograms ADR-001 §5 calls out, formatted in Prometheus text
// (https://prometheus.io/docs/instrumenting/exposition_formats/#text-based-format).
//
// Why hand-rolled instead of pulling in `prometheus/client_golang`:
//   - Boring SaaS principle — fewer deps, less surface area.
//   - The metric set is small and stable (8 series + bucketed histogram).
//   - Future-flagged: when the ops story grows we'll swap to the official
//     client without touching call sites — Inc / Observe surfaces match.
type Metrics struct {
	mu                sync.RWMutex
	pushReqs          map[string]uint64 // status -> count
	pushItems         map[string]uint64 // status -> count
	pullReqs          map[string]uint64
	pullItems         uint64
	conflicts         map[string]uint64 // type -> count
	queueDepth        map[string]uint64 // gym_id -> backlog (gauge)
	fullReqs          uint64
	durations         map[string]*histogram // endpoint -> histogram
	schemaUpgradeReqs uint64
}

func NewMetrics() *Metrics {
	return &Metrics{
		pushReqs:   make(map[string]uint64),
		pushItems:  make(map[string]uint64),
		pullReqs:   make(map[string]uint64),
		conflicts:  make(map[string]uint64),
		queueDepth: make(map[string]uint64),
		durations:  make(map[string]*histogram),
	}
}

func (m *Metrics) IncPushRequest(status string) { m.mu.Lock(); m.pushReqs[status]++; m.mu.Unlock() }
func (m *Metrics) IncPushItem(status string)    { m.mu.Lock(); m.pushItems[status]++; m.mu.Unlock() }
func (m *Metrics) IncPullRequest(status string) { m.mu.Lock(); m.pullReqs[status]++; m.mu.Unlock() }
func (m *Metrics) IncPullItems(n int) {
	if n <= 0 {
		return
	}
	m.mu.Lock()
	m.pullItems += uint64(n)
	m.mu.Unlock()
}
func (m *Metrics) IncConflict(kind string) { m.mu.Lock(); m.conflicts[kind]++; m.mu.Unlock() }
func (m *Metrics) IncFullRequest()         { m.mu.Lock(); m.fullReqs++; m.mu.Unlock() }
func (m *Metrics) IncSchemaUpgrade()       { m.mu.Lock(); m.schemaUpgradeReqs++; m.mu.Unlock() }
func (m *Metrics) SetQueueDepth(gymID string, d int) {
	m.mu.Lock()
	m.queueDepth[gymID] = uint64(d)
	m.mu.Unlock()
}

// ObserveDuration records a request duration for an endpoint label.
func (m *Metrics) ObserveDuration(endpoint string, d time.Duration) {
	m.mu.Lock()
	h, ok := m.durations[endpoint]
	if !ok {
		h = newHistogram()
		m.durations[endpoint] = h
	}
	h.observe(d.Seconds())
	m.mu.Unlock()
}

// Render — Prometheus text format. Stable ordering for diff-friendliness.
func (m *Metrics) Render() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var b strings.Builder
	writeCounter(&b, "sync_push_requests_total", "push request count by status", m.pushReqs, "status")
	writeCounter(&b, "sync_push_items_total", "push item count by status", m.pushItems, "status")
	writeCounter(&b, "sync_pull_requests_total", "pull request count by status", m.pullReqs, "status")
	b.WriteString("# HELP sync_pull_items_total total entities returned by /sync/pull\n")
	b.WriteString("# TYPE sync_pull_items_total counter\n")
	fmt.Fprintf(&b, "sync_pull_items_total %d\n", m.pullItems)
	writeCounter(&b, "sync_conflict_total", "conflicts by resolution type", m.conflicts, "type")
	b.WriteString("# HELP sync_full_requests_total total /sync/full requests\n")
	b.WriteString("# TYPE sync_full_requests_total counter\n")
	fmt.Fprintf(&b, "sync_full_requests_total %d\n", m.fullReqs)
	b.WriteString("# HELP sync_schema_upgrade_total clients rejected with 426\n")
	b.WriteString("# TYPE sync_schema_upgrade_total counter\n")
	fmt.Fprintf(&b, "sync_schema_upgrade_total %d\n", m.schemaUpgradeReqs)
	writeGauge(&b, "sync_queue_depth", "pending sync_queue items per gym (gauge)", m.queueDepth, "gym_id")
	writeHistograms(&b, "sync_request_duration_seconds", "request duration in seconds", m.durations)
	return b.String()
}

func writeCounter(b *strings.Builder, name, help string, m map[string]uint64, label string) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s counter\n", name)
	keys := sortedKeys(m)
	for _, k := range keys {
		fmt.Fprintf(b, "%s{%s=\"%s\"} %d\n", name, label, k, m[k])
	}
}

func writeGauge(b *strings.Builder, name, help string, m map[string]uint64, label string) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s gauge\n", name)
	keys := sortedKeys(m)
	for _, k := range keys {
		fmt.Fprintf(b, "%s{%s=\"%s\"} %d\n", name, label, k, m[k])
	}
}

func sortedKeys(m map[string]uint64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// histogram — minimal Prom-compatible histogram. Buckets are tuned for our
// expected latency band (sub-100ms typical, alarmed at 5s).
type histogram struct {
	buckets []float64 // upper bounds, ascending
	counts  []uint64  // count per bucket (cumulative on render)
	sum     float64
	count   uint64
}

func newHistogram() *histogram {
	return &histogram{
		buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		counts:  make([]uint64, 11),
	}
}

func (h *histogram) observe(v float64) {
	h.count++
	h.sum += v
	for i, ub := range h.buckets {
		if v <= ub {
			h.counts[i]++
		}
	}
}

func writeHistograms(b *strings.Builder, name, help string, m map[string]*histogram) {
	if len(m) == 0 {
		return
	}
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s histogram\n", name)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h := m[k]
		// h.counts[i] is already cumulative (every bucket whose upper-bound
		// is ≥ the observation gets incremented in observe()), so we emit
		// directly — Prom expects cumulative bucket counts.
		for i, ub := range h.buckets {
			fmt.Fprintf(b, "%s_bucket{endpoint=\"%s\",le=\"%g\"} %d\n", name, k, ub, h.counts[i])
		}
		fmt.Fprintf(b, "%s_bucket{endpoint=\"%s\",le=\"+Inf\"} %d\n", name, k, h.count)
		fmt.Fprintf(b, "%s_sum{endpoint=\"%s\"} %g\n", name, k, h.sum)
		fmt.Fprintf(b, "%s_count{endpoint=\"%s\"} %d\n", name, k, h.count)
	}
}
