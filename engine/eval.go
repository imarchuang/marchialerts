package engine

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"marchialerts/metrics"
)

// Evaluator ticks over the rules and compares each matching series against
// its threshold. PR1 is stateless: every tick logs firing/ok per
// (rule, series). Instance state (pending/for/resolved) arrives in PR2.
type Evaluator struct {
	Rules []Rule
	Store *metrics.Store
	Log   *slog.Logger
}

// Result is one evaluated (rule, series) pair — the proto-instance.
type Result struct {
	Rule   Rule
	Labels map[string]string
	Value  float64
	Firing bool
}

// EvalOnce runs one evaluation tick over all rules.
func (e *Evaluator) EvalOnce() ([]Result, error) {
	var out []Result
	for _, r := range e.Rules {
		for _, sm := range e.Store.Select(r.Expr.Metric, r.Expr.Labels) {
			firing, err := r.Expr.Compare(sm.V)
			if err != nil {
				return out, fmt.Errorf("rule %q: %w", r.Alert, err)
			}
			out = append(out, Result{Rule: r, Labels: sm.Labels, Value: sm.V, Firing: firing})
		}
	}
	return out, nil
}

// LogResults emits one line per result: firing or ok.
func (e *Evaluator) LogResults(results []Result) {
	for _, res := range results {
		state := "ok"
		if res.Firing {
			state = "firing"
		}
		e.Log.Info("eval",
			"rule", res.Rule.Alert,
			"labels", metrics.LabelsString(res.Labels),
			"value", res.Value,
			"state", state,
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
			results, err := e.EvalOnce()
			if err != nil {
				e.Log.Error("eval tick failed", "err", err)
				continue
			}
			e.LogResults(results)
		}
	}
}
