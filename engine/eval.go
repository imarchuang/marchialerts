package engine

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"marchialerts/metrics"
)

// Evaluator ticks over the rules, compares each matching series against its
// threshold, and folds the outcome into the instance state machine (PR2).
// Only state transitions are returned/logged — steady states are quiet.
// Transitions that ShouldNotify go to the AM via the Sender (PR3): the only
// egress from the engine (H1).
type Evaluator struct {
	Rules  []Rule
	Store  *metrics.Store
	States *StateManager
	Sender *Sender
	// Now injects the clock; nil means time.Now. Tests use a fake clock —
	// never sleep in CI.
	Now func() time.Time
	Log *slog.Logger
}

func (e *Evaluator) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// EvalOnce runs one evaluation tick over all rules and returns the state
// transitions it caused (empty when nothing changed).
func (e *Evaluator) EvalOnce() ([]Transition, error) {
	if e.States == nil {
		e.States = NewStateManager()
	}
	now := e.now()
	var out []Transition
	for _, r := range e.Rules {
		for _, sm := range e.Store.Select(r.Expr.Metric, r.Expr.Labels) {
			firing, err := r.Expr.Compare(sm.V)
			if err != nil {
				return out, fmt.Errorf("rule %q: %w", r.Alert, err)
			}
			tr := e.States.Apply(r, sm.Labels, sm.V, firing, now)
			if tr.From != tr.To {
				out = append(out, tr)
			}
		}
	}
	if e.Sender != nil {
		e.Sender.Send(out)
	}
	return out, nil
}

// LogTransitions emits one line per state change.
func (e *Evaluator) LogTransitions(transitions []Transition) {
	for _, tr := range transitions {
		e.Log.Info("transition",
			"rule", tr.Instance.RuleAlert,
			"labels", metrics.LabelsString(tr.Instance.Labels),
			"from", tr.From.String(),
			"to", tr.To.String(),
			"value", tr.Instance.LastValue,
			"notify", tr.ShouldNotify(),
		)
	}
}

// Run ticks every interval until ctx is done.
func (e *Evaluator) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			transitions, err := e.EvalOnce()
			if err != nil {
				e.Log.Error("eval tick failed", "err", err)
				continue
			}
			e.LogTransitions(transitions)
		}
	}
}
