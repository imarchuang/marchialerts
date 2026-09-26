package am

import (
	"crypto/rand"
	"fmt"
	"regexp"
	"sync"
	"time"
)

// Matcher is one label matcher in a silence (AM matcher subset: =, !=, =~, !~).
type Matcher struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	IsRegex bool   `json:"isRegex"`
	IsEqual bool   `json:"isEqual"` // false = != / !~

	re *regexp.Regexp
}

// Compile validates and precompiles the matcher.
func (m *Matcher) Compile() error {
	if m.Name == "" {
		return fmt.Errorf("matcher: name is required")
	}
	if m.IsRegex {
		re, err := regexp.Compile("^(?:" + m.Value + ")$")
		if err != nil {
			return fmt.Errorf("matcher %q: bad regex: %w", m.Name, err)
		}
		m.re = re
	}
	return nil
}

// Matches reports whether the matcher hits the label set. A missing label
// matches != and !~ (Prometheus semantics: the empty value).
func (m *Matcher) Matches(labels map[string]string) bool {
	v := labels[m.Name]
	var ok bool
	if m.IsRegex {
		ok = m.re.MatchString(v)
	} else {
		ok = v == m.Value
	}
	if !m.IsEqual {
		return !ok
	}
	return ok
}

// Silence mutes notifications for matching alerts within [StartsAt, EndsAt).
// It gates NOTIFY ONLY — evaluation and grouping keep running (H6).
type Silence struct {
	ID        string    `json:"id"`
	Matchers  []Matcher `json:"matchers"`
	StartsAt  time.Time `json:"startsAt"`
	EndsAt    time.Time `json:"endsAt"`
	CreatedBy string    `json:"createdBy"`
	Comment   string    `json:"comment"`
}

// State is the silence lifecycle: pending → active → expired.
func (s *Silence) State(now time.Time) string {
	switch {
	case now.Before(s.StartsAt):
		return "pending"
	case now.Before(s.EndsAt):
		return "active"
	default:
		return "expired"
	}
}

// matches reports whether all matchers hit the label set.
func (s *Silence) matches(labels map[string]string) bool {
	for _, m := range s.Matchers {
		if !m.Matches(labels) {
			return false
		}
	}
	return true
}

// SilenceStore keeps silences in memory (delivery state is ephemeral, PLAN §1).
type SilenceStore struct {
	mu       sync.RWMutex
	silences map[string]*Silence
}

// NewSilenceStore returns an empty store.
func NewSilenceStore() *SilenceStore {
	return &SilenceStore{silences: make(map[string]*Silence)}
}

// newID mints a random hex id (UUID-shaped enough for the MVP).
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Create validates and stores a silence, returning its id.
func (ss *SilenceStore) Create(s Silence) (string, error) {
	if len(s.Matchers) == 0 {
		return "", fmt.Errorf("at least one matcher is required")
	}
	for i := range s.Matchers {
		if err := s.Matchers[i].Compile(); err != nil {
			return "", err
		}
	}
	if s.EndsAt.Before(s.StartsAt) {
		return "", fmt.Errorf("endsAt must be after startsAt")
	}
	if s.ID == "" {
		s.ID = newID()
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	cp := s
	ss.silences[s.ID] = &cp
	return s.ID, nil
}

// Get returns one silence (nil if unknown).
func (ss *SilenceStore) Get(id string) *Silence {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	if s, ok := ss.silences[id]; ok {
		cp := *s
		return &cp
	}
	return nil
}

// List returns all silences, optionally filtered by state
// ("pending"/"active"/"expired"; empty = all).
func (ss *SilenceStore) List(state string, now time.Time) []*Silence {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	var out []*Silence
	for _, s := range ss.silences {
		if state != "" && s.State(now) != state {
			continue
		}
		cp := *s
		out = append(out, &cp)
	}
	return out
}

// Delete expires a silence (AM semantics: delete = set EndsAt to now).
func (ss *SilenceStore) Delete(id string, now time.Time) bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	s, ok := ss.silences[id]
	if !ok {
		return false
	}
	s.EndsAt = now
	return true
}

// Mutes reports whether any ACTIVE silence matches the label set, and the
// matching silence IDs (for the marker / status API).
func (ss *SilenceStore) Mutes(labels map[string]string, now time.Time) (bool, []string) {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	var ids []string
	for _, s := range ss.silences {
		if s.State(now) == "active" && s.matches(labels) {
			ids = append(ids, s.ID)
		}
	}
	return len(ids) > 0, ids
}
