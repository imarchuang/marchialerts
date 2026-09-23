package engine_test

import (
	"testing"
	"time"

	"marchialerts/engine"
	"marchialerts/metrics"
)

// fakeClock is a deterministic clock for state-machine tests.
// Tests advance it explicitly; nobody sleeps in CI.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time      { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func newHarness(forDur time.Duration) (*engine.Evaluator, *metrics.Store, *fakeClock) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0).UTC()}
	st := metrics.NewStore()
	rule := engine.Rule{
		Alert: "HighCPU",
		Expr:  engine.Expr{Metric: "cpu_usage", Labels: map[string]string{"job": "api"}, Op: ">", Threshold: 0.8},
		For:   forDur,
	}
	ev := &engine.Evaluator{
		Rules:  []engine.Rule{rule},
		Store:  st,
		States: engine.NewStateManager(),
		Now:    clock.Now,
	}
	return ev, st, clock
}

func setValue(st *metrics.Store, clock *fakeClock, v float64) {
	st.Add(metrics.Sample{
		Metric: "cpu_usage",
		Labels: map[string]string{"job": "api", "host": "a"},
		T:      clock.Now(),
		V:      v,
	})
}

func statesOf(trs []engine.Transition) []engine.State {
	out := make([]engine.State, len(trs))
	for i, tr := range trs {
		out[i] = tr.To
	}
	return out
}

// for=4s: condition true at t0 → pending; still pending at t0+2s;
// firing once for has elapsed at t0+4s.
func TestPendingUntilForElapses(t *testing.T) {
	ev, st, clock := newHarness(4 * time.Second)

	setValue(st, clock, 0.95)
	trs, err := ev.EvalOnce()
	if err != nil {
		t.Fatal(err)
	}
	if len(trs) != 1 || trs[0].To != engine.StatePending {
		t.Fatalf("t0: transitions = %v, want one →pending", statesOf(trs))
	}
	if trs[0].ShouldNotify() {
		t.Error("H2 violated: pending transition must not notify")
	}

	clock.Advance(2 * time.Second)
	setValue(st, clock, 0.95)
	trs, _ = ev.EvalOnce()
	if len(trs) != 0 {
		t.Fatalf("t0+2s: transitions = %v, want none (still pending)", statesOf(trs))
	}
	if got := ev.States.Get("HighCPU", map[string]string{"job": "api", "host": "a"}); got.State != engine.StatePending {
		t.Fatalf("t0+2s: state = %s, want pending", got.State)
	}

	clock.Advance(2 * time.Second) // t0+4s: for elapsed
	setValue(st, clock, 0.95)
	trs, _ = ev.EvalOnce()
	if len(trs) != 1 || trs[0].To != engine.StateFiring {
		t.Fatalf("t0+4s: transitions = %v, want one →firing", statesOf(trs))
	}
	if !trs[0].ShouldNotify() {
		t.Error("firing transition must notify")
	}
}

// for=0: straight to firing on the first breaching tick.
func TestZeroForFiresImmediately(t *testing.T) {
	ev, st, clock := newHarness(0)
	setValue(st, clock, 0.95)
	trs, _ := ev.EvalOnce()
	if len(trs) != 1 || trs[0].To != engine.StateFiring {
		t.Fatalf("transitions = %v, want one →firing", statesOf(trs))
	}
}

// Condition clears while pending → back to normal, never fired.
func TestClearFromPendingReturnsToNormal(t *testing.T) {
	ev, st, clock := newHarness(4 * time.Second)

	setValue(st, clock, 0.95)
	if trs, _ := ev.EvalOnce(); len(trs) != 1 || trs[0].To != engine.StatePending {
		t.Fatalf("t0: want →pending, got %v", statesOf(trs))
	}

	clock.Advance(time.Second)
	setValue(st, clock, 0.10) // clears before for elapses
	trs, _ := ev.EvalOnce()
	if len(trs) != 1 || trs[0].To != engine.StateNormal {
		t.Fatalf("t0+1s: transitions = %v, want one →normal", statesOf(trs))
	}
	if trs[0].ShouldNotify() {
		t.Error("H2 violated: pending→normal must not notify")
	}
}

// firing → clear → resolved (notified) → next tick rests at normal.
func TestFiringThenClearResolves(t *testing.T) {
	ev, st, clock := newHarness(0)

	setValue(st, clock, 0.95)
	if trs, _ := ev.EvalOnce(); len(trs) != 1 || trs[0].To != engine.StateFiring {
		t.Fatalf("t0: want →firing, got %v", statesOf(trs))
	}

	clock.Advance(time.Second)
	setValue(st, clock, 0.10)
	trs, _ := ev.EvalOnce()
	if len(trs) != 1 || trs[0].To != engine.StateResolved {
		t.Fatalf("t0+1s: transitions = %v, want one →resolved", statesOf(trs))
	}
	if !trs[0].ShouldNotify() {
		t.Error("resolved transition must notify (H2: firing AND resolved leave the engine)")
	}

	clock.Advance(time.Second)
	trs, _ = ev.EvalOnce()
	if len(trs) != 1 || trs[0].To != engine.StateNormal {
		 t.Fatalf("t0+2s: transitions = %v, want one →normal (resolve acknowledged)", statesOf(trs))
	}
}

// resolved → firing again is a fresh firing period (refire).
func TestRefireAfterResolve(t *testing.T) {
	ev, st, clock := newHarness(0)

	setValue(st, clock, 0.95)
	ev.EvalOnce() // firing
	clock.Advance(time.Second)
	setValue(st, clock, 0.10)
	ev.EvalOnce() // resolved
	clock.Advance(time.Second)
	ev.EvalOnce() // normal

	clock.Advance(time.Second)
	setValue(st, clock, 0.99)
	trs, _ := ev.EvalOnce()
	if len(trs) != 1 || trs[0].To != engine.StateFiring {
		t.Fatalf("refire: transitions = %v, want one →firing", statesOf(trs))
	}
	if !trs[0].Instance.FiredAt.Equal(clock.Now()) {
		t.Errorf("refire should start a new firing period: FiredAt = %v, want %v",
			trs[0].Instance.FiredAt, clock.Now())
	}
}

// Instance identity: label map order must not matter (canonical key).
func TestInstanceKeyIsLabelOrderIndependent(t *testing.T) {
	ev, st, clock := newHarness(0)
	st.Add(metrics.Sample{Metric: "cpu_usage", Labels: map[string]string{"job": "api", "host": "a"}, T: clock.Now(), V: 0.95})
	if _, err := ev.EvalOnce(); err != nil {
		t.Fatal(err)
	}

	// Same series, labels constructed in a different order.
	inst := ev.States.Get("HighCPU", map[string]string{"host": "a", "job": "api"})
	if inst == nil {
		t.Fatal("instance not found with reordered labels")
	}
	if inst.State != engine.StateFiring {
		t.Errorf("state = %s, want firing", inst.State)
	}
	if ev.States.Len() != 1 {
		t.Errorf("instances = %d, want 1", ev.States.Len())
	}
}
