package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Webhook POSTs the Grafana-ish JSON payload to a URL. 5xx and transport
// errors are retryable; 4xx is permanent (the request itself is wrong).
type Webhook struct {
	URL    string
	Client *http.Client
	name   string
}

// NewWebhook returns a webhook contact point with a bounded HTTP client.
func NewWebhook(name, url string) *Webhook {
	return &Webhook{
		URL:  url,
		name: name,
		Client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// Name implements ContactPoint.
func (w *Webhook) Name() string { return w.name }

// Notify implements ContactPoint.
func (w *Webhook) Notify(ctx context.Context, p Payload) (bool, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return false, fmt.Errorf("marshal payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(b))
	if err != nil {
		return false, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.Client.Do(req)
	if err != nil {
		return true, fmt.Errorf("post: %w", err) // transport error: retryable
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return false, nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return false, fmt.Errorf("webhook returned %s (permanent)", resp.Status)
	default:
		return true, fmt.Errorf("webhook returned %s", resp.Status) // 5xx: retryable
	}
}
