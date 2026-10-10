package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Metrics is a small Prometheus text-format exporter. It avoids a heavy
// client dependency for the vertical slice; the metric names follow
// Prometheus conventions so dashboards survive a later switch to
// client_golang / OpenTelemetry (see docs/operations/observability.md).
type Metrics struct {
	mu        sync.Mutex
	requests  map[string]uint64  // method|route|code
	durations map[string]float64 // method|route -> seconds sum
	counts    map[string]uint64  // method|route -> count
	counters  map[string]*atomic.Int64
}

func NewMetrics() *Metrics {
	return &Metrics{requests: map[string]uint64{}, durations: map[string]float64{}, counts: map[string]uint64{},
		counters: map[string]*atomic.Int64{}}
}

func (m *Metrics) ObserveRequest(method, route string, status int, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[fmt.Sprintf("%s|%s|%d", method, route, status)]++
	k := method + "|" + route
	m.durations[k] += d.Seconds()
	m.counts[k]++
}

// Add increments (or decrements) a named counter/gauge.
func (m *Metrics) Add(name string, delta int64) {
	m.mu.Lock()
	c, ok := m.counters[name]
	if !ok {
		c = &atomic.Int64{}
		m.counters[name] = c
	}
	m.mu.Unlock()
	c.Add(delta)
}

func (m *Metrics) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var b strings.Builder
	b.WriteString("# TYPE astrion_http_requests_total counter\n")
	keys := sortedKeys(m.requests)
	for _, k := range keys {
		p := strings.SplitN(k, "|", 3)
		fmt.Fprintf(&b, "astrion_http_requests_total{method=%q,route=%q,code=%q} %d\n", p[0], p[1], p[2], m.requests[k])
	}
	b.WriteString("# TYPE astrion_http_request_duration_seconds summary\n")
	for _, k := range sortedKeys(m.counts) {
		p := strings.SplitN(k, "|", 2)
		fmt.Fprintf(&b, "astrion_http_request_duration_seconds_sum{method=%q,route=%q} %g\n", p[0], p[1], m.durations[k])
		fmt.Fprintf(&b, "astrion_http_request_duration_seconds_count{method=%q,route=%q} %d\n", p[0], p[1], m.counts[k])
	}
	names := make([]string, 0, len(m.counters))
	for n := range m.counters {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, "astrion_%s %d\n", n, m.counters[n].Load())
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(b.String()))
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
