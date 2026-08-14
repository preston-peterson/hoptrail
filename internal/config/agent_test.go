package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// validAgent returns a config that passes validation; tests mutate
// one field at a time from here.
func validAgent() AgentConfig {
	cfg := DefaultAgent()
	cfg.ProbeID = "site-east-pi"
	cfg.Central.URL = "http://central.local:8080"
	cfg.Central.Token = "a-long-random-secret-of-32-chars"
	return cfg
}

func TestAgentValidate_ValidConfigPasses(t *testing.T) {
	if err := validAgent().Validate(); err != nil {
		t.Errorf("valid agent config rejected: %v", err)
	}
}

func TestAgentValidate_DefaultsAloneAreInvalid(t *testing.T) {
	// DefaultAgent has no probe_id, url, or token — all three must be
	// reported in one pass.
	err := DefaultAgent().Validate()
	if err == nil {
		t.Fatal("bare defaults validated, want errors for probe_id + url + token")
	}
	for _, want := range []string{"probe_id", "central.url", "central.token"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing a %s problem", err, want)
		}
	}
}

func TestAgentValidate_FieldMatrix(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*AgentConfig)
	}{
		{"reserved probe_id local", func(c *AgentConfig) { c.ProbeID = "local" }},
		{"reserved probe_id all", func(c *AgentConfig) { c.ProbeID = "all" }},
		{"uppercase probe_id", func(c *AgentConfig) { c.ProbeID = "Site-East" }},
		{"one-char probe_id", func(c *AgentConfig) { c.ProbeID = "x" }},
		{"ftp url", func(c *AgentConfig) { c.Central.URL = "ftp://central:8080" }},
		{"hostless url", func(c *AgentConfig) { c.Central.URL = "http://" }},
		{"short token", func(c *AgentConfig) { c.Central.Token = "short" }},
		{"zero heartbeat", func(c *AgentConfig) { c.Central.HeartbeatInterval = 0 }},
		{"zero ingest interval", func(c *AgentConfig) { c.Central.IngestInterval = 0 }},
		{"zero probe interval", func(c *AgentConfig) { c.Probe.Interval = 0 }},
		{"max_hops over ceiling", func(c *AgentConfig) { c.Probe.MaxHops = 65 }},
		{"empty buffer path", func(c *AgentConfig) { c.Buffer.Path = " " }},
		{"zero buffer size", func(c *AgentConfig) { c.Buffer.MaxSizeMB = 0 }},
		{"bad log level", func(c *AgentConfig) { c.Log.Level = "loud" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validAgent()
			tc.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Errorf("mutation %q validated, want error", tc.name)
			}
		})
	}
}

func TestLoadAgent_OverlaysOnDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(path, []byte(`
probe_id: "gateway-rack"
central:
  url: "https://hoptrail.tail.example:8080"
  token: "a-long-random-secret-of-32-chars"
  heartbeat_interval: "30s"
probe:
  interval: "2s"
`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := LoadAgent(path)
	if err != nil {
		t.Fatalf("LoadAgent: %v", err)
	}
	if cfg.ProbeID != "gateway-rack" {
		t.Errorf("ProbeID = %q", cfg.ProbeID)
	}
	if cfg.Central.HeartbeatInterval.Std() != 30*time.Second {
		t.Errorf("HeartbeatInterval = %s, want 30s (file value)", cfg.Central.HeartbeatInterval.Std())
	}
	if cfg.Central.IngestInterval.Std() != 5*time.Second {
		t.Errorf("IngestInterval = %s, want 5s (default kept)", cfg.Central.IngestInterval.Std())
	}
	if cfg.Probe.Interval.Std() != 2*time.Second {
		t.Errorf("Probe.Interval = %s, want 2s (file value)", cfg.Probe.Interval.Std())
	}
	if cfg.Buffer.MaxSizeMB != 50 {
		t.Errorf("Buffer.MaxSizeMB = %d, want 50 (default kept)", cfg.Buffer.MaxSizeMB)
	}
}

func TestLoadAgent_InvalidConfigRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(path, []byte(`probe_id: "BAD ID"`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadAgent(path); err == nil {
		t.Fatal("invalid agent config loaded without error")
	}
}
