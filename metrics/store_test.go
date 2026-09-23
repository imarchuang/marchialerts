package metrics

import (
	"testing"
	"time"
)

var testTime = time.Unix(1_700_000_000, 0).UTC()

func addTwoSeries(t *testing.T, st *Store) {
	t.Helper()
	st.Add(
		Sample{Metric: "cpu_usage", Labels: map[string]string{"job": "api", "host": "a"}, T: testTime, V: 0.95},
		Sample{Metric: "cpu_usage", Labels: map[string]string{"job": "api", "host": "b"}, T: testTime, V: 0.10},
	)
}

func TestSelectByMetricAndLabels(t *testing.T) {
	st := NewStore()
	addTwoSeries(t, st)

	both := st.Select("cpu_usage", map[string]string{"job": "api"})
	if len(both) != 2 {
		t.Fatalf("Select(job=api) = %d series, want 2", len(both))
	}
	// Deterministic order: host=a sorts before host=b.
	if both[0].Labels["host"] != "a" || both[1].Labels["host"] != "b" {
		t.Errorf("Select order = %v, %v", both[0].Labels, both[1].Labels)
	}

	one := st.Select("cpu_usage", map[string]string{"job": "api", "host": "a"})
	if len(one) != 1 || one[0].V != 0.95 {
		t.Fatalf("Select(host=a) = %v", one)
	}

	if none := st.Select("cpu_usage", map[string]string{"job": "web"}); len(none) != 0 {
		t.Errorf("Select(job=web) = %v, want empty", none)
	}
	if none := st.Select("other_metric", nil); len(none) != 0 {
		t.Errorf("Select(other_metric) = %v, want empty", none)
	}
}

func TestLatestWins(t *testing.T) {
	st := NewStore()
	labels := map[string]string{"job": "api"}
	st.Add(Sample{Metric: "cpu_usage", Labels: labels, T: testTime, V: 0.5})

	// Newer sample replaces.
	st.Add(Sample{Metric: "cpu_usage", Labels: labels, T: testTime.Add(time.Second), V: 0.9})
	if got := st.Select("cpu_usage", nil)[0].V; got != 0.9 {
		t.Fatalf("after newer add, V = %v, want 0.9", got)
	}

	// Older sample does not.
	st.Add(Sample{Metric: "cpu_usage", Labels: labels, T: testTime, V: 0.1})
	if got := st.Select("cpu_usage", nil)[0].V; got != 0.9 {
		t.Fatalf("after stale add, V = %v, want 0.9", got)
	}
}

// Canonicalization: label map order must not change series identity
// (the marchimetrics SeriesID lesson).
func TestSeriesIdentityIsLabelOrderIndependent(t *testing.T) {
	st := NewStore()
	st.Add(Sample{Metric: "cpu_usage", Labels: map[string]string{"job": "api", "host": "a"}, T: testTime, V: 0.5})
	st.Add(Sample{Metric: "cpu_usage", Labels: map[string]string{"host": "a", "job": "api"}, T: testTime.Add(time.Second), V: 0.9})

	got := st.Select("cpu_usage", nil)
	if len(got) != 1 {
		t.Fatalf("Select = %d series, want 1 (same series, different map order)", len(got))
	}
	if got[0].V != 0.9 {
		t.Errorf("V = %v, want 0.9 (second add updated the same series)", got[0].V)
	}
}
