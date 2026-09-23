package am

import (
	"time"
)

// Entry is the nflog record for one (receiver, groupKey): which alert
// fingerprints were firing/resolved at the last successful notification,
// and when. In-memory only (PLAN §1: delivery state is ephemeral).
type Entry struct {
	Firing    map[Fingerprint]struct{}
	Resolved  map[Fingerprint]struct{}
	Timestamp time.Time
}

func isSubset(sub, super map[Fingerprint]struct{}) bool {
	for fp := range sub {
		if _, ok := super[fp]; !ok {
			return false
		}
	}
	return true
}

// NeedsUpdate decides whether a flush should actually notify — a port of AM
// DedupStage.needsUpdate. Timers decide WHEN a group flushes; this decides
// WHETHER the flush sends.
func (e *Entry) NeedsUpdate(firing, resolved map[Fingerprint]struct{}, repeat time.Duration, now time.Time) (bool, string) {
	// Never notified: fire right away, unless there is nothing firing —
	// a resolve the receiver never knew about is not worth a message.
	if e == nil {
		return len(firing) > 0, "fire"
	}
	// New firing fingerprints appeared.
	if !isSubset(firing, e.Firing) {
		return true, "fire subset"
	}
	// Everything resolved: notify only if the receiver previously heard
	// about firing alerts, so the log can be cleared downstream.
	if len(firing) == 0 {
		return len(e.Firing) > 0, "resolve"
	}
	// New resolved fingerprints (send_resolved is always true in the MVP).
	if !isSubset(resolved, e.Resolved) {
		return true, "resolve subset"
	}
	// Nothing changed: only the repeat interval can justify a reminder.
	return !now.Before(e.Timestamp.Add(repeat)), "repeat"
}

// Log is the in-memory nflog: one entry per (receiver, groupKey).
type Log struct {
	entries map[string]*Entry
}

// NewLog returns an empty nflog.
func NewLog() *Log {
	return &Log{entries: make(map[string]*Entry)}
}

// Get returns the entry for (receiver, groupKey), or nil.
func (l *Log) Get(receiver, groupKey string) *Entry {
	if e, ok := l.entries[receiver+"\xff"+groupKey]; ok {
		cp := *e
		return &cp
	}
	return nil
}

// Set records a successful notification.
func (l *Log) Set(receiver, groupKey string, firing, resolved map[Fingerprint]struct{}, now time.Time) {
	l.entries[receiver+"\xff"+groupKey] = &Entry{Firing: firing, Resolved: resolved, Timestamp: now}
}

// Delete drops the entry (group teardown after a notified resolve).
func (l *Log) Delete(receiver, groupKey string) {
	delete(l.entries, receiver+"\xff"+groupKey)
}
