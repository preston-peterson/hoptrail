package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTempConfig writes content to a temp file and returns its path.
func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing temp config: %v", err)
	}
	return path
}

func TestDefault_IsValid(t *testing.T) {
	// The documented defaults must themselves pass validation — otherwise
	// a sparse config file (or none) could never start.
	if err := Default().Validate(); err != nil {
		t.Fatalf("Default() config failed validation: %v", err)
	}
}

func TestLoad_SparseFileKeepsDefaults(t *testing.T) {
	// A file that sets only one field must keep defaults for everything
	// else.
	path := writeTempConfig(t, `
listen: ":7777"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Listen != ":7777" {
		t.Errorf("Listen = %q, want \":7777\" (from file)", cfg.Listen)
	}
	if cfg.Probe.MaxHops != 30 {
		t.Errorf("Probe.MaxHops = %d, want 30 (default kept)", cfg.Probe.MaxHops)
	}
	if cfg.Probe.Interval.Std() != 1*time.Second {
		t.Errorf("Probe.Interval = %s, want 1s (default kept)", cfg.Probe.Interval.Std())
	}
}

func TestLoad_DurationParsing(t *testing.T) {
	path := writeTempConfig(t, `
probe:
  interval: "500ms"
  discovery_interval: "1m"
  timeout: "250ms"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Probe.Interval.Std() != 500*time.Millisecond {
		t.Errorf("Interval = %s, want 500ms", cfg.Probe.Interval.Std())
	}
	if cfg.Probe.DiscoveryInterval.Std() != 1*time.Minute {
		t.Errorf("DiscoveryInterval = %s, want 1m", cfg.Probe.DiscoveryInterval.Std())
	}
	if cfg.Probe.Timeout.Std() != 250*time.Millisecond {
		t.Errorf("Timeout = %s, want 250ms", cfg.Probe.Timeout.Std())
	}
}

func TestLoad_BadDurationIsRejected(t *testing.T) {
	path := writeTempConfig(t, `
probe:
  interval: "not-a-duration"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() accepted an invalid duration string; want error")
	}
	if !strings.Contains(err.Error(), "duration") {
		t.Errorf("error %q does not mention the duration problem", err)
	}
}

func TestLoad_MissingFileIsError(t *testing.T) {
	_, err := Load("/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("Load() on a missing file returned nil error")
	}
}

func TestLoad_EnvOverrides(t *testing.T) {
	path := writeTempConfig(t, `
listen: ":8080"
`)
	t.Setenv("HOPTRAIL_LISTEN", "127.0.0.1:9000")
	t.Setenv("HOPTRAIL_LOG_LEVEL", "debug")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Listen != "127.0.0.1:9000" {
		t.Errorf("Listen = %q, want \"127.0.0.1:9000\" (env override)", cfg.Listen)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("Log.Level = %q, want \"debug\" (env override)", cfg.Log.Level)
	}
}

func TestValidate_AggregatesAllProblems(t *testing.T) {
	// A config with several independent problems must report all of them
	// at once, not just the first. Step-32 removed probe.target from
	// the validator (it moved to the active_targets DB table), so the
	// fragments here cover what remains in yaml-validated scope.
	cfg := Default()
	cfg.Listen = "not a valid address"
	cfg.Storage.RetentionDays = 0
	cfg.Probe.MaxHops = 999
	cfg.Log.Level = "verbose"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() returned nil for a config with four problems")
	}
	if !errors.Is(err, ErrConfigInvalid) {
		t.Errorf("error does not wrap ErrConfigInvalid: %v", err)
	}

	msg := err.Error()
	for _, fragment := range []string{"listen", "retention_days", "max_hops", "log.level"} {
		if !strings.Contains(msg, fragment) {
			t.Errorf("aggregated error is missing the %q problem:\n%s", fragment, msg)
		}
	}
}

// TestLoad_IgnoresLegacyYamlTarget — yaml.v3 silently drops keys not
// in the struct, so an existing config.yaml with a legacy
// `probe.target` field still loads fine. The field's value is
// ignored; the daemon hydrates active targets from the DB instead.
// This keeps step-32 a no-op for operators with in-flight deploys.
func TestLoad_IgnoresLegacyYamlTarget(t *testing.T) {
	path := writeTempConfig(t, `
probe:
  target: "8.8.8.8"
  interval: "1s"
`)
	if _, err := Load(path); err != nil {
		t.Errorf("Load() rejected a config with legacy probe.target: %v", err)
	}
}

func TestValidate_AcceptsValidNonDefaultConfig(t *testing.T) {
	cfg := Default()
	cfg.Listen = "127.0.0.1:8080"
	cfg.Probe.Interval = Duration(2 * time.Second)
	cfg.Probe.Timeout = Duration(5 * time.Second) // longer than interval is fine
	cfg.Probe.MaxHops = 64                        // the ceiling, allowed
	cfg.Probe.RouteChangeThreshold = 1            // the floor, allowed
	cfg.Log.Format = "json"

	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() rejected a valid non-default config: %v", err)
	}
}

func TestValidate_RejectsMaxHopsBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		maxHops int
		wantErr bool
	}{
		{"zero", 0, true},
		{"negative", -1, true},
		{"one", 1, false},
		{"ceiling", maxHopsCeiling, false},
		{"above ceiling", maxHopsCeiling + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.Probe.MaxHops = tc.maxHops
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("MaxHops=%d: want error, got nil", tc.maxHops)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("MaxHops=%d: want no error, got %v", tc.maxHops, err)
			}
		})
	}
}

func TestValidate_AgentTokens(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tokens  []string
		wantErr bool
	}{
		{"empty list (default, ingest disabled)", nil, false},
		{"proper token", []string{"a-long-random-secret-of-32-chars"}, false},
		{"two tokens", []string{"a-long-random-secret-of-32-chars", "another-long-random-secret-here"}, false},
		{"empty string token", []string{""}, true},
		{"whitespace token", []string{"                    "}, true},
		{"too short", []string{"changeme"}, true},
		{"one good one bad", []string{"a-long-random-secret-of-32-chars", "weak"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.Agents.Tokens = tc.tokens
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("tokens=%q: want error, got nil", tc.tokens)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("tokens=%q: want no error, got %v", tc.tokens, err)
			}
		})
	}
}

func TestDurationRoundTrip(t *testing.T) {
	// Marshal then unmarshal must preserve the value, so a config the
	// daemon rewrites stays stable.
	original := Duration(90 * time.Second)
	out, err := original.MarshalYAML()
	if err != nil {
		t.Fatalf("MarshalYAML: %v", err)
	}
	s, ok := out.(string)
	if !ok {
		t.Fatalf("MarshalYAML returned %T, want string", out)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		t.Fatalf("re-parsing marshaled duration %q: %v", s, err)
	}
	if Duration(parsed) != original {
		t.Errorf("round trip: got %s, want %s", time.Duration(parsed), time.Duration(original))
	}
}
