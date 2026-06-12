// Agent-role configuration (docs/v0.3-protocol-design.md §7). The
// agent's yaml is deliberately small: identity, where central is,
// probe cadence, and the spill buffer. Targets are NOT configured
// here — central owns the target set and propagates it via heartbeat.

package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// AgentConfig is the top-level configuration for `hoptrail probe` —
// the struct keeps the internal "agent" name (a config.ProbeConfig
// already exists for the probing-cadence block); operators only ever
// see "probe".
type AgentConfig struct {
	// ProbeID is this agent's stable identity — kebab-case, 2-32
	// chars (e.g. "site-east-pi"). Central keys everything by it;
	// changing it orphans the agent's historical data under the old
	// name.
	ProbeID string `yaml:"probe_id"`

	Central CentralConfig `yaml:"central"`
	Probe   ProbeConfig   `yaml:"probe"`
	Buffer  BufferConfig  `yaml:"buffer"`
	Log     LogConfig     `yaml:"log"`
}

// CentralConfig says where the central daemon is and how to talk to it.
type CentralConfig struct {
	// URL is the central's base URL, e.g. "http://central.local:8080"
	// or a Tailscale/WireGuard address. http and https both accepted
	// (§12.5 — the operator's network topology picks).
	URL string `yaml:"url"`

	// Token is the bearer token from `hoptrail token gen`, also
	// present in the central's probes.tokens list.
	Token string `yaml:"token"`

	// HeartbeatInterval is the steady-state registration cadence.
	// Default 60s.
	HeartbeatInterval Duration `yaml:"heartbeat_interval"`

	// IngestInterval is how often buffered samples are POSTed.
	// Default 5s.
	IngestInterval Duration `yaml:"ingest_interval"`
}

// BufferConfig controls the partition-recovery spill buffer (§8).
type BufferConfig struct {
	// Path is the buffer's SQLite file. Parent directory is created
	// if absent.
	Path string `yaml:"path"`

	// MaxSizeMB bounds total buffered payload bytes; oldest batches
	// are evicted when full. Default 50 (≈14h at default cadence).
	MaxSizeMB int `yaml:"max_size_mb"`
}

