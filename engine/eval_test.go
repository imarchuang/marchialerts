package engine_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"marchialerts/engine"
	"marchialerts/metrics"
)

func highCPU() engine.Rule {
	return engine.Rule{
		Alert: "HighCPU",
		Expr:  engine.Expr{Metric: "cpu_usage", Labels: map[string]string{"job": "api"}, Op: ">", Threshold: 0.8},
	}
}

// twoSeries: host=a breaches (0.95 > 0.8), host=b is fine (0.10).
func twoSeries(t *testing.T) *metrics.Store {
	t.Helper()
	st := metrics.NewStore()
	now := time.Unix(1_700_000_000, 0).UTC()
	st.Add(
		metrics.Sample{Metric: "cpu_usage", Labels: map[string]string{"job": "api", "host": "a"}, T: now, V: 0.95},
		metrics.Sample{Metric: "cpu_usage", Labels: map[string]string{"job": "api", "host": "b"}, T: now, V: 0.10},
	)
	return st
}

// PR1 acceptance, restated on the PR2 state machine: one rule, two series —
// the "problem" is per label set, not per rule. Only the breaching series
// produces a (firing) transition; the healthy one stays silently normal.
func TestEvalOnceTwoSeriesOnlyBadOneFires(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	ev := &engine.Evaluator{
		Rules:  []engine.Rule{highCPU()},
		Store:  twoSeries(t),
		States: engine.NewStateManager(),
		Now:    func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
		Log:    log,
	}

	transitions, err := ev.EvalOnce()
	if err != nil {
		t.Fatal(err)
	}
	if len(transitions) != 1 {
		t.Fatalf("transitions = %d, want 1 (only host=a fires)", len(transitions))
	}
	tr := transitions[0]
	if tr.To != engine.StateFiring || tr.Instance.Labels["host"] != "a" {
		t.Errorf("transition = %+v, want →firing for host=a", tr)
	}

	ev.LogTransitions(transitions)
	out := buf.String()
	if c := strings.Count(out, "to=firing"); c != 1 {
		t.Errorf("firing lines = %d, want 1\n%s", c, out)
	}
	if strings.Contains(out, "host=b") {
		t.Errorf("host=b is steady normal and must stay quiet\n%s", out)
	}
}

func TestEvalOnceNoSeriesNoTransitions(t *testing.T) {
	ev := &engine.Evaluator{
		Rules: []engine.Rule{highCPU()},
		Store: metrics.NewStore(),
		Log:   slog.Default(),
	}
	transitions, err := ev.EvalOnce()
	if err != nil {
		t.Fatal(err)
	}
	if len(transitions) != 0 {
		t.Errorf("transitions = %v, want empty", transitions)
	}
}

func TestCompareOps(t *testing.T) {
	cases := []struct {
		op        string
		threshold float64
		v         float64
		want      bool
	}{
		{">", 0.8, 0.9, true},
		{">", 0.8, 0.8, false},
		{">=", 0.8, 0.8, true},
		{"<", 10, 9, true},
		{"<", 10, 10, false},
		{"<=", 10, 10, true},
		{"==", 1, 1, true},
		{"!=", 1, 2, true},
	}
	for _, c := range cases {
		e := engine.Expr{Op: c.op, Threshold: c.threshold}
		got, err := e.Compare(c.v)
		if err != nil {
			t.Fatalf("op %q: %v", c.op, err)
		}
		if got != c.want {
			t.Errorf("%v %s %v = %v, want %v", c.v, c.op, c.threshold, got, c.want)
		}
	}
	if _, err := (engine.Expr{Op: "~"}).Compare(1); err == nil {
		t.Error("unknown op should error")
	}
}

func TestRuleValidate(t *testing.T) {
	if err := highCPU().Validate(); err != nil {
		t.Fatalf("valid rule: %v", err)
	}
	bad := highCPU()
	bad.Expr.Op = "approx"
	if err := bad.Validate(); err == nil {
		t.Error("bad op should fail validation")
	}
	bad = highCPU()
	bad.Alert = ""
	if err := bad.Validate(); err == nil {
		t.Error("empty alert name should fail validation")
	}
	bad = highCPU()
	bad.Expr.Metric = ""
	if err := bad.Validate(); err == nil {
		t.Error("empty metric should fail validation")
	}
}
