// Package metrics implements the in-memory sample store: the eval input
// side of marchialerts (Grafana's datasource, minus PromQL).
package metrics

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// Sample is one (metric, labels, t, v) point — the import shape from
// PLAN.md §5 PR1.
type Sample struct {
	Metric string
	Labels map[string]string
	T      time.Time
	V      float64
}

// seriesKey canonicalizes metric+labels the same way marchimetrics
// canonicalizes SeriesID: sorted k=v pairs, so neither map iteration order
// nor input order can change a series' identity.
func seriesKey(metric string, labels map[string]string) string {
	var b strings.Builder
	b.WriteString(metric)
	b.WriteByte('{')
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
	}
	b.WriteByte('}')
	return b.String()
}

// Store keeps the latest sample per series. That is all the threshold
// evaluator needs; history is a marchimetrics concern, not ours.
type Store struct {
	mu     sync.RWMutex
	latest map[string]Sample
}

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{latest: make(map[string]Sample)}
}

// Add upserts samples and returns how many were applied. A sample replaces
// the stored one for its series unless the stored one is newer.
func (s *Store) Add(samples ...Sample) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, sm := range samples {
		key := seriesKey(sm.Metric, sm.Labels)
		if cur, ok := s.latest[key]; !ok || !sm.T.Before(cur.T) {
			s.latest[key] = sm
			n++
		}
	}
	return n
}

// Select returns the latest sample of every series matching the selector:
// metric equality (skipped when empty) plus label superset match. Results
// are sorted by series key so ticks and tests are deterministic.
func (s *Store) Select(metric string, match map[string]string) []Sample {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Sample
	for _, sm := range s.latest {
		if metric != "" && sm.Metric != metric {
			continue
		}
		if !labelsMatch(sm.Labels, match) {
			continue
		}
		out = append(out, sm)
	}
	sort.Slice(out, func(i, j int) bool {
		return seriesKey(out[i].Metric, out[i].Labels) < seriesKey(out[j].Metric, out[j].Labels)
	})
	return out
}

func labelsMatch(labels, match map[string]string) bool {
	for k, v := range match {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// LabelsString renders labels as sorted k=v pairs — the canonical form used
// for series identity, so logs read the same way identity is computed.
func LabelsString(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
	}
	return b.String()
}
