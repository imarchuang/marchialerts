package main

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from YAML strings like "2s".
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string like \"2s\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) String() string { return time.Duration(d).String() }

// Expr is a threshold condition on one metric + label selector (used from PR1).
type Expr struct {
	Metric    string            `yaml:"metric"`
	Labels    map[string]string `yaml:"labels"`
	Op        string            `yaml:"op"`
	Threshold float64           `yaml:"threshold"`
}

// Rule is one alert rule spec (evaluated from PR1 on).
type Rule struct {
	Alert  string            `yaml:"alert"`
	Expr   Expr              `yaml:"expr"`
	For    Duration          `yaml:"for"`
	Labels map[string]string `yaml:"labels"`
}

// ContactPoint is one notification adapter (wired in PR5).
type ContactPoint struct {
	Name    string `yaml:"name"`
	Stdout  *struct {
	} `yaml:"stdout"`
	Webhook *struct {
		URL string `yaml:"url"`
	} `yaml:"webhook"`
}

// Config is the root of the YAML config file.
type Config struct {
	Listen         string         `yaml:"listen"`
	EvalInterval   Duration       `yaml:"eval_interval"`
	GroupWait      Duration       `yaml:"group_wait"`
	GroupInterval  Duration       `yaml:"group_interval"`
	RepeatInterval Duration       `yaml:"repeat_interval"`
	GroupBy        []string       `yaml:"group_by"`
	ContactPoints  []ContactPoint `yaml:"contact_points"`
	Rules          []Rule         `yaml:"rules"`
}

// DefaultConfig matches the worked example in PLAN.md §4.
func DefaultConfig() Config {
	return Config{
		Listen:         ":9094",
		EvalInterval:   Duration(2 * time.Second),
		GroupWait:      Duration(3 * time.Second),
		GroupInterval:  Duration(6 * time.Second),
		RepeatInterval: Duration(12 * time.Second),
		GroupBy:        []string{"alertname", "team"},
	}
}

// LoadConfig reads path over the defaults (missing keys keep their default),
// validates, and coerces repeat_interval up to a multiple of group_interval —
// repeat is only ever checked at flush cadence, like Grafana.
func LoadConfig(path string, log *slog.Logger) (Config, error) {
	cfg := DefaultConfig()
	if path == "" {
		return cfg, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}

	if time.Duration(cfg.EvalInterval) <= 0 {
		return cfg, fmt.Errorf("eval_interval must be > 0, got %s", cfg.EvalInterval)
	}
	if time.Duration(cfg.GroupInterval) <= 0 {
		return cfg, fmt.Errorf("group_interval must be > 0, got %s", cfg.GroupInterval)
	}
	if time.Duration(cfg.GroupWait) < 0 {
		return cfg, fmt.Errorf("group_wait must be >= 0, got %s", cfg.GroupWait)
	}

	ri, gi := time.Duration(cfg.RepeatInterval), time.Duration(cfg.GroupInterval)
	if ri < gi || ri%gi != 0 {
		coerced := ((ri + gi - 1) / gi) * gi
		if coerced < gi {
			coerced = gi
		}
		if log != nil {
			log.Warn("repeat_interval coerced to a multiple of group_interval",
				"repeat_interval", ri, "group_interval", gi, "coerced", coerced)
		}
		cfg.RepeatInterval = Duration(coerced)
	}
	return cfg, nil
}
