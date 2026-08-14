package config

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// maxHopsCeiling is the hard upper bound on Probe.MaxHops. Beyond this,
// a traceroute is almost certainly chasing a routing loop rather than a
// real path.
const maxHopsCeiling = 64

// minAgentTokenLen is the floor on agent bearer-token length. Proper
// tokens are 32 random bytes base64-encoded (≈44 chars); the floor
// only exists to reject obviously weak or truncated values.
const minAgentTokenLen = 16

// validLogLevels and validLogFormats are the accepted values for the
// corresponding LogConfig fields.
var (
	validLogLevels  = []string{"debug", "info", "warn", "error"}
	validLogFormats = []string{"text", "json"}
)

// Validate checks every field of the Config and returns a single error
// describing all problems found, or nil if the config is safe to run.
//
// Validation aggregates: a config with three mistakes produces one error
// listing all three, so the operator fixes everything in one pass instead
// of discovering problems one restart at a time.
func (c Config) Validate() error {
	var problems []string

	// --- Listen ---
	if c.Listen == "" {
		problems = append(problems, "listen: must not be empty (e.g. \":8080\")")
	} else if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		problems = append(problems,
			fmt.Sprintf("listen: %q is not a valid host:port address: %v", c.Listen, err))
	}

	// --- Storage ---
	if strings.TrimSpace(c.Storage.Path) == "" {
		problems = append(problems, "storage.path: must not be empty")
	}
	if c.Storage.RetentionDays < 1 {
		problems = append(problems,
			fmt.Sprintf("storage.retention_days: must be >= 1, got %d", c.Storage.RetentionDays))
	}

	// --- Probe ---
	//
	// Step-32 moved the monitored-target list from yaml to the
	// active_targets table. yaml's job is now true configuration
	// (listen, retention, log, rdns); the operational tab set lives
	// in SQLite and the operator manages it from the UI. No yaml
	// target field to validate here.
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
	if c.Probe.MaxHops < 1 {
		problems = append(problems,
			fmt.Sprintf("probe.max_hops: must be >= 1, got %d", c.Probe.MaxHops))
	} else if c.Probe.MaxHops > maxHopsCeiling {
		problems = append(problems,
			fmt.Sprintf("probe.max_hops: must be <= %d, got %d", maxHopsCeiling, c.Probe.MaxHops))
	}
	if c.Probe.RouteChangeThreshold < 1 {
		problems = append(problems,
			fmt.Sprintf("probe.route_change_threshold: must be >= 1, got %d", c.Probe.RouteChangeThreshold))
	}

	// --- Agents ---
	//
	// Tokens gate the /api/ingest/* (remote probe) surface; a weak or accidentally
	// truncated token is a real misconfiguration, so enforce a floor.
	// 16 chars is well below a properly generated token (32 random
	// bytes base64 ≈ 44 chars) but catches "changeme" and paste
	// accidents.
	for i, tok := range c.Agents.Tokens {
		if strings.TrimSpace(tok) == "" {
			problems = append(problems,
				fmt.Sprintf("probes.tokens[%d]: must not be empty", i))
		} else if len(tok) < minAgentTokenLen {
			problems = append(problems,
				fmt.Sprintf("probes.tokens[%d]: must be at least %d characters (use a long random secret)", i, minAgentTokenLen))
		}
	}

	// --- Log ---
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

	// Wrap ErrConfigInvalid so callers can errors.Is against it to
	// distinguish "config invalid" from "config file unreadable".
	return fmt.Errorf("%w:\n  - %s", ErrConfigInvalid, strings.Join(problems, "\n  - "))
}

// ErrConfigInvalid is returned (wrapped) when validation fails. Callers
// that want to distinguish "config invalid" from "config file unreadable"
// can errors.Is against this.
var ErrConfigInvalid = errors.New("invalid configuration")

// contains reports whether s is in list.
func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}
