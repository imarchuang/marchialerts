package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// exampleYAML is the worked example from PLAN.md §4, verbatim.
const exampleYAML = `
listen: :9094
eval_interval: 2s
group_wait: 3s
group_interval: 6s
repeat_interval: 12s
group_by: [alertname, team]
contact_points:
  - name: logger
    stdout: {}
  - name: hook
    webhook:
      url: http://127.0.0.1:9999/alerts
rules:
  - alert: HighCPU
    expr: { metric: cpu_usage, labels: {job: api}, op: ">", threshold: 0.8 }
    for: 4s
    labels: { team: o11y, severity: warning }
`

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfigExample(t *testing.T) {
	cfg, err := LoadConfig(writeTempConfig(t, exampleYAML), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":9094" {
		t.Errorf("listen = %q, want :9094", cfg.Listen)
	}
	if time.Duration(cfg.EvalInterval) != 2*time.Second {
		t.Errorf("eval_interval = %v, want 2s", cfg.EvalInterval)
	}
	if time.Duration(cfg.GroupWait) != 3*time.Second {
		t.Errorf("group_wait = %v, want 3s", cfg.GroupWait)
	}
	if time.Duration(cfg.GroupInterval) != 6*time.Second {
		t.Errorf("group_interval = %v, want 6s", cfg.GroupInterval)
	}
	if time.Duration(cfg.RepeatInterval) != 12*time.Second {
		t.Errorf("repeat_interval = %v, want 12s", cfg.RepeatInterval)
	}
	if len(cfg.GroupBy) != 2 || cfg.GroupBy[0] != "alertname" || cfg.GroupBy[1] != "team" {
		t.Errorf("group_by = %v", cfg.GroupBy)
	}
	if len(cfg.ContactPoints) != 2 {
		t.Fatalf("contact_points = %d, want 2", len(cfg.ContactPoints))
	}
	if cfg.ContactPoints[0].Stdout == nil {
		t.Errorf("contact_points[0].stdout should be set")
	}
	if cfg.ContactPoints[1].Webhook == nil || cfg.ContactPoints[1].Webhook.URL != "http://127.0.0.1:9999/alerts" {
		t.Errorf("contact_points[1].webhook = %+v", cfg.ContactPoints[1].Webhook)
	}
	if len(cfg.Rules) != 1 {
		t.Fatalf("rules = %d, want 1", len(cfg.Rules))
	}
	r := cfg.Rules[0]
	if r.Alert != "HighCPU" {
		t.Errorf("rule.alert = %q", r.Alert)
	}
	if r.Expr.Metric != "cpu_usage" || r.Expr.Op != ">" || r.Expr.Threshold != 0.8 {
		t.Errorf("rule.expr = %+v", r.Expr)
	}
	if r.Expr.Labels["job"] != "api" {
		t.Errorf("rule.expr.labels = %v", r.Expr.Labels)
	}
	if time.Duration(r.For) != 4*time.Second {
		t.Errorf("rule.for = %v, want 4s", r.For)
	}
	if r.Labels["team"] != "o11y" || r.Labels["severity"] != "warning" {
		t.Errorf("rule.labels = %v", r.Labels)
	}
}

// Grafana hypothesis H5: repeat_interval is coerced to a multiple of
// group_interval, because repeat is only checked at flush cadence.
func TestRepeatIntervalCoercedToMultiple(t *testing.T) {
	cfg, err := LoadConfig(writeTempConfig(t, "group_interval: 3s\nrepeat_interval: 5s\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := time.Duration(cfg.RepeatInterval); got != 6*time.Second {
		t.Fatalf("repeat_interval = %v, want 6s", got)
	}
}

func TestRepeatIntervalZeroBecomesOneGroupInterval(t *testing.T) {
	cfg, err := LoadConfig(writeTempConfig(t, "group_interval: 3s\nrepeat_interval: 0s\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := time.Duration(cfg.RepeatInterval); got != 3*time.Second {
		t.Fatalf("repeat_interval = %v, want 3s (one group_interval)", got)
	}
}

func TestDefaultsWithoutFile(t *testing.T) {
	cfg, err := LoadConfig("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":9094" || time.Duration(cfg.EvalInterval) != 2*time.Second {
		t.Errorf("defaults = %+v", cfg)
	}
}

func TestMissingKeysKeepDefaults(t *testing.T) {
	cfg, err := LoadConfig(writeTempConfig(t, "listen: :9999\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":9999" {
		t.Errorf("listen = %q, want :9999", cfg.Listen)
	}
	if time.Duration(cfg.GroupInterval) != 6*time.Second {
		t.Errorf("group_interval = %v, want default 6s", cfg.GroupInterval)
	}
}

func TestInvalidDuration(t *testing.T) {
	if _, err := LoadConfig(writeTempConfig(t, "eval_interval: nope\n"), nil); err == nil {
		t.Fatal("expected error for invalid duration")
	}
}

func TestMissingFile(t *testing.T) {
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "absent.yaml"), nil); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestEngineRulesConversion(t *testing.T) {
	cfg, err := LoadConfig(writeTempConfig(t, exampleYAML), nil)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := engineRules(cfg.Rules)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 {
		t.Fatalf("rules = %d, want 1", len(rules))
	}
	r := rules[0]
	if r.Alert != "HighCPU" || r.Expr.Metric != "cpu_usage" || r.Expr.Op != ">" {
		t.Errorf("engine rule = %+v", r)
	}
	if r.For != 4*time.Second {
		t.Errorf("for = %v, want 4s", r.For)
	}
}

func TestEngineRulesRejectsBadOp(t *testing.T) {
	cfg, err := LoadConfig(writeTempConfig(t, `
rules:
  - alert: Bad
    expr: { metric: m, op: "~", threshold: 1 }
`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engineRules(cfg.Rules); err == nil {
		t.Fatal("expected validation error for bad op")
	}
}
