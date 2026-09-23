package am

import (
	"sort"
	"strings"
	"time"
)

// Notifier is the contact-point side of the pipeline. PR4 wires a recorder;
// real adapters (stdout, webhook + retry) land in PR5.
type Notifier interface {
	Notify(groupKey string, alerts []PostableAlert) error
}

// NotifierFunc adapts a function to Notifier.
type NotifierFunc func(groupKey string, alerts []PostableAlert) error

// Notify implements Notifier.
func (f NotifierFunc) Notify(groupKey string, alerts []PostableAlert) error {
	return f(groupKey, alerts)
}

// aggrGroup is one aggregation group: a living fingerprint set with three
// timers (wait on birth, interval after first send, repeat for reminders)
// and nflog dedup gating every flush. Mirrors AM dispatch.aggrGroup.
type aggrGroup struct {
	key      string
	receiver string
	labels   map[string]string
	opts     RouteOpts

	alerts     map[Fingerprint]PostableAlert
	hasFlushed bool
	// nextFlush is the wall time of the next timer fire; zero until the
	// first alert arrives (birth starts group wait).
	nextFlush time.Time
}

// insert adds or merges an alert. A late first alert whose group_wait has
// already elapsed flushes immediately (AM aggrGroup.insert).
func (g *aggrGroup) insert(a PostableAlert) (flushNow bool) {
	fp := a.Fingerprint()
	if cur, ok := g.alerts[fp]; ok {
		g.alerts[fp] = cur.Merge(a)
	} else {
		g.alerts[fp] = a
	}
	if !g.hasFlushed && a.StartsAt.Add(g.opts.GroupWait).Before(now()) {
		return true
	}
	return false
}

// split returns firing vs resolved fingerprint sets at the flush time.
func (g *aggrGroup) split(now time.Time) (firing, resolved map[Fingerprint]struct{}) {
	firing = map[Fingerprint]struct{}{}
	resolved = map[Fingerprint]struct{}{}
	for fp, a := range g.alerts {
		if a.ResolvedAt(now) {
			resolved[fp] = struct{}{}
		} else {
			firing[fp] = struct{}{}
		}
	}
	return firing, resolved
}

// batch returns the alerts to send: resolved ones keep their endsAt, firing
// ones are scrubbed of endsAt (AM flush semantics).
func (g *aggrGroup) batch(now time.Time) []PostableAlert {
	out := make([]PostableAlert, 0, len(g.alerts))
	for _, a := range g.alerts {
		if !a.ResolvedAt(now) {
			a.EndsAt = time.Time{}
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Fingerprint() < out[j].Fingerprint() })
	return out
}

// RouteOpts are the per-route grouping and timer knobs (PLAN §4 config).
type RouteOpts struct {
	Receiver       string
	GroupBy        []string // nil/empty: single catch-all group; ["..."]: GroupByAll
	GroupWait      time.Duration
	GroupInterval  time.Duration
	RepeatInterval time.Duration // coerced to a multiple of GroupInterval at load
}

// groupByAll reports whether grouping is disabled (group by every label).
func (o RouteOpts) groupByAll() bool {
	return len(o.GroupBy) == 1 && o.GroupBy[0] == "..."
}

// groupLabels extracts the group-by labels from an alert.
func (o RouteOpts) groupLabels(labels map[string]string) map[string]string {
	if o.groupByAll() {
		return Canonicalize(labels)
	}
	out := make(map[string]string, len(o.GroupBy))
	for _, k := range o.GroupBy {
		if v, ok := labels[k]; ok {
			out[k] = v
		}
	}
	return out
}

// groupKey renders group labels canonically: {k=v,...}.
func groupKey(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
	}
	b.WriteByte('}')
	return b.String()
}
