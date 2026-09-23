package am

import (
	"time"
)

// now is the package clock. Tests override it with a fake clock; production
// wires time.Now via SetClock (or leaves the default).
var now = time.Now

// SetClock replaces the package clock (nil resets to time.Now). Tests use
// this to drive timers deterministically instead of sleeping.
func SetClock(f func() time.Time) {
	if f == nil {
		now = time.Now
		return
	}
	now = f
}

// Dispatcher is the in-process Alertmanager core: PutAlerts → route →
// aggregation groups → (dedup-gated) notify. Single-process; HA stages
// (gossip settle, nflog timestamp guard) are no-ops by design (PLAN §1).
type Dispatcher struct {
	opts     RouteOpts
	nflog    *Log
	notifier Notifier

	groups map[string]*aggrGroup
}

// NewDispatcher builds a dispatcher for one default route (matcher tree is
// a later PR). notifier may be nil (drops notifications — used in tests
// that only care about group lifecycle).
func NewDispatcher(opts RouteOpts, notifier Notifier) *Dispatcher {
	return &Dispatcher{
		opts:     opts,
		nflog:    NewLog(),
		notifier: notifier,
		groups:   make(map[string]*aggrGroup),
	}
}

// PutAlerts implements Receiver — the engine's only egress (H1).
func (d *Dispatcher) PutAlerts(alerts ...PostableAlert) {
	for _, a := range alerts {
		d.insert(a)
	}
}

func (d *Dispatcher) insert(a PostableAlert) {
	gl := d.opts.groupLabels(a.Labels)
	key := groupKey(gl)

	g, ok := d.groups[key]
	if !ok {
		g = &aggrGroup{
			key:      key,
			receiver: d.opts.Receiver,
			labels:   gl,
			opts:     d.opts,
			alerts:   make(map[Fingerprint]PostableAlert),
		}
		d.groups[key] = g
		// Birth: first alert of an empty group starts group wait.
		g.nextFlush = now().Add(d.opts.GroupWait)
	}
	if g.insert(a) {
		// Late first alert whose group_wait already elapsed: flush now.
		d.flush(g)
	}
}

// flush runs one timer fire for a group: dedup gate → notify → bookkeeping.
func (d *Dispatcher) flush(g *aggrGroup) {
	t := now()
	firing, resolved := g.split(t)

	entry := d.nflog.Get(g.receiver, g.key)
	send, _ := entry.NeedsUpdate(firing, resolved, d.opts.RepeatInterval, t)

	g.hasFlushed = true
	g.nextFlush = t.Add(d.opts.GroupInterval)

	if !send {
		return
	}

	if d.notifier != nil {
		if err := d.notifier.Notify(g.key, g.batch(t)); err != nil {
			// Failure: keep resolved alerts up to 3*group_interval (bounds
			// memory, mirrors AM DeleteIfStale); try again next flush.
			d.deleteResolved(g, t.Add(-3*d.opts.GroupInterval))
			return
		}
	}

	// Success: record the incident set, drop resolved alerts (AM
	// DeleteIfNotModified), and tear down an empty group so the next fire
	// is a new birth (group wait again).
	d.nflog.Set(g.receiver, g.key, firing, resolved, t)
	d.deleteResolved(g, time.Time{})
	if len(g.alerts) == 0 {
		delete(d.groups, g.key)
		d.nflog.Delete(g.receiver, g.key)
	}
}

// deleteResolved removes alerts resolved before the cutoff; a zero cutoff
// removes everything currently resolved.
func (d *Dispatcher) deleteResolved(g *aggrGroup, cutoff time.Time) {
	for fp, a := range g.alerts {
		if a.ResolvedAt(now()) && (cutoff.IsZero() || a.EndsAt.Before(cutoff)) {
			delete(g.alerts, fp)
		}
	}
}

// Tick advances every group's timer to t. Production calls it on a ticker;
// tests call it after advancing the fake clock — nobody sleeps in CI.
func (d *Dispatcher) Tick() {
	t := now()
	// Snapshot keys: flush may delete groups.
	keys := make([]string, 0, len(d.groups))
	for k := range d.groups {
		keys = append(keys, k)
	}
	for _, k := range keys {
		g, ok := d.groups[k]
		if !ok {
			continue
		}
		if !g.nextFlush.IsZero() && !t.Before(g.nextFlush) {
			d.flush(g)
		}
	}
}

// GroupCount reports how many aggregation groups exist (for tests/inspection).
func (d *Dispatcher) GroupCount() int { return len(d.groups) }

// GroupAlerts returns the current alerts of one group (nil if unknown).
func (d *Dispatcher) GroupAlerts(key string) []PostableAlert {
	g, ok := d.groups[key]
	if !ok {
		return nil
	}
	out := make([]PostableAlert, 0, len(g.alerts))
	for _, a := range g.alerts {
		out = append(out, a)
	}
	return out
}
