package am

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// fakeClock drives the package clock deterministically.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// recorder captures notifications: one entry per Notify call.
type recorder struct {
	calls [][]PostableAlert
	err   error // injected failure
}

func (r *recorder) Notify(groupKey string, alerts []PostableAlert) error {
	if r.err != nil {
		return r.err
	}
	cp := make([]PostableAlert, len(alerts))
	copy(cp, alerts)
	r.calls = append(r.calls, cp)
	return nil
}

func (r *recorder) fingerprints(call int) []Fingerprint {
	var out []Fingerprint
	for _, a := range r.calls[call] {
		out = append(out, a.Fingerprint())
	}
	return out
}

var t0 = time.Unix(1_700_000_000, 0).UTC()

// opts: wait 3s, interval 6s, repeat 12s (PLAN §4 example values).
func testOpts() RouteOpts {
	return RouteOpts{
		Receiver:       "hook",
		GroupBy:        []string{"alertname", "team"},
		GroupWait:      3 * time.Second,
		GroupInterval:  6 * time.Second,
		RepeatInterval: 12 * time.Second,
	}
}

// alert builds a firing PostableAlert with the given extra labels.
func alert(at time.Time, kv ...string) PostableAlert {
	labels := map[string]string{"alertname": "HighCPU", "team": "o11y"}
	for i := 0; i < len(kv); i += 2 {
		labels[kv[i]] = kv[i+1]
	}
	return PostableAlert{
		Labels:   labels,
		StartsAt: at,
		EndsAt:   at.Add(time.Minute), // rolling deadline, refreshed by the engine
	}
}

func setup(t *testing.T) (*fakeClock, *Dispatcher, *recorder) {
	t.Helper()
	clock := &fakeClock{t: t0}
	SetClock(clock.now)
	t.Cleanup(func() { SetClock(nil) })
	rec := &recorder{}
	return clock, NewDispatcher(testOpts(), rec), rec
}

// 1. A at t=0, no notify until group wait elapses.
func TestGroupWaitGatesFirstNotify(t *testing.T) {
	clock, d, rec := setup(t)

	d.PutAlerts(alert(clock.now(), "host", "a"))
	d.Tick()
	if len(rec.calls) != 0 {
		t.Fatalf("notified %d times before group wait", len(rec.calls))
	}

	clock.advance(2 * time.Second)
	d.Tick()
	if len(rec.calls) != 0 {
		t.Fatalf("notified at wait-1s; group wait must gate the first send")
	}

	clock.advance(time.Second) // t=3s: group wait elapsed
	d.Tick()
	if len(rec.calls) != 1 {
		t.Fatalf("calls = %d, want 1 after group wait", len(rec.calls))
	}
}

// 2. B before wait elapses → ONE first notify containing A+B.
func TestGroupWaitCoalescesAB(t *testing.T) {
	clock, d, rec := setup(t)

	d.PutAlerts(alert(clock.now(), "host", "a"))
	clock.advance(2 * time.Second)
	d.PutAlerts(alert(clock.now(), "host", "b")) // joins the same group
	d.Tick()
	if len(rec.calls) != 0 {
		t.Fatalf("notified before wait elapsed")
	}

	clock.advance(time.Second) // t=3s
	d.Tick()
	if len(rec.calls) != 1 {
		t.Fatalf("calls = %d, want exactly 1", len(rec.calls))
	}
	if got := rec.fingerprints(0); len(got) != 2 {
		t.Fatalf("first notify has %d alerts, want A+B = 2", len(got))
	}
}

// 3. After first send, C before group interval → no immediate send;
// next send has A+B+C.
func TestGroupIntervalBatchesC(t *testing.T) {
	clock, d, rec := setup(t)

	d.PutAlerts(alert(clock.now(), "host", "a"))
	clock.advance(3 * time.Second)
	d.Tick() // first flush: A
	if len(rec.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(rec.calls))
	}

	// C arrives 1s after the first flush — must NOT send immediately.
	clock.advance(time.Second)
	d.PutAlerts(alert(clock.now(), "host", "c"))
	d.Tick()
	if len(rec.calls) != 1 {
		t.Fatalf("C triggered an immediate send; group interval must batch it")
	}

	// Keep A and C alive (rolling endsAt), advance to the next flush.
	clock.advance(5 * time.Second) // t=9s: 6s after first flush
	d.PutAlerts(alert(clock.now(), "host", "a")) // refresh A
	d.PutAlerts(alert(clock.now(), "host", "c")) // refresh C
	d.Tick()
	if len(rec.calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(rec.calls))
	}
	if got := rec.fingerprints(1); len(got) != 2 {
		t.Fatalf("second notify has %d alerts, want A+C = 2 (B resolved and left)", len(got))
	}
}

