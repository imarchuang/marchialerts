package am

import (
	"testing"
	"time"
)

// Label map order must not change identity (marchimetrics SeriesID lesson,
// now at the AM boundary).
func TestFingerprintLabelOrderIndependent(t *testing.T) {
	a := FingerprintLabels(map[string]string{"job": "api", "host": "a", "alertname": "HighCPU"})
	b := FingerprintLabels(map[string]string{"alertname": "HighCPU", "host": "a", "job": "api"})
	if a != b {
		t.Fatalf("fingerprint differs by map order: %v vs %v", a, b)
	}
}

// Annotations are not identity: changing them must not produce a new alert.
func TestFingerprintIgnoresAnnotations(t *testing.T) {
	labels := map[string]string{"alertname": "HighCPU", "host": "a"}
	a := PostableAlert{Labels: labels, Annotations: map[string]string{"summary": "v1"}}
	b := PostableAlert{Labels: labels, Annotations: map[string]string{"summary": "v2", "extra": "x"}}
	if a.Fingerprint() != b.Fingerprint() {
		t.Fatalf("annotation change produced new fingerprint: %v vs %v", a.Fingerprint(), b.Fingerprint())
	}
}

// A label change is a NEW alert; the old one expires via EndsAt.
func TestFingerprintLabelChangeIsNewAlert(t *testing.T) {
	a := FingerprintLabels(map[string]string{"alertname": "HighCPU", "host": "a"})
	b := FingerprintLabels(map[string]string{"alertname": "HighCPU", "host": "b"})
	if a == b {
		t.Fatal("different label values must not share a fingerprint")
	}
}

// Resolve reuses the identical label set → same fingerprint → the firing
// alert expires. (Grafana StateToPostableAlert does this deliberately.)
func TestResolveReusesLabelsSameFingerprint(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	labels := map[string]string{"alertname": "HighCPU", "host": "a"}
	firing := PostableAlert{Labels: labels, StartsAt: now, EndsAt: now.Add(time.Minute)}
	resolved := PostableAlert{Labels: labels, StartsAt: now, EndsAt: now.Add(2 * time.Minute)}
	if firing.Fingerprint() != resolved.Fingerprint() {
		t.Fatal("resolve must reuse identical labels to keep the fingerprint")
	}
	if !resolved.ResolvedAt(now.Add(3 * time.Minute)) {
		t.Error("resolved alert should report ResolvedAt after EndsAt")
	}
}

// Empty label names/values are dropped before hashing — the AM would reject
// invalid label sets anyway (Grafana drops them in StateToPostableAlert).
func TestFingerprintDropsEmptyLabels(t *testing.T) {
	a := FingerprintLabels(map[string]string{"alertname": "HighCPU", "empty": "", "": "x"})
	b := FingerprintLabels(map[string]string{"alertname": "HighCPU"})
	if a != b {
		t.Fatalf("empty labels must be dropped: %v vs %v", a, b)
	}
}

// Merge: earliest StartsAt, latest EndsAt, annotations from the latest copy.
func TestMerge(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0).UTC()
	old := PostableAlert{
		Labels:      map[string]string{"alertname": "HighCPU"},
		Annotations: map[string]string{"summary": "old"},
		StartsAt:    t0,
		EndsAt:      t0.Add(time.Minute),
	}
	newer := PostableAlert{
		Labels:      map[string]string{"alertname": "HighCPU"},
		Annotations: map[string]string{"summary": "new"},
		StartsAt:    t0.Add(30 * time.Second),
		EndsAt:      t0.Add(2 * time.Minute),
	}
	m := old.Merge(newer)
	if !m.StartsAt.Equal(t0) {
		t.Errorf("StartsAt = %v, want earliest %v", m.StartsAt, t0)
	}
	if !m.EndsAt.Equal(t0.Add(2 * time.Minute)) {
		t.Errorf("EndsAt = %v, want latest", m.EndsAt)
	}
	if m.Annotations["summary"] != "new" {
		t.Errorf("Annotations = %v, want latest copy", m.Annotations)
	}
}

func TestResolvedAtSemantics(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	firing := PostableAlert{EndsAt: now.Add(time.Minute)}
	if firing.ResolvedAt(now) {
		t.Error("not resolved before EndsAt")
	}
	if firing.ResolvedAt(now.Add(30 * time.Second)) {
		t.Error("not resolved at EndsAt-30s")
	}
	if !firing.ResolvedAt(now.Add(61 * time.Second)) {
		t.Error("resolved once now > EndsAt")
	}
	zero := PostableAlert{}
	if zero.ResolvedAt(now.Add(time.Hour)) {
		t.Error("zero EndsAt means not resolved")
	}
}
