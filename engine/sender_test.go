package engine_test

import (
	"testing"
	"time"

	"marchialerts/am"
	"marchialerts/engine"
	"marchialerts/metrics"
)

type senderHarness struct {
	ev    *engine.Evaluator
	st    *metrics.Store
	clock *fakeClock
	stub  *am.Stub
}

func newSenderHarness(forDur time.Duration) *senderHarness {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0).UTC()}
	st := metrics.NewStore()
	stub := &am.Stub{}
	rule := engine.Rule{
		Alert:  "HighCPU",
		Expr:   engine.Expr{Metric: "cpu_usage", Labels: map[string]string{"job": "api"}, Op: ">", Threshold: 0.8},
		For:    forDur,
		Labels: map[string]string{"team": "o11y", "severity": "warning"},
	}
	ev := &engine.Evaluator{
		Rules:  []engine.Rule{rule},
		Store:  st,
		States: engine.NewStateManager(),
		Sender: &engine.Sender{AM: stub},
		Now:    clock.Now,
	}
	return &senderHarness{ev: ev, st: st, clock: clock, stub: stub}
}

func (h *senderHarness) setValue(v float64) {
	h.st.Add(metrics.Sample{
		Metric: "cpu_usage",
		Labels: map[string]string{"job": "api", "host": "a"},
		T:      h.clock.Now(),
		V:      v,
	})
}

// H1/H2 acceptance: a pending tick puts ZERO alerts on the wire.
func TestPendingTickSendsNothing(t *testing.T) {
	h := newSenderHarness(4 * time.Second)
	h.setValue(0.95)

	trs, err := h.ev.EvalOnce()
	if err != nil {
		t.Fatal(err)
	}
	if len(trs) != 1 || trs[0].To != engine.StatePending {
		t.Fatalf("want one →pending transition, got %v", statesOf(trs))
	}
	if len(h.stub.Received) != 0 {
		t.Fatalf("H2 violated: pending produced %d PutAlerts", len(h.stub.Received))
	}
}

// Firing → exactly one alert, with identity labels and a rolling endsAt.
func TestFiringSendsOneAlert(t *testing.T) {
	h := newSenderHarness(0)
	h.setValue(0.95)
	if _, err := h.ev.EvalOnce(); err != nil {
		t.Fatal(err)
	}

	if len(h.stub.Received) != 1 {
		t.Fatalf("received = %d, want 1", len(h.stub.Received))
	}
	a := h.stub.Received[0]
	if a.Labels["alertname"] != "HighCPU" || a.Labels["host"] != "a" ||
		a.Labels["team"] != "o11y" || a.Labels["severity"] != "warning" {
		t.Errorf("labels = %v", a.Labels)
	}
	if !a.StartsAt.Equal(h.clock.Now()) {
		t.Errorf("StartsAt = %v, want %v", a.StartsAt, h.clock.Now())
	}
	// Rolling deadline: endsAt in the future so the AM won't auto-resolve.
	if !a.EndsAt.After(h.clock.Now()) {
		t.Errorf("EndsAt = %v, want > now (rolling)", a.EndsAt)
	}
}

// Resolved → exactly one alert with endsAt set to now, same fingerprint as
// the firing alert (so the AM can expire it).
func TestResolvedSendsOneAlertWithEndsAt(t *testing.T) {
	h := newSenderHarness(0)
	h.setValue(0.95)
	h.ev.EvalOnce() // firing
	firing := h.stub.Received[0]

	h.clock.Advance(time.Second)
	h.setValue(0.10)
	if _, err := h.ev.EvalOnce(); err != nil {
		t.Fatal(err)
	}

	if len(h.stub.Received) != 2 {
		t.Fatalf("received = %d, want 2 (fire + resolve)", len(h.stub.Received))
	}
	res := h.stub.Received[1]
	if !res.EndsAt.Equal(h.clock.Now()) {
		t.Errorf("resolve EndsAt = %v, want now %v", res.EndsAt, h.clock.Now())
	}
	if res.Fingerprint() != firing.Fingerprint() {
		t.Errorf("resolve fingerprint %v != firing fingerprint %v — labels must be identical",
			res.Fingerprint(), firing.Fingerprint())
	}
}

// H1: pending → normal (cleared before for) never touches the AM.
func TestPendingToNormalSendsNothing(t *testing.T) {
	h := newSenderHarness(4 * time.Second)
	h.setValue(0.95)
	h.ev.EvalOnce() // pending

	h.clock.Advance(time.Second)
	h.setValue(0.10)
	trs, _ := h.ev.EvalOnce() // pending → normal

	if len(trs) != 1 || trs[0].To != engine.StateNormal {
		t.Fatalf("want →normal, got %v", statesOf(trs))
	}
	if len(h.stub.Received) != 0 {
		t.Fatalf("received = %d, want 0", len(h.stub.Received))
	}
}

// Steady firing produces no transitions, hence no re-sends from the engine.
// (AM-side repeat reminders are the nflog's job — PR4.)
func TestSteadyFiringNoResend(t *testing.T) {
	h := newSenderHarness(0)
	h.setValue(0.95)
	h.ev.EvalOnce()

	h.clock.Advance(time.Second)
	h.setValue(0.96)
	trs, _ := h.ev.EvalOnce()
	if len(trs) != 0 {
		t.Fatalf("steady firing should be quiet, got %v", statesOf(trs))
	}
	if len(h.stub.Received) != 1 {
		t.Fatalf("received = %d, want 1 (no engine-side resend)", len(h.stub.Received))
	}
}
