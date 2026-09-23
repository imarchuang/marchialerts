package engine

import (
	"time"

	"marchialerts/am"
)

// resendDelay is the minimum rolling endsAt horizon for firing alerts —
// Grafana's ResendDelay, so the AM never auto-resolves between ticks.
const resendDelay = time.Minute

// Sender is the ONLY egress from the engine (H1): it converts state
// transitions into PostableAlerts and hands them to the AM via PutAlerts.
// Contact points are never called from here — that is the AM's job.
type Sender struct {
	AM am.Receiver
}

// labelsFor builds the alert's identity labels: instance labels + rule
// labels + alertname (Grafana's extra-labels idea, minus org/folder).
func labelsFor(tr Transition) map[string]string {
	labels := make(map[string]string, len(tr.Instance.Labels)+len(tr.Rule.Labels)+1)
	for k, v := range tr.Instance.Labels {
		labels[k] = v
	}
	for k, v := range tr.Rule.Labels {
		labels[k] = v
	}
	labels["alertname"] = tr.Rule.Alert
	return am.Canonicalize(labels)
}

// ToPostable converts one transition. Returns ok=false when the transition
// must not cross the boundary (H2: pending/normal stay in the engine).
func (s *Sender) ToPostable(tr Transition) (a am.PostableAlert, ok bool) {
	if !tr.ShouldNotify() {
		return am.PostableAlert{}, false
	}
	a = am.PostableAlert{
		Labels:   labelsFor(tr),
		StartsAt: tr.Instance.FiredAt,
	}
	switch tr.To {
	case StateFiring:
		// Rolling deadline: refreshed every tick so the AM does not
		// auto-resolve between evaluations (Grafana Maintain).
		a.EndsAt = tr.At.Add(resendDelay)
	case StateResolved:
		// Resolve reuses the IDENTICAL label set so the fingerprint
		// matches and expires the previous alert.
		a.EndsAt = tr.At
	}
	return a, true
}

// Send converts all notifiable transitions and puts them to the AM.
// Returns how many alerts were put.
func (s *Sender) Send(transitions []Transition) int {
	if s.AM == nil {
		return 0
	}
	var batch []am.PostableAlert
	for _, tr := range transitions {
		if a, ok := s.ToPostable(tr); ok {
			batch = append(batch, a)
		}
	}
	if len(batch) > 0 {
		s.AM.PutAlerts(batch...)
	}
	return len(batch)
}
