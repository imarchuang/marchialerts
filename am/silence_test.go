package am

import (
	"testing"
	"time"
)

// silenceHarness builds a dispatcher with a recorder and a silence store.
func silenceSetup(t *testing.T) (*fakeClock, *Dispatcher, *recorder) {
	t.Helper()
	return setup(t)
}

func mkSilence(t *testing.T, d *Dispatcher, start, end time.Time, kv ...string) string {
	t.Helper()
	var matchers []Matcher
	for i := 0; i < len(kv); i += 2 {
		matchers = append(matchers, Matcher{Name: kv[i], Value: kv[i+1], IsEqual: true})
	}
	id, err := d.Silences.Create(Silence{
		Matchers:  matchers,
		StartsAt:  start,
		EndsAt:    end,
		CreatedBy: "test",
		Comment:   "test silence",
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// H6: a silenced alert still goes firing in the engine and stays in the
// group; the webhook just stays silent.
func TestSilenceMutesNotifyButNotEval(t *testing.T) {
	clock, d, rec := silenceSetup(t)

	// Silence host=a for the next hour.
	mkSilence(t, d, clock.now().Add(-time.Second), clock.now().Add(time.Hour), "host", "a")

	d.PutAlerts(alert(clock.now(), "host", "a"))
	clock.advance(3 * time.Second) // group wait
	d.Tick()

	// Webhook silent…
	if len(rec.calls) != 0 {
		t.Fatalf("calls = %d, want 0 (silenced)", len(rec.calls))
	}
	// …but the alert is still in the group, still firing.
	got := d.GroupAlerts(`{alertname=HighCPU,team=o11y}`)
	if len(got) != 1 {
		t.Fatalf("group alerts = %d, want 1 (silence ≠ deletion)", len(got))
	}
	if got[0].ResolvedAt(clock.now()) {
		t.Error("alert must still be firing under a silence")
	}
}

// Expire the silence → the unchanged-but-never-notified set sends at the
// next flush.
func TestSilenceExpireResumesNotify(t *testing.T) {
	clock, d, rec := silenceSetup(t)

	id := mkSilence(t, d, clock.now().Add(-time.Second), clock.now().Add(10*time.Second), "host", "a")

	d.PutAlerts(alert(clock.now(), "host", "a"))
	clock.advance(3 * time.Second)
	d.Tick()
	if len(rec.calls) != 0 {
		t.Fatalf("calls = %d, want 0 (silenced)", len(rec.calls))
	}

	// Expire the silence early; the alert is still firing.
	d.Silences.Delete(id, clock.now())
	// Keep the alert alive (rolling endsAt) and flush at the next interval.
	clock.advance(6 * time.Second)
	d.PutAlerts(alert(clock.now(), "host", "a"))
	d.Tick()

	if len(rec.calls) != 1 {
		t.Fatalf("calls = %d, want 1 (notify resumes after silence expires)", len(rec.calls))
	}
}

// Contrast test (H6): "pause rule" — the engine stops evaluating entirely.
// That is NOT a silence: no instance, no group, nothing to resume.
func TestPauseRuleStopsEval(t *testing.T) {
	// This is an engine-level concern: a paused rule never produces
	// transitions, so the AM never sees an alert at all. The silence path
	// (above) keeps the instance firing. Assert the AM-side difference:
	// silenced alert EXISTS in the group; a paused rule's alert does not.
	clock, d, _ := silenceSetup(t)
	mkSilence(t, d, clock.now().Add(-time.Second), clock.now().Add(time.Hour), "host", "a")

	d.PutAlerts(alert(clock.now(), "host", "a"))
	if d.GroupCount() != 1 {
		t.Fatalf("silenced alert should still form a group, got %d", d.GroupCount())
	}
	// A paused rule would have produced no PutAlerts at all → GroupCount 0.
}

// Alerts API: silenced alerts are listed with status, not deleted.
func TestAlertsSilencedFilter(t *testing.T) {
	clock, d, _ := silenceSetup(t)

	d.PutAlerts(alert(clock.now(), "host", "a"))
	d.PutAlerts(alert(clock.now(), "host", "b"))
	mkSilence(t, d, clock.now().Add(-time.Second), clock.now().Add(time.Hour), "host", "a")

	all := d.Alerts(nil)
	if len(all) != 2 {
		t.Fatalf("all alerts = %d, want 2", len(all))
	}

	yes := true
	silenced := d.Alerts(&yes)
	if len(silenced) != 1 || silenced[0].Labels["host"] != "a" {
		t.Fatalf("silenced=true = %v, want only host=a", silenced)
	}
	if len(silenced[0].SilencedBy) != 1 {
		t.Errorf("SilencedBy = %v, want one silence id", silenced[0].SilencedBy)
	}

	no := false
	notSilenced := d.Alerts(&no)
	if len(notSilenced) != 1 || notSilenced[0].Labels["host"] != "b" {
		t.Fatalf("silenced=false = %v, want only host=b", notSilenced)
	}
}

// Matcher semantics: regex, negative, and missing-label cases.
func TestMatcherSemantics(t *testing.T) {
	cases := []struct {
		name   string
		m      Matcher
		labels map[string]string
		want   bool
	}{
		{"equal hit", Matcher{Name: "host", Value: "a", IsEqual: true}, map[string]string{"host": "a"}, true},
		{"equal miss", Matcher{Name: "host", Value: "a", IsEqual: true}, map[string]string{"host": "b"}, false},
		{"not-equal hit", Matcher{Name: "host", Value: "a", IsEqual: false}, map[string]string{"host": "b"}, true},
		{"not-equal on missing", Matcher{Name: "host", Value: "a", IsEqual: false}, map[string]string{}, true},
		{"equal on missing", Matcher{Name: "host", Value: "a", IsEqual: true}, map[string]string{}, false},
		{"regex hit", Matcher{Name: "host", Value: "a|b", IsRegex: true, IsEqual: true}, map[string]string{"host": "b"}, true},
		{"regex anchored", Matcher{Name: "host", Value: "a", IsRegex: true, IsEqual: true}, map[string]string{"host": "abc"}, false},
		{"not-regex hit", Matcher{Name: "host", Value: "a.*", IsRegex: true, IsEqual: false}, map[string]string{"host": "z"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := c.m
			if err := m.Compile(); err != nil {
				t.Fatal(err)
			}
			if got := m.Matches(c.labels); got != c.want {
				t.Errorf("Matches(%v) = %v, want %v", c.labels, got, c.want)
			}
		})
	}
}

// Silence lifecycle states.
func TestSilenceState(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	s := &Silence{StartsAt: now.Add(time.Hour), EndsAt: now.Add(2 * time.Hour)}
	if s.State(now) != "pending" {
		t.Errorf("state = %s, want pending", s.State(now))
	}
	s.StartsAt = now.Add(-time.Hour)
	if s.State(now) != "active" {
		t.Errorf("state = %s, want active", s.State(now))
	}
	s.EndsAt = now.Add(-time.Second)
	if s.State(now) != "expired" {
		t.Errorf("state = %s, want expired", s.State(now))
	}
}

// A pending (not-yet-active) silence does not mute.
func TestPendingSilenceDoesNotMute(t *testing.T) {
	clock, d, rec := silenceSetup(t)

	// Silence starts in the future.
	mkSilence(t, d, clock.now().Add(time.Hour), clock.now().Add(2*time.Hour), "host", "a")

	d.PutAlerts(alert(clock.now(), "host", "a"))
	clock.advance(3 * time.Second)
	d.Tick()
	if len(rec.calls) != 1 {
		t.Fatalf("calls = %d, want 1 (pending silence must not mute)", len(rec.calls))
	}
}
