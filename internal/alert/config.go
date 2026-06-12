// Package alert is the v0.6 alerting engine: rule evaluation against
// data the daemon already has, an incident state machine with sustain
// and recovery, gating (enabled → per-event → quiet hours → cooldown →
// rate limit), and a persistent delivery queue drained by an ntfy
// sender. Design: docs/design/v0.6-alerting-design.md.
//
// Convention note: this package depends only on internal/storage and
// stdlib — rule inputs arrive as primitives via injected providers.
package alert

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/preston-peterson/hoptrail/internal/storage"
)

// Event type identifiers — also the alert_state.event_type values.
const (
	EventProbeOffline = "probe_offline"
	EventTargetLoss   = "target_loss"
	EventLatency      = "latency"
	EventDerate       = "derate"
)

// Config is the operator-tunable alerting configuration, persisted as
// alert.* config KV rows (absent row = compiled default).
type Config struct {
	Enabled   bool
	ServerURL string // ntfy server, e.g. http://127.0.0.1:2586
	Topic     string
	Token     string // optional bearer token

	EventProbeOffline bool
	EventTargetLoss   bool
	EventLatency      bool
	EventDerate       bool

	LossPct      float64 // destination loss % that counts as down (1-100)
	SustainS     int     // condition must hold this long before alerting (30-3600)
	LatencyLevel string  // which tab threshold alerts: warning|critical

	CooldownS      int    // per-incident re-raise suppression (60-86400)
	RateLimitPerH  int    // global ceiling on alerts/hour (1-120)
	QuietStart     string // "HH:MM" local time; both empty = no quiet hours
	QuietEnd       string
}

// DefaultConfig per design §4.
func DefaultConfig() Config {
	return Config{
		EventProbeOffline: true,
		EventTargetLoss:   true,
		EventLatency:      false,
		EventDerate:       true,
		LossPct:           20,
		SustainS:          120,
		LatencyLevel:      "critical",
		CooldownS:         1800,
		RateLimitPerH:     12,
	}
}

// Validate checks a composite config.
func (c Config) Validate() error {
	if c.ServerURL != "" && !strings.HasPrefix(c.ServerURL, "http://") && !strings.HasPrefix(c.ServerURL, "https://") {
		return fmt.Errorf("server_url must start with http:// or https://")
	}
	if c.Enabled && (c.ServerURL == "" || c.Topic == "") {
		return fmt.Errorf("enabling alerts requires server_url and topic")
	}
	if c.Topic != "" && strings.ContainsAny(c.Topic, " /") {
		return fmt.Errorf("topic must not contain spaces or slashes")
	}
	if c.LossPct < 1 || c.LossPct > 100 {
		return fmt.Errorf("loss_pct %v out of range 1-100", c.LossPct)
	}
	if c.SustainS < 30 || c.SustainS > 3600 {
		return fmt.Errorf("sustain_s %d out of range 30-3600", c.SustainS)
	}
	if c.LatencyLevel != "warning" && c.LatencyLevel != "critical" {
		return fmt.Errorf("latency_level must be warning or critical")
	}
	if c.CooldownS < 60 || c.CooldownS > 86400 {
		return fmt.Errorf("cooldown_s %d out of range 60-86400", c.CooldownS)
	}
	if c.RateLimitPerH < 1 || c.RateLimitPerH > 120 {
		return fmt.Errorf("rate_limit_per_h %d out of range 1-120", c.RateLimitPerH)
	}
	if (c.QuietStart == "") != (c.QuietEnd == "") {
		return fmt.Errorf("quiet_start and quiet_end must be set together")
	}
	for _, v := range []string{c.QuietStart, c.QuietEnd} {
		if v != "" {
			if _, _, err := parseHHMM(v); err != nil {
				return err
			}
		}
	}
	return nil
}

func parseHHMM(v string) (h, m int, err error) {
	parts := strings.SplitN(v, ":", 2)
	if len(parts) == 2 {
		h, err1 := strconv.Atoi(parts[0])
		m, err2 := strconv.Atoi(parts[1])
		if err1 == nil && err2 == nil && h >= 0 && h <= 23 && m >= 0 && m <= 59 {
			return h, m, nil
		}
	}
	return 0, 0, fmt.Errorf("time %q must be HH:MM (24h)", v)
}

// kv maps config keys to read/write closures — the same
// corrupt-row-keeps-field-default loader shape the bandwidth package
// established.
const keyPrefix = "alert."

func (c *Config) fields() map[string]struct {
	get func() string
	set func(string) bool
} {
	boolField := func(p *bool) struct {
		get func() string
		set func(string) bool
	} {
		return struct {
			get func() string
			set func(string) bool
		}{
			get: func() string { return strconv.FormatBool(*p) },
			set: func(v string) bool { b, err := strconv.ParseBool(v); if err == nil { *p = b }; return err == nil },
		}
	}
	strField := func(p *string) struct {
		get func() string
		set func(string) bool
	} {
		return struct {
			get func() string
			set func(string) bool
		}{get: func() string { return *p }, set: func(v string) bool { *p = v; return true }}
	}
	intField := func(p *int) struct {
		get func() string
		set func(string) bool
	} {
		return struct {
			get func() string
			set func(string) bool
		}{
			get: func() string { return strconv.Itoa(*p) },
			set: func(v string) bool { n, err := strconv.Atoi(v); if err == nil { *p = n }; return err == nil },
		}
	}
	return map[string]struct {
		get func() string
		set func(string) bool
	}{
		"enabled":              boolField(&c.Enabled),
		"server_url":           strField(&c.ServerURL),
		"topic":                strField(&c.Topic),
		"token":                strField(&c.Token),
		"events.probe_offline": boolField(&c.EventProbeOffline),
		"events.target_loss":   boolField(&c.EventTargetLoss),
		"events.latency":       boolField(&c.EventLatency),
		"events.derate":        boolField(&c.EventDerate),
		"loss_pct": {
			get: func() string { return strconv.FormatFloat(c.LossPct, 'f', -1, 64) },
			set: func(v string) bool { f, err := strconv.ParseFloat(v, 64); if err == nil { c.LossPct = f }; return err == nil },
		},
		"sustain_s":       intField(&c.SustainS),
		"latency_level":   strField(&c.LatencyLevel),
		"cooldown_s":      intField(&c.CooldownS),
		"rate_limit_per_h": intField(&c.RateLimitPerH),
		"quiet_start":     strField(&c.QuietStart),
		"quiet_end":       strField(&c.QuietEnd),
	}
}

// LoadConfig reads alert.* rows over the compiled defaults. A corrupt
// row keeps that field's default (and is logged by the caller via the
// returned warnings) rather than failing startup.
func LoadConfig(ctx context.Context, store *storage.Store) (Config, []string, error) {
	c := DefaultConfig()
	warnings := []string{}
	for key, f := range c.fields() {
		v, ok, err := store.GetConfig(ctx, keyPrefix+key)
		if err != nil {
			return c, warnings, fmt.Errorf("alert: load config %s: %w", key, err)
		}
		if ok && !f.set(v) {
			warnings = append(warnings, fmt.Sprintf("alert.%s: unparseable value %q, keeping default", key, v))
		}
	}
	return c, warnings, nil
}

// SaveConfig writes every field as its config row.
func SaveConfig(ctx context.Context, store *storage.Store, c Config) error {
	for key, f := range c.fields() {
		if err := store.SetConfig(ctx, keyPrefix+key, f.get()); err != nil {
			return fmt.Errorf("alert: save config %s: %w", key, err)
		}
	}
	return nil
}
