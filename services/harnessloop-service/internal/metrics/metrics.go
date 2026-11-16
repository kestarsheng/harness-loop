// Package metrics exposes the DFX-M-01 metric set for harnessloop-service.
//
// The registry is intentionally dependency-free: it renders the Prometheus text
// exposition format directly so /metrics works without a collector during
// tests, local runs and single-node deployments. Metric names and labels follow
// design 10.1; every series is pre-registered so a scrape always shows the full
// DFX-M-01 surface even before traffic arrives.
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type Kind string

const (
	KindCounter   Kind = "counter"
	KindGauge     Kind = "gauge"
	KindHistogram Kind = "histogram"
)

// Registry is a concurrency-safe in-process metric registry.
type Registry struct {
	mu     sync.RWMutex
	series map[string]*Series
	order  []string
}

// Series is a single metric series (one label combination).
type Series struct {
	Name    string
	Kind    Kind
	Help    string
	Labels  map[string]string
	Buckets []float64

	mu      sync.Mutex
	count   float64
	sum     float64
	buckets []uint64
}

func NewRegistry() *Registry {
	return &Registry{series: map[string]*Series{}}
}

func seriesKey(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(name)
	for _, k := range keys {
		b.WriteString(",")
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(labels[k])
	}
	return b.String()
}

func (r *Registry) seriesFor(kind Kind, name, help string, labels map[string]string, buckets []float64) *Series {
	key := seriesKey(name, labels)
	r.mu.RLock()
	s, ok := r.series[key]
	r.mu.RUnlock()
	if ok {
		return s
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.series[key]; ok {
		return s
	}
	if len(labels) > 0 {
		copied := make(map[string]string, len(labels))
		for k, v := range labels {
			copied[k] = v
		}
		labels = copied
	}
	s = &Series{Name: name, Kind: kind, Help: help, Labels: labels, Buckets: buckets}
	if kind == KindHistogram {
		s.buckets = make([]uint64, len(buckets))
	}
	r.series[key] = s
	r.order = append(r.order, key)
	return s
}

// Counter registers (or returns) a counter series.
func (r *Registry) Counter(name, help string, labels map[string]string) *Series {
	return r.seriesFor(KindCounter, name, help, labels, nil)
}

// Gauge registers (or returns) a gauge series.
func (r *Registry) Gauge(name, help string, labels map[string]string) *Series {
	return r.seriesFor(KindGauge, name, help, labels, nil)
}

// Histogram registers (or returns) a histogram series with the given buckets.
func (r *Registry) Histogram(name, help string, labels map[string]string, buckets []float64) *Series {
	sorted := append([]float64(nil), buckets...)
	sort.Float64s(sorted)
	return r.seriesFor(KindHistogram, name, help, labels, sorted)
}

func (s *Series) Inc() { s.Add(1) }

func (s *Series) Add(v float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count += v
}

func (s *Series) Set(v float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count = v
}

func (s *Series) Observe(v float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count++
	s.sum += v
	for i, bound := range s.Buckets {
		if v <= bound {
			s.buckets[i]++
			return
		}
	}
}

// Value returns the current counter/gauge value.
func (s *Series) Value() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

func (s *Series) snapshotValue() (count, sum float64, buckets []uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	buckets = append(buckets, s.buckets...)
	return s.count, s.sum, buckets
}

// Sample is a copy of one series used by alert rules and health calculations.
type Sample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// Snapshot returns all current samples. Histograms report their count.
func (r *Registry) Snapshot() []Sample {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Sample, 0, len(r.order))
	for _, key := range r.order {
		s := r.series[key]
		labels := make(map[string]string, len(s.Labels))
		for k, v := range s.Labels {
			labels[k] = v
		}
		count, _, _ := s.snapshotValue()
		out = append(out, Sample{Name: s.Name, Labels: labels, Value: count})
	}
	return out
}

// Find returns the first sample matching name and all provided labels.
func (r *Registry) Find(name string, labels map[string]string) (float64, bool) {
	for _, sample := range r.Snapshot() {
		if sample.Name != name {
			continue
		}
		match := true
		for k, v := range labels {
			if sample.Labels[k] != v {
				match = false
				break
			}
		}
		if match {
			return sample.Value, true
		}
	}
	return 0, false
}

// Render produces the Prometheus text exposition format.
func (r *Registry) Render() string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	byName := map[string][]*Series{}
	names := []string{}
	for _, key := range r.order {
		s := r.series[key]
		if _, seen := byName[s.Name]; !seen {
			names = append(names, s.Name)
		}
		byName[s.Name] = append(byName[s.Name], s)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		group := byName[name]
		first := group[0]
		fmt.Fprintf(&b, "# HELP %s %s\n", name, first.Help)
		fmt.Fprintf(&b, "# TYPE %s %s\n", name, first.Kind)
		for _, s := range group {
			labels := renderLabels(s.Labels, "")
			count, sum, bucketCounts := s.snapshotValue()
			if s.Kind == KindHistogram {
				cumulative := uint64(0)
				for i, bound := range s.Buckets {
					cumulative += bucketCounts[i]
					le := strconv.FormatFloat(bound, 'g', -1, 64)
					fmt.Fprintf(&b, "%s_bucket%s %d\n", name, renderLabels(s.Labels, le), cumulative)
				}
				fmt.Fprintf(&b, "%s_bucket%s %d\n", name, renderLabels(s.Labels, "+Inf"), uint64(count))
				fmt.Fprintf(&b, "%s_sum%s %s\n", name, labels, formatFloat(sum))
				fmt.Fprintf(&b, "%s_count%s %s\n", name, labels, formatFloat(count))
				continue
			}
			fmt.Fprintf(&b, "%s%s %s\n", name, labels, formatFloat(count))
		}
	}
	return b.String()
}

func renderLabels(labels map[string]string, le string) string {
	if len(labels) == 0 && le == "" {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys)+1)
	for _, k := range keys {
		pairs = append(pairs, fmt.Sprintf("%s=%q", k, labels[k]))
	}
	if le != "" {
		pairs = append(pairs, fmt.Sprintf("le=%q", le))
	}
	return "{" + strings.Join(pairs, ",") + "}"
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// Handler serves the /metrics endpoint.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(r.Render()))
	})
}

// Serve exposes /metrics on addr and returns the running server. A nil server
// means addr was empty and metrics stay in-process only.
func (r *Registry) Serve(addr string) *http.Server {
	if strings.TrimSpace(addr) == "" {
		return nil
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", r.Handler())
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() { _ = srv.ListenAndServe() }()
	return srv
}
