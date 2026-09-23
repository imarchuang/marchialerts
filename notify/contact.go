// Package notify holds the contact points: dumb ingress adapters to
// channels (H4). No grouping, no dedup, no timers live here — the AM
// pipeline hands each adapter a fully-formed group payload.
package notify

import (
	"context"
	"time"

	"marchialerts/am"
)

// ContactPoint is the adapter interface (AM notify.Integration, thinned).
type ContactPoint interface {
	Name() string
	// Notify delivers one group's payload. retryable=false means the error
	// is permanent (e.g. 4xx) and the retry wrapper should give up.
	Notify(ctx context.Context, p Payload) (retryable bool, err error)
}

// Payload is the Grafana-ish webhook body: a GROUP of alerts, not a single
// instance (H7). status is "firing" when any alert is firing, else
// "resolved".
type Payload struct {
	Receiver string  `json:"receiver"`
	Status   string  `json:"status"`
	GroupKey string  `json:"groupKey"`
	Alerts   []Alert `json:"alerts"`
}

// Alert is one alert in the payload.
type Alert struct {
	Status      string            `json:"status"` // firing | resolved
	Labels      map[string]string `json:"labels"`
	Fingerprint string            `json:"fingerprint"` // %016x
	StartsAt    time.Time         `json:"startsAt"`
	EndsAt      time.Time         `json:"endsAt,omitempty"`
}

// BuildPayload converts a group's alerts into the wire payload.
func BuildPayload(receiver, groupKey string, alerts []am.PostableAlert, now time.Time) Payload {
	p := Payload{Receiver: receiver, GroupKey: groupKey, Status: "resolved"}
	for _, a := range alerts {
		status := "firing"
		if a.ResolvedAt(now) {
			status = "resolved"
		} else {
			p.Status = "firing"
		}
		p.Alerts = append(p.Alerts, Alert{
			Status:      status,
			Labels:      a.Labels,
			Fingerprint: a.Fingerprint().String(),
			StartsAt:    a.StartsAt,
			EndsAt:      a.EndsAt,
		})
	}
	return p
}

// Fanout delivers the same payload to every contact point (AM FanoutStage).
// The first error is returned so the group is retried at the next flush.
type Fanout []ContactPoint

// Notify implements am.Notifier.
func (f Fanout) Notify(groupKey string, alerts []am.PostableAlert) error {
	now := time.Now()
	for _, cp := range f {
		p := BuildPayload(cp.Name(), groupKey, alerts, now)
		if _, err := cp.Notify(context.Background(), p); err != nil {
			return err
		}
	}
	return nil
}