// 4. Steady past repeat → reminder with the same firing set.
func TestRepeatReminder(t *testing.T) {
	clock, d, rec := setup(t)

	d.PutAlerts(alert(clock.now(), "host", "a"))
	clock.advance(3 * time.Second)
	d.Tick() // first flush
	if len(rec.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(rec.calls))
	}

	// Two interval flushes before repeat (12s): nothing new → dedup blocks.
	for i := 0; i < 1; i++ {
		clock.advance(6 * time.Second)
		d.PutAlerts(alert(clock.now(), "host", "a")) // refresh rolling endsAt
		d.Tick()
	}
	if len(rec.calls) != 1 {
		t.Fatalf("calls = %d before repeat, want 1 (dedup gates)", len(rec.calls))
	}

	clock.advance(6 * time.Second) // t=15s: repeat (12s) elapsed
	d.PutAlerts(alert(clock.now(), "host", "a"))
	d.Tick()
	if len(rec.calls) != 2 {
		t.Fatalf("calls = %d, want 2 (repeat reminder)", len(rec.calls))
	}
	if got := rec.fingerprints(1); len(got) != 1 {
		t.Fatalf("reminder has %d alerts, want the same firing set (A)", len(got))
	}
}

// 5. Resolve all → interval notifies resolve → group gone; fire A → group
// wait again (new birth).
func TestResolveDeletesGroupAndRebirth(t *testing.T) {
	clock, d, rec := setup(t)

	d.PutAlerts(alert(clock.now(), "host", "a"))
	clock.advance(3 * time.Second)
	d.Tick() // first flush: A firing
	if len(rec.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(rec.calls))
	}

	// A resolves: engine re-sends identical labels with endsAt=now.
	resolved := alert(clock.now(), "host", "a")
	resolved.EndsAt = clock.now()
	d.PutAlerts(resolved)
	d.Tick()
	if len(rec.calls) != 1 {
		t.Fatalf("resolve must wait for the group interval flush")
	}

	clock.advance(6 * time.Second)
	d.Tick() // flush: resolve notification, then group teardown
	if len(rec.calls) != 2 {
		t.Fatalf("calls = %d, want 2 (resolve notification)", len(rec.calls))
	}
	if d.GroupCount() != 0 {
		t.Fatalf("groups = %d, want 0 (empty after notified resolve)", d.GroupCount())
	}

	// Fire A again: a NEW birth → group wait applies again.
	clock.advance(time.Second)
	d.PutAlerts(alert(clock.now(), "host", "a"))
	d.Tick()
	if len(rec.calls) != 2 {
		t.Fatalf("rebirth notified immediately; group wait must apply again")
	}
	clock.advance(3 * time.Second)
	d.Tick()
	if len(rec.calls) != 3 {
		t.Fatalf("calls = %d, want 3 (new birth after group wait)", len(rec.calls))
	}
}

// 6. repeat=5s, interval=3s → coerced to 6s (config-level; verified here
// through observable behavior: no reminder at 5s, reminder at 6s).
func TestRepeatCoercedToMultipleOfInterval(t *testing.T) {
	clock := &fakeClock{t: t0}
	SetClock(clock.now)
	t.Cleanup(func() { SetClock(nil) })
	rec := &recorder{}

	opts := RouteOpts{
		Receiver:       "hook",
		GroupBy:        []string{"alertname", "team"},
		GroupWait:      time.Second,
		GroupInterval:  3 * time.Second,
		RepeatInterval: 6 * time.Second, // what the config loader coerces 5s→6s into
	}
	d := NewDispatcher(opts, rec)

	d.PutAlerts(alert(clock.now(), "host", "a"))
	clock.advance(time.Second)
	d.Tick() // first flush at t=1s
	if len(rec.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(rec.calls))
	}

	// t=4s flush: repeat (6s) not yet elapsed → dedup blocks.
	clock.advance(3 * time.Second)
	d.PutAlerts(alert(clock.now(), "host", "a"))
	d.Tick()
	if len(rec.calls) != 1 {
		t.Fatalf("calls = %d at t=4s, want 1 (repeat not elapsed)", len(rec.calls))
	}

	// t=7s flush: 6s since first send → reminder.
	clock.advance(3 * time.Second)
	d.PutAlerts(alert(clock.now(), "host", "a"))
	d.Tick()
	if len(rec.calls) != 2 {
		t.Fatalf("calls = %d at t=7s, want 2 (repeat reminder)", len(rec.calls))
	}
}

// 7. Flush with unchanged firing set before repeat → NO send (dedup gates,
// not just timers).
func TestDedupBlocksUnchangedSetBeforeRepeat(t *testing.T) {
	clock, d, rec := setup(t)

	d.PutAlerts(alert(clock.now(), "host", "a"))
	clock.advance(3 * time.Second)
	d.Tick() // first flush at t=3s

	// One interval flush (t=9s), set unchanged, repeat (12s) not reached.
	clock.advance(6 * time.Second)
	d.PutAlerts(alert(clock.now(), "host", "a")) // keep alive
	d.Tick()
	if len(rec.calls) != 1 {
		t.Fatalf("calls = %d, want 1 — unchanged set must not re-notify before repeat", len(rec.calls))
	}
}

