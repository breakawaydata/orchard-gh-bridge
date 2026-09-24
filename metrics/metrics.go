// Package metrics is a deliberately small Prometheus text-format exporter.
//
// The bridge exposes a handful of gauges and counters, so it hand-rolls the
// exposition format instead of pulling in client_golang and its dependency
// tree. Every vector method is nil-safe, so code paths that run without a
// registry (unit tests, metrics disabled) need no guards.
package metrics

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Registry holds every metric the process exposes on /metrics.
type Registry struct {
	mu      sync.Mutex
	metrics []*vec
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{} }

// GaugeVec is a gauge partitioned by a fixed set of label names.
type GaugeVec struct{ v *vec }

// CounterVec is a monotonically increasing counter partitioned by a fixed set
// of label names.
type CounterVec struct{ v *vec }

type vec struct {
	name       string
	help       string
	kind       string
	labelNames []string

	mu     sync.Mutex
	series map[string]*series
}

type series struct {
	labelValues []string
	value       float64
}

func (r *Registry) register(name, help, kind string, labelNames []string) *vec {
	v := &vec{
		name:       name,
		help:       help,
		kind:       kind,
		labelNames: append([]string(nil), labelNames...),
		series:     make(map[string]*series),
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.metrics = append(r.metrics, v)
	return v
}

// NewGaugeVec registers a gauge. A nil registry returns a nil (no-op) gauge.
func (r *Registry) NewGaugeVec(name, help string, labelNames ...string) *GaugeVec {
	if r == nil {
		return nil
	}
	return &GaugeVec{v: r.register(name, help, "gauge", labelNames)}
}

// NewCounterVec registers a counter. A nil registry returns a nil (no-op)
// counter.
func (r *Registry) NewCounterVec(name, help string, labelNames ...string) *CounterVec {
	if r == nil {
		return nil
	}
	return &CounterVec{v: r.register(name, help, "counter", labelNames)}
}

// Set sets the gauge for the given label values, in labelNames order.
func (g *GaugeVec) Set(value float64, labelValues ...string) {
	if g == nil {
		return
	}
	g.v.update(labelValues, func(s *series) { s.value = value })
}

// Delete removes the series for the given label values, so a condition that
// has cleared stops being reported rather than lingering at its last value.
func (g *GaugeVec) Delete(labelValues ...string) {
	if g == nil {
		return
	}
	g.v.mu.Lock()
	defer g.v.mu.Unlock()
	delete(g.v.series, seriesKey(labelValues))
}

// Value returns the current value for the given label values and whether the
// series exists. Intended for tests.
func (g *GaugeVec) Value(labelValues ...string) (float64, bool) {
	if g == nil {
		return 0, false
	}
	return g.v.get(labelValues)
}

// Inc adds one to the counter for the given label values.
func (c *CounterVec) Inc(labelValues ...string) {
	if c == nil {
		return
	}
	c.v.update(labelValues, func(s *series) { s.value++ })
}

// Value returns the current value for the given label values. Intended for
// tests.
func (c *CounterVec) Value(labelValues ...string) float64 {
	if c == nil {
		return 0
	}
	v, _ := c.v.get(labelValues)
	return v
}

func (v *vec) update(labelValues []string, fn func(*series)) {
	if len(labelValues) != len(v.labelNames) {
		panic(fmt.Sprintf("metrics: %s wants %d label values, got %d", v.name, len(v.labelNames), len(labelValues)))
	}
	key := seriesKey(labelValues)
	v.mu.Lock()
	defer v.mu.Unlock()
	s, ok := v.series[key]
	if !ok {
		s = &series{labelValues: append([]string(nil), labelValues...)}
		v.series[key] = s
	}
	fn(s)
}

func (v *vec) get(labelValues []string) (float64, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	s, ok := v.series[seriesKey(labelValues)]
	if !ok {
		return 0, false
	}
	return s.value, true
}

// seriesKey joins label values with a byte that cannot appear in valid UTF-8
// text, so distinct tuples never collide.
func seriesKey(labelValues []string) string { return strings.Join(labelValues, "\xff") }

// WriteText writes every registered metric in the Prometheus text exposition
// format (version 0.0.4). Series are sorted so output is stable.
func (r *Registry) WriteText(w io.Writer) error {
	r.mu.Lock()
	vecs := append([]*vec(nil), r.metrics...)
	r.mu.Unlock()

	var b strings.Builder
	for _, v := range vecs {
		fmt.Fprintf(&b, "# HELP %s %s\n", v.name, escapeHelp(v.help))
		fmt.Fprintf(&b, "# TYPE %s %s\n", v.name, v.kind)

		v.mu.Lock()
		keys := make([]string, 0, len(v.series))
		for k := range v.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s := v.series[k]
			b.WriteString(v.name)
			if len(v.labelNames) > 0 {
				b.WriteByte('{')
				for i, ln := range v.labelNames {
					if i > 0 {
						b.WriteByte(',')
					}
					fmt.Fprintf(&b, "%s=\"%s\"", ln, escapeLabelValue(s.labelValues[i]))
				}
				b.WriteByte('}')
			}
			b.WriteByte(' ')
			b.WriteString(strconv.FormatFloat(s.value, 'g', -1, 64))
			b.WriteByte('\n')
		}
		v.mu.Unlock()
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func escapeHelp(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(s)
}

func escapeLabelValue(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`).Replace(s)
}

// Handler serves the registry at any path it is mounted on.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_ = r.WriteText(w)
	})
}

// Serve exposes the registry at GET /metrics on the given port. Blocks until
// the server exits; a failure is logged, never fatal, because losing metrics
// must not take the bridge down with it.
func Serve(port int, r *Registry, logger *slog.Logger) {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", r.Handler())
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	logger.Info("metrics server listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil {
		logger.Error("metrics server error", "error", err)
	}
}
