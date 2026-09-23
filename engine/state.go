package engine

import (
	"time"

	"marchialerts/metrics"
)

// State is the instance state machine: normal → pending → firing →
// resolved → normal. Grafana adds recovering (keep_firing_for) — PR7.
type State int

const (
	StateNormal State = iota
	StatePending
	StateFiring
	StateResolved
)
func (s State) String() string {
	switch s {
	case StateNormal:
		return "normal"
	case StatePending:
		return "pending"
	case StateFiring:
		return "firing"
	case StateResolved:
		return "resolved"
	default:
		return "unknown"
	}
}

// Instance is one (rule × label set) state machine — Grafana's alert
// instance. The key is canonical: label map order cannot change identity.
type Instance struct {
	RuleAlert string
	Labels    map[string]string
	State     State
	Since     time.Time // when the current state started
	FiredAt   time.Time // when it last entered firing
	LastValue float64
	LastEval  time.Time
}

// Key returns rule name + canonical labels.
func (i *Instance) Key() string {
	return i.RuleAlert + "{" + metrics.LabelsString(i.Labels) + "}"
}

// Transition records one state change of an instance.
type Transition struct {
	Instance Instance // snapshot after the transition
	Rule     Rule
	From, To State
	At       time.Time
}

// ShouldNotify encodes H2: only firing and resolved ever leave the engine.
// Pending is engine-internal and must never reach a contact point.
func (t Transition) ShouldNotify() bool {
	return t.To == StateFiring || t.To == StateResolved
}

// StateManager owns the instance map. Instances are never garbage collected
// yet — stale-series cleanup is a later concern (Grafana: MissingSeries).
type StateManager struct {
	instances map[string]*Instance
}

// NewStateManager returns an empty manager.
func NewStateManager() *StateManager {
	return &StateManager{instances: make(map[string]*Instance)}
}

// Get returns the current instance for inspection (nil if unknown).
func (sm *StateManager) Get(ruleAlert string, labels map[string]string) *Instance {
	key := ruleAlert + "{" + metrics.LabelsString(labels) + "}"
	if inst, ok := sm.instances[key]; ok {
		cp := *inst
		return &cp
	}
	return nil
}

// Len returns the number of tracked instances.
func (sm *StateManager) Len() int { return len(sm.instances) }

// Apply folds one eval result into the instance's state machine and returns
// the resulting transition (From == To when the state did not change).
func (sm *StateManager) Apply(rule Rule, labels map[string]string, value float64, firing bool, now time.Time) Transition {
	key := rule.Alert + "{" + metrics.LabelsString(labels) + "}"
	inst, ok := sm.instances[key]
	if !ok {
		inst = &Instance{RuleAlert: rule.Alert, Labels: labels, State: StateNormal, Since: now}
		sm.instances[key] = inst
	}

	from := inst.State
	switch {
	case from == StateNormal && firing:
		if rule.For > 0 {
			inst.State, inst.Since = StatePending, now
		} else {
			inst.State, inst.Since = StateFiring, now
			inst.FiredAt = now
		}
	case from == StatePending && firing:
		if !inst.Since.Add(rule.For).After(now) {
			inst.State = StateFiring
			inst.FiredAt = now
		}
	case from == StatePending && !firing:
		inst.State, inst.Since = StateNormal, now
	case from == StateFiring && !firing:
		inst.State, inst.Since = StateResolved, now
	case from == StateResolved && firing:
		// Refire: a fresh firing period.
		inst.State, inst.Since = StateFiring, now
		inst.FiredAt = now
	case from == StateResolved && !firing:
		// Resolve is a one-tick event; the instance rests at normal.
		inst.State, inst.Since = StateNormal, now
	}
	inst.LastValue, inst.LastEval = value, now

	return Transition{Instance: *inst, Rule: rule, From: from, To: inst.State, At: now}
}