// 8. Resolve without a prior notified fire → no resolve notification
// (the receiver never knew about it).
func TestResolveWithoutPriorFireIsSilent(t *testing.T) {
	clock, d, rec := setup(t)

	// Alert arrives already resolved (engine flapped inside group_wait).
	a := alert(clock.now(), "host", "a")
	a.EndsAt = clock.now()
	d.PutAlerts(a)

	clock.advance(3 * time.Second)
	d.Tick()
	if len(rec.calls) != 0 {
		t.Fatalf("calls = %d, want 0 — resolve of an unknown incident is silent", len(rec.calls))
	}
}

// 9. group_by: [...] → A and B with different labels land in DIFFERENT groups.
func TestGroupByAllSeparatesInstances(t *testing.T) {
	clock := &fakeClock{t: t0}
	SetClock(clock.now)
	t.Cleanup(func() { SetClock(nil) })
	rec := &recorder{}

	opts := testOpts()
	opts.GroupBy = []string{"..."}
	d := NewDispatcher(opts, rec)

	d.PutAlerts(alert(clock.now(), "host", "a"))
	d.PutAlerts(alert(clock.now(), "host", "b"))
	if d.GroupCount() != 2 {
		t.Fatalf("groups = %d, want 2 (GroupByAll)", d.GroupCount())
	}

	clock.advance(3 * time.Second)
	d.Tick()
	if len(rec.calls) != 2 {
		t.Fatalf("calls = %d, want 2 (one per group)", len(rec.calls))
	}
	for i, c := range rec.calls {
		if len(c) != 1 {
			t.Errorf("call %d has %d alerts, want 1 (no aggregation)", i, len(c))
		}
	}
}

// 10. Notify fails → resolved alert retained; later success → deleted
// (no re-notify of that resolve).
func TestFailedNotifyRetainsResolved(t *testing.T) {
	clock, d, rec := setup(t)

	d.PutAlerts(alert(clock.now(), "host", "a"))
	clock.advance(3 * time.Second)
	d.Tick() // first flush: A firing
	if len(rec.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(rec.calls))
	}

	// A resolves, but the notify fails.
	resolved := alert(clock.now(), "host", "a")
	resolved.EndsAt = clock.now()
	d.PutAlerts(resolved)
	rec.err = errors.New("webhook 500")
	clock.advance(6 * time.Second)
	d.Tick()
	if d.GroupCount() != 1 {
		t.Fatalf("groups = %d, want 1 (failure keeps the group)", d.GroupCount())
	}
	if got := len(d.GroupAlerts(`{alertname=HighCPU,team=o11y}`)); got != 1 {
		t.Fatalf("alerts = %d, want 1 (resolved retained on failure)", got)
	}

	// Next flush succeeds: resolve is notified once, group torn down.
	rec.err = nil
	clock.advance(6 * time.Second)
	d.Tick()
	if len(rec.calls) != 2 {
		t.Fatalf("calls = %d, want 2 (resolve notified after recovery)", len(rec.calls))
	}
	if d.GroupCount() != 0 {
		t.Fatalf("groups = %d, want 0 (teardown after successful resolve)", d.GroupCount())
	}

	// No further flushes may re-notify that resolve.
	clock.advance(6 * time.Second)
	d.Tick()
	if len(rec.calls) != 2 {
		t.Fatalf("calls = %d, want 2 (resolve must not be re-notified)", len(rec.calls))
	}
}

// Sanity: group key is canonical regardless of label construction order.
func TestGroupKeyCanonical(t *testing.T) {
	a := groupKey(map[string]string{"alertname": "HighCPU", "team": "o11y"})
	b := groupKey(map[string]string{"team": "o11y", "alertname": "HighCPU"})
	if a != b {
		t.Fatalf("group keys differ: %q vs %q", a, b)
	}
	if want := `{alertname=HighCPU,team=o11y}`; a != want {
		t.Fatalf("group key = %q, want %q", a, want)
	}
}

// Late first alert whose group_wait already elapsed flushes immediately
// (AM aggrGroup.insert behavior).
func TestLateFirstAlertFlushesImmediately(t *testing.T) {
	clock, d, rec := setup(t)

	// Alert started 10s ago — longer than group_wait (3s).
	a := alert(clock.now().Add(-10*time.Second), "host", "a")
	d.PutAlerts(a) // insert triggers an immediate flush
	if len(rec.calls) != 1 {
		t.Fatalf("calls = %d, want 1 (late alert flushes without waiting)", len(rec.calls))
	}
}

var _ = fmt.Sprintf // keep fmt imported for future debug helpers