// probeIDPattern mirrors the server-side ingest validation
// (internal/server/ingest.go) — keep the two in sync. Validating at
// config load means a bad probe_id fails at startup with a clear
// message instead of as a heartbeat 400 crash-loop.
var probeIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,31}$`)

// reservedProbeIDs can't be claimed by agents: 'local' is the central
// daemon's own engine, 'all' the merged-view virtual probe.
var reservedProbeIDs = []string{"local", "all"}

// DefaultAgent returns the documented agent defaults. ProbeID and
// central URL/token have no sensible defaults and must be set.
func DefaultAgent() AgentConfig {
	return AgentConfig{
		Central: CentralConfig{
			HeartbeatInterval: Duration(60 * time.Second),
			IngestInterval:    Duration(5 * time.Second),
		},
		Probe: ProbeConfig{
			Interval:             Duration(1 * time.Second),
			DiscoveryInterval:    Duration(30 * time.Second),
			MaxHops:              30,
			Timeout:              Duration(2 * time.Second),
			RouteChangeThreshold: 5,
		},
		Buffer: BufferConfig{
			Path:      "/var/lib/hoptrail/probe-buffer.db",
			MaxSizeMB: 50,
		},
		Log: LogConfig{
			Level:  "info",
			Format: "text",
		},
	}
}

// LoadAgent reads an agent yaml, overlays it on the defaults, and
// validates. Same overlay semantics as the central's Load.
func LoadAgent(path string) (AgentConfig, error) {
	cfg := DefaultAgent()
	raw, err := os.ReadFile(path)
	if err != nil {
		return AgentConfig{}, fmt.Errorf("reading probe config file: %w", err)
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return AgentConfig{}, fmt.Errorf("parsing probe config file %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return AgentConfig{}, err
	}
	return cfg, nil
}

// Validate aggregates all problems into one error, mirroring the
// central config's behavior.
func (c AgentConfig) Validate() error {
	var problems []string

	// --- probe_id ---
	if c.ProbeID == "" {
		problems = append(problems, "probe_id: must be set (e.g. \"site-east-pi\")")
	} else if !probeIDPattern.MatchString(c.ProbeID) {
		problems = append(problems,
			fmt.Sprintf("probe_id: %q must match %s (kebab-case, 2-32 chars)", c.ProbeID, probeIDPattern.String()))
	} else {
		for _, reserved := range reservedProbeIDs {
			if c.ProbeID == reserved {
				problems = append(problems,
					fmt.Sprintf("probe_id: %q is reserved", c.ProbeID))
			}
		}
	}

	// --- central ---
	if c.Central.URL == "" {
		problems = append(problems, "central.url: must be set (e.g. \"http://central.local:8080\")")
	} else if u, err := url.Parse(c.Central.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		problems = append(problems,
			fmt.Sprintf("central.url: %q must be an http:// or https:// URL with a host", c.Central.URL))
	}
	if strings.TrimSpace(c.Central.Token) == "" {
		problems = append(problems, "central.token: must be set (generate with `hoptrail token gen` on central)")
	} else if len(c.Central.Token) < minAgentTokenLen {
		problems = append(problems,
			fmt.Sprintf("central.token: must be at least %d characters", minAgentTokenLen))
	}
	if c.Central.HeartbeatInterval.Std() <= 0 {
		problems = append(problems,
			fmt.Sprintf("central.heartbeat_interval: must be positive, got %s", c.Central.HeartbeatInterval.Std()))
	}
	if c.Central.IngestInterval.Std() <= 0 {
		problems = append(problems,
			fmt.Sprintf("central.ingest_interval: must be positive, got %s", c.Central.IngestInterval.Std()))
	}

	// --- probe --- (same rules as the central's probe block)
	if c.Probe.Interval.Std() <= 0 {
		problems = append(problems,
			fmt.Sprintf("probe.interval: must be positive, got %s", c.Probe.Interval.Std()))
	}
	if c.Probe.DiscoveryInterval.Std() <= 0 {
		problems = append(problems,
			fmt.Sprintf("probe.discovery_interval: must be positive, got %s", c.Probe.DiscoveryInterval.Std()))
	}
	if c.Probe.Timeout.Std() <= 0 {
		problems = append(problems,
			fmt.Sprintf("probe.timeout: must be positive, got %s", c.Probe.Timeout.Std()))
	}
	if c.Probe.MaxHops < 1 || c.Probe.MaxHops > maxHopsCeiling {
		problems = append(problems,
			fmt.Sprintf("probe.max_hops: must be 1-%d, got %d", maxHopsCeiling, c.Probe.MaxHops))
	}
	if c.Probe.RouteChangeThreshold < 1 {
		problems = append(problems,
			fmt.Sprintf("probe.route_change_threshold: must be >= 1, got %d", c.Probe.RouteChangeThreshold))
	}

	// --- buffer ---
	if strings.TrimSpace(c.Buffer.Path) == "" {
		problems = append(problems, "buffer.path: must not be empty")
	}
	if c.Buffer.MaxSizeMB < 1 {
		problems = append(problems,
			fmt.Sprintf("buffer.max_size_mb: must be >= 1, got %d", c.Buffer.MaxSizeMB))
	}

	// --- log ---
	if !contains(validLogLevels, c.Log.Level) {
		problems = append(problems,
			fmt.Sprintf("log.level: %q is not one of %s", c.Log.Level, strings.Join(validLogLevels, ", ")))
	}
	if !contains(validLogFormats, c.Log.Format) {
		problems = append(problems,
			fmt.Sprintf("log.format: %q is not one of %s", c.Log.Format, strings.Join(validLogFormats, ", ")))
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%w:\n  - %s", ErrConfigInvalid, strings.Join(problems, "\n  - "))
}
