// Package config defines hoptrail's configuration schema, loads it from a
// YAML file, applies environment-variable overrides, and validates the
// result. The daemon refuses to start on an invalid config; `hoptrail
// check-config` exists to validate without starting.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration for a hoptrail daemon.
//
// v0.1 is single-target: exactly one probe target per running daemon.
// The schema is intentionally flat and small.
type Config struct {
	// Listen is the address the HTTP server binds to, e.g. ":8080" or
	// "127.0.0.1:8080". Passed directly to net/http.
	Listen string `yaml:"listen"`

	Storage StorageConfig `yaml:"storage"`
	Probe   ProbeConfig   `yaml:"probe"`
	RDNS    RDNSConfig    `yaml:"rdns"`
	Log     LogConfig     `yaml:"log"`
	Agents  AgentsConfig  `yaml:"probes"`
}

// StorageConfig controls where probe data is persisted and for how long.
type StorageConfig struct {
	// Path is the SQLite database file. Its parent directory must exist
	// and be writable by the daemon's user.
	Path string `yaml:"path"`

	// RetentionDays is how many days of raw samples to keep. The hourly
	// retention job deletes anything older. Must be >= 1.
	RetentionDays int `yaml:"retention_days"`
}

// ProbeConfig controls the probe engine: what to probe, how often, how far.
type ProbeConfig struct {
	// Interval is the per-hop ping cadence — how often each already-known
	// hop is pinged. Default 1s.
	Interval Duration `yaml:"interval"`

	// DiscoveryInterval is the path-discovery sweep cadence — how often a
	// full TTL sweep runs to find new hops and feed route-change
	// detection. Default 30s.
	DiscoveryInterval Duration `yaml:"discovery_interval"`

	// MaxHops is the largest TTL probed. Default 30. Hard ceiling 64
	// (the practical Linux TTL limit for this purpose).
	MaxHops int `yaml:"max_hops"`

	// Timeout is how long a single probe waits for a response before
	// being recorded as a timeout. Default 2s.
	Timeout Duration `yaml:"timeout"`

	// RouteChangeThreshold is how many consecutive observations of a new
	// IP-at-TTL are required before a route change is flagged. This is
	// the ECMP-bucketing knob: higher values are more conservative.
	// Default 5. Must be >= 1.
	RouteChangeThreshold int `yaml:"route_change_threshold"`
}

// RDNSConfig controls the reverse-DNS worker that populates hostnames
// for IPs seen in probe samples. The worker is a separate goroutine
// from the probe loop; disabling it means the path response shows
// raw IPs but probing is otherwise unaffected. Operators may want to
// disable it for privacy reasons (don't leak hops to public DNS) or
// when running against a known-target set where the names are obvious.
type RDNSConfig struct {
	// Enabled toggles the rdns worker on or off. Default true.
	Enabled bool `yaml:"enabled"`
}

// AgentsConfig controls the v0.3 probe-ingest surface on a central
// daemon — the yaml block is `probes:` (the user-facing word for a
// remote measurement point; "agent" survives only as the internal
// code term for the remote-probe process, see cmd/hoptrail/main.go's
// terminology note). Remote `hoptrail probe` processes authenticate
// with a bearer token from this list; an empty list (the default)
// disables ingest entirely, which is the correct shape for a
// single-host deploy. Tokens are opaque shared secrets — long random
// strings the operator copies into each remote probe's config.
type AgentsConfig struct {
	// Tokens is the list of accepted probe bearer tokens.
	Tokens []string `yaml:"tokens"`
}

// LogConfig controls structured logging output.
type LogConfig struct {
	// Level is one of: debug, info, warn, error.
	Level string `yaml:"level"`

	// Format is one of: text, json.
	Format string `yaml:"format"`
}

// Duration is a time.Duration that unmarshals from a human-readable YAML
// string like "1s" or "30s" rather than an integer nanosecond count.
type Duration time.Duration

// UnmarshalYAML decodes a duration string (e.g. "1s", "500ms", "2m").
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string like \"1s\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalYAML renders the duration back to a string form.
func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

// Std returns the underlying time.Duration.
func (d Duration) Std() time.Duration {
	return time.Duration(d)
}

// Default returns a Config populated with the documented defaults. Loading
// a file overlays parsed values on top of these, so a sparse config file
// only needs to specify what differs from the defaults.
func Default() Config {
	return Config{
		Listen: ":8080",
		Storage: StorageConfig{
			Path:          "/var/lib/hoptrail/hoptrail.db",
			RetentionDays: 7,
		},
		Probe: ProbeConfig{
			Interval:             Duration(1 * time.Second),
			DiscoveryInterval:    Duration(30 * time.Second),
			MaxHops:              30,
			Timeout:              Duration(2 * time.Second),
			RouteChangeThreshold: 5,
		},
		RDNS: RDNSConfig{
			Enabled: true,
		},
		Log: LogConfig{
			Level:  "info",
			Format: "text",
		},
	}
}

// Load reads a YAML config file from path, overlays it on the defaults,
// applies environment-variable overrides, and validates the result. A nil
// error means the returned Config is safe to run with.
func Load(path string) (Config, error) {
	cfg := Default()

	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("reading config file: %w", err)
	}

	// yaml.Unmarshal overlays onto cfg: keys present in the file replace
	// the default; keys absent from the file keep the default value.
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("parsing config file %s: %w", path, err)
	}

	cfg.applyEnvOverrides()

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// applyEnvOverrides lets a handful of high-value fields be overridden by
// environment variables. This is primarily for container deployments,
// where baking a config file is more friction than setting an env var.
// Only documented fields are supported; unknown HOPTRAIL_* vars are
// ignored.
func (c *Config) applyEnvOverrides() {
	if v := os.Getenv("HOPTRAIL_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("HOPTRAIL_STORAGE_PATH"); v != "" {
		c.Storage.Path = v
	}
	if v := os.Getenv("HOPTRAIL_LOG_LEVEL"); v != "" {
		c.Log.Level = v
	}
}
