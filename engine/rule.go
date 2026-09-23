package engine

import (
	"fmt"
	"time"
)

// Rule is an alert rule spec: a threshold over a metric+label selector.
// A rule is only a spec — a "problem" is per label set (instance), which is
// what PR2's state machine will track.
type Rule struct {
	Alert  string
	Expr   Expr
	For    time.Duration
	Labels map[string]string
}

// Expr is a threshold condition: metric{labels...} op threshold.
type Expr struct {
	Metric    string
	Labels    map[string]string
	Op        string
	Threshold float64
}

// Compare reports whether v breaches the threshold.
func (e Expr) Compare(v float64) (bool, error) {
	switch e.Op {
	case ">":
		return v > e.Threshold, nil
	case ">=":
		return v >= e.Threshold, nil
	case "<":
		return v < e.Threshold, nil
	case "<=":
		return v <= e.Threshold, nil
	case "==":
		return v == e.Threshold, nil
	case "!=":
		return v != e.Threshold, nil
	default:
		return false, fmt.Errorf("unknown op %q", e.Op)
	}
}

// Validate checks the rule spec at load time.
func (r Rule) Validate() error {
	if r.Alert == "" {
		return fmt.Errorf("rule: alert name is required")
	}
	if r.Expr.Metric == "" {
		return fmt.Errorf("rule %q: expr.metric is required", r.Alert)
	}
	if _, err := r.Expr.Compare(0); err != nil {
		return fmt.Errorf("rule %q: %w", r.Alert, err)
	}
	if r.For < 0 {
		return fmt.Errorf("rule %q: for must be >= 0, got %s", r.Alert, r.For)
	}
	return nil
}
