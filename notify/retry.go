package notify

import (
	"context"
	"time"
)

// Retry wraps a ContactPoint with bounded exponential backoff (a thinned AM
// RetryStage): retryable errors are retried until maxAttempts, permanent
// errors give up immediately. Sleep is injectable so tests never wait.
type Retry struct {
	Inner       ContactPoint
	MaxAttempts int           // total attempts including the first; <= 0 means 3
	BaseBackoff time.Duration // first backoff; doubles each attempt; <= 0 means 100ms
	Sleep       func(ctx context.Context, d time.Duration) error
}

// NewRetry wraps inner with defaults (3 attempts, 100ms base backoff).
func NewRetry(inner ContactPoint) *Retry {
	return &Retry{Inner: inner, MaxAttempts: 3, BaseBackoff: 100 * time.Millisecond}
}

// Name implements ContactPoint.
func (r *Retry) Name() string { return r.Inner.Name() }

func (r *Retry) sleep(ctx context.Context, d time.Duration) error {
	if r.Sleep != nil {
		return r.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Notify implements ContactPoint.
func (r *Retry) Notify(ctx context.Context, p Payload) (bool, error) {
	maxAttempts := r.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	backoff := r.BaseBackoff
	if backoff <= 0 {
		backoff = 100 * time.Millisecond
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		retryable, err := r.Inner.Notify(ctx, p)
		if err == nil {
			return false, nil
		}
		lastErr = err
		if !retryable {
			return false, err // permanent: no point retrying
		}
		if attempt == maxAttempts {
			break
		}
		if serr := r.sleep(ctx, backoff); serr != nil {
			return false, serr
		}
		backoff *= 2
	}
	return true, lastErr
}
