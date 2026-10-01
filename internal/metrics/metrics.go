// Package metrics keeps the brain's counters and writes them in Prometheus's
// text format. A handful of series do not need a client library.
package metrics

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
)

// Set is a family of labelled counters.
type Set struct {
	mu       sync.Mutex
	counters map[string]*family
}

type family struct {
	help   string
	labels []string
	values map[string]float64 // key: the label values joined by \x00
}

// New returns an empty set.
func New() *Set { return &Set{counters: map[string]*family{}} }

// Counter declares a counter and its labels.
func (s *Set) Counter(name, help string, labels ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counters[name] = &family{help: help, labels: labels, values: map[string]float64{}}
}

// Inc adds one to a counter's series.
func (s *Set) Inc(name string, values ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.counters[name]
	if !ok || len(values) != len(f.labels) {
		return
	}
	f.values[strings.Join(values, "\x00")]++
}

// Add adds an amount to a counter's series.
func (s *Set) Add(name string, v float64, values ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.counters[name]
	if !ok || len(values) != len(f.labels) || v < 0 {
		return
	}
	f.values[strings.Join(values, "\x00")] += v
}

// Get reads a counter's series (tests).
func (s *Set) Get(name string, values ...string) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f, ok := s.counters[name]; ok {
		return f.values[strings.Join(values, "\x00")]
	}
	return 0
}

// Write writes every counter.
func (s *Set) Write(w io.Writer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, name := range slices.Sorted(maps.Keys(s.counters)) {
		f := s.counters[name]
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", name, f.help, name)
		for _, key := range slices.Sorted(maps.Keys(f.values)) {
			fmt.Fprintf(w, "%s%s %g\n", name, Labels(f.labels, strings.Split(key, "\x00")), f.values[key])
		}
	}
}

// Gauge writes one gauge series.
func Gauge(w io.Writer, name string, labels, values []string, v float64) {
	fmt.Fprintf(w, "%s%s %g\n", name, Labels(labels, values), v)
}

// Labels renders {a="x",b="y"}; nothing for no labels.
func Labels(names, values []string) string {
	if len(names) == 0 {
		return ""
	}
	parts := make([]string, len(names))
	for i, n := range names {
		v := ""
		if i < len(values) {
			v = values[i]
		}
		v = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
		parts[i] = fmt.Sprintf(`%s="%s"`, n, v)
	}
	return "{" + strings.Join(parts, ",") + "}"
}
