// Package am is the in-process Alertmanager: postable alerts, receiver-side
// fingerprints, and (from PR4) aggregation groups and the notify pipeline.
package am

import (
	"fmt"
	"hash/fnv"
	"sort"
	"time"
)

// separatorByte cannot occur in valid UTF-8 and separates label names and
// values in the hash input — same trick as prometheus/common/model.
const separatorByte byte = 255

// PostableAlert is what crosses the engine→AM boundary (PutAlerts).
// Labels are the identity: there is NO fingerprint field on the wire — the
// receiver derives it. Annotations never participate in identity.
type PostableAlert struct {
	Labels      map[string]string
	Annotations map[string]string
	StartsAt    time.Time
	EndsAt      time.Time // rolling deadline while firing; set on resolve
}

// Fingerprint is the receiver-side alert identity: FNV-1a 64 over sorted
// name 0xff value 0xff pairs of the canonical labels (empty names/values
// dropped — the AM would reject invalid label sets anyway). Rendered as
// %016x like Prometheus.
type Fingerprint uint64

func (f Fingerprint) String() string { return fmt.Sprintf("%016x", uint64(f)) }

// Canonicalize returns a copy of labels with empty names/values dropped.
func Canonicalize(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		if k == "" || v == "" {
			continue
		}
		out[k] = v
	}
	return out
}

// FingerprintLabels hashes a label set. Label map order is irrelevant.
func FingerprintLabels(labels map[string]string) Fingerprint {
	labels = Canonicalize(labels)
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := fnv.New64a()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{separatorByte})
		h.Write([]byte(labels[k]))
		h.Write([]byte{separatorByte})
	}
	return Fingerprint(h.Sum64())
}

// Fingerprint returns this alert's identity.
func (a PostableAlert) Fingerprint() Fingerprint {
	return FingerprintLabels(a.Labels)
}

// ResolvedAt reports whether the alert is resolved at t (AM semantics:
// now > EndsAt).
func (a PostableAlert) ResolvedAt(t time.Time) bool {
	return !a.EndsAt.IsZero() && a.EndsAt.Before(t)
}

// Merge folds o into a (same fingerprint assumed): earliest StartsAt wins,
// latest EndsAt wins, annotations come from the latest copy.
// Mirrors AM types.Alert.Merge.
func (a PostableAlert) Merge(o PostableAlert) PostableAlert {
	res := o
	if a.StartsAt.Before(o.StartsAt) {
		res.StartsAt = a.StartsAt
	}
	if o.EndsAt.After(a.EndsAt) {
		res.EndsAt = o.EndsAt
	}
	return res
}

// Receiver is the AM-side entry point: PutAlerts. The engine must only ever
// talk to this — never to a contact point (H1).
type Receiver interface {
	PutAlerts(alerts ...PostableAlert)
}

// Stub is a Receiver that records what it received, for tests and for the
// PR3 smoke (real groups arrive in PR4).
type Stub struct {
	Received []PostableAlert
}

// PutAlerts records the batch.
func (s *Stub) PutAlerts(alerts ...PostableAlert) {
	s.Received = append(s.Received, alerts...)
}
