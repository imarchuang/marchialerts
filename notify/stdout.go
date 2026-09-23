package notify

import (
	"context"
	"encoding/json"
	"log/slog"
)

// Stdout logs the payload as one JSON line — the always-available adapter.
type Stdout struct {
	Log *slog.Logger
}

// NewStdout returns a stdout contact point.
func NewStdout(log *slog.Logger) *Stdout {
	return &Stdout{Log: log}
}

// Name implements ContactPoint.
func (s *Stdout) Name() string { return "stdout" }

// Notify implements ContactPoint. Logging never fails retryably.
func (s *Stdout) Notify(_ context.Context, p Payload) (bool, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return false, err
	}
	s.Log.Info("notify", "contact_point", s.Name(), "payload", string(b))
	return false, nil
}
