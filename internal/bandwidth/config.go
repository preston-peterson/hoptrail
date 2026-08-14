// Package bandwidth is hoptrail's v0.4 bandwidth-monitoring engine
// (docs/v0.4-bandwidth-design.md): scheduled Ookla-CLI speedtests,
// rolling-baseline asymmetric-derate detection, and the config
// plumbing for the settings panel. The hoptrail binary stays static —
// the speedtest CLI is an external, operator-installed capability.
package bandwidth

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/preston-peterson/hoptrail/internal/storage"
)

// Config keys in the storage config table. The compiled defaults
// below apply whenever a key is absent — the table only holds
// operator overrides and state flags.
const (
	KeyEnabled           = "bandwidth.enabled"
	KeyCadenceMode       = "bandwidth.cadence_mode"
	KeyIntervalMinutes   = "bandwidth.interval_minutes"
	KeyScheduledTimes    = "bandwidth.scheduled_times"
	KeyTimezone          = "bandwidth.timezone"
	KeyDirections        = "bandwidth.directions"
	KeyServerMode        = "bandwidth.server_mode"
	KeyServerID          = "bandwidth.server_id"
	KeyDerateThreshold   = "bandwidth.derate_threshold"
	KeyBaselineDays      = "bandwidth.baseline_days"
	KeyBaselineMetric    = "bandwidth.baseline_metric"
	KeyHealthFloorMbps   = "bandwidth.health_check_floor_mbps"
	KeyPauseICMP         = "bandwidth.pause_icmp_during_test"
	KeyInstallDismissed  = "bandwidth.install_banner_dismissed_for_version"
	KeyDerateDismissedTs = "bandwidth.derate_banner_dismissed_incident_ts"
	KeyRunInFlight       = "bandwidth.run_in_flight"
	// KeyChartWindow is the bandwidth chart's range selection
	// (step-145): display state, but server-persisted so it follows
	// the operator across browsers/origins — a fresh browser hitting
	// a new DNS name must not "lose" the chart to a reset picker.
	KeyChartWindow = "bandwidth.chart_window"
)

// MinBaselineSamples is the bootstrap gate: derate detection stays
// dormant until this many successful tests exist in the baseline
// window (design §"Resolved design calls").
const MinBaselineSamples = 7

// Config is the typed view of the bandwidth.* rows. All fields are
// operator-tunable in the settings panel; the zero value is NOT valid
// — construct via DefaultConfig or LoadConfig.
type Config struct {
	Enabled        bool
	CadenceMode    string   // times | interval (step-108)
	ChartWindow    string   // view | 1h | 6h | 24h | 7d | 30d (step-145)
	IntervalMin    int      // minutes between tests when CadenceMode=interval
	ScheduledTimes []string // "HH:MM" 24h wall-clock in Timezone
	Timezone       string   // IANA name; "" = system local
	Directions     string   // both | down_only | up_only (derate-detection scope)
	ServerMode     string   // auto | pinned
	ServerID       *int64   // used when pinned
	DerateThresh   float64  // fraction of baseline (0.1–0.9)
	BaselineDays   int
	BaselineMetric string // median | p50 | trimmed_mean
	HealthFloor    float64
	PauseICMP      bool
}

// DefaultConfig returns the design's §5 defaults.
func DefaultConfig() Config {
	return Config{
		Enabled:        false,
		CadenceMode:    "times",
		ChartWindow:    "view",
		IntervalMin:    60,
		ScheduledTimes: []string{"02:00"},
		Timezone:       "",
		Directions:     "both",
		ServerMode:     "auto",
		DerateThresh:   0.5,
		BaselineDays:   7,
		BaselineMetric: "median",
		HealthFloor:    10.0,
		PauseICMP:      true,
	}
}

// hhmmRe validates schedule entries.
var hhmmRe = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

// Validate checks operator-supplied values (the PATCH handler calls
// this before persisting). Aggregates problems like the yaml config
// validators do.
func (c Config) Validate() error {
	var problems []string
	switch c.ChartWindow {
	case "view", "1h", "6h", "24h", "7d", "30d":
	default:
		problems = append(problems, fmt.Sprintf("chart_window: %q is not view/1h/6h/24h/7d/30d", c.ChartWindow))
	}
	switch c.CadenceMode {
	case "times", "interval":
	default:
		problems = append(problems, fmt.Sprintf("cadence_mode: %q is not times/interval", c.CadenceMode))
	}
	if c.IntervalMin < 15 || c.IntervalMin > 1440 {
		problems = append(problems, fmt.Sprintf("interval_minutes: %d outside 15-1440", c.IntervalMin))
	}
	if n := len(c.ScheduledTimes); n < 1 || n > 6 {
		problems = append(problems, fmt.Sprintf("scheduled_times: need 1-6 entries, got %d", n))
	}
	for _, t := range c.ScheduledTimes {
		if !hhmmRe.MatchString(t) {
			problems = append(problems, fmt.Sprintf("scheduled_times: %q is not HH:MM (24h)", t))
		}
	}
	if c.Timezone != "" {
		if _, err := time.LoadLocation(c.Timezone); err != nil {
			problems = append(problems, fmt.Sprintf("timezone: %q is not an IANA zone name", c.Timezone))
		}
	}
	switch c.Directions {
	case "both", "down_only", "up_only":
	default:
		problems = append(problems, fmt.Sprintf("directions: %q is not both/down_only/up_only", c.Directions))
	}
	switch c.ServerMode {
	case "auto", "pinned":
	default:
		problems = append(problems, fmt.Sprintf("server_mode: %q is not auto/pinned", c.ServerMode))
	}
	if c.ServerMode == "pinned" && c.ServerID == nil {
		problems = append(problems, "server_id: required when server_mode is pinned")
	}
	if c.DerateThresh < 0.1 || c.DerateThresh > 0.9 {
		problems = append(problems, fmt.Sprintf("derate_threshold: %v outside 0.1-0.9", c.DerateThresh))
	}
	switch c.BaselineDays {
	case 1, 3, 7, 14, 30:
	default:
		problems = append(problems, fmt.Sprintf("baseline_days: %d not one of 1/3/7/14/30", c.BaselineDays))
	}
	switch c.BaselineMetric {
	case "median", "p50", "trimmed_mean":
	default:
		problems = append(problems, fmt.Sprintf("baseline_metric: %q not median/p50/trimmed_mean", c.BaselineMetric))
	}
	// Upper bound is a typo/garbage guard, not a hardware limit — multi-gig
	// residential (2.5G/5G/10G) is ordinary, so the ceiling is generous
	// enough never to constrain a real link (100 Gbps).
	if c.HealthFloor < 1.0 || c.HealthFloor > 100000.0 {
		problems = append(problems, fmt.Sprintf("health_check_floor_mbps: %v outside 1-100000", c.HealthFloor))
	}
	if len(problems) > 0 {
		return fmt.Errorf("bandwidth config invalid:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

// Location resolves the configured timezone, falling back to the
// system's local zone for "" or an unloadable name (the validator
// rejects bad names at write time; this fallback covers rows written
// before a zone was removed from the host's tzdata).
func (c Config) Location() *time.Location {
	if c.Timezone == "" {
		return time.Local
	}
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return time.Local
	}
	return loc
}

// LoadConfig reads the bandwidth.* rows and overlays them on the
// defaults. Unparseable stored values keep the default for that field
// (and only that field) — a corrupt row degrades one knob, not the
// whole feature.
func LoadConfig(ctx context.Context, store *storage.Store) (Config, error) {
	cfg := DefaultConfig()
	rows, err := store.ConfigWithPrefix(ctx, "bandwidth.")
	if err != nil {
		return cfg, err
	}
	if v, ok := rows[KeyEnabled]; ok {
		cfg.Enabled = v == "true"
	}
	if v, ok := rows[KeyChartWindow]; ok {
		switch v {
		case "view", "1h", "6h", "24h", "7d", "30d":
			cfg.ChartWindow = v
		}
	}
	if v, ok := rows[KeyCadenceMode]; ok {
		cfg.CadenceMode = v
	}
	if v, ok := rows[KeyIntervalMinutes]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.IntervalMin = n
		}
	}
	if v, ok := rows[KeyScheduledTimes]; ok {
		var times []string
		if json.Unmarshal([]byte(v), &times) == nil && len(times) > 0 {
			cfg.ScheduledTimes = times
		}
	}
	if v, ok := rows[KeyTimezone]; ok {
		cfg.Timezone = v
	}
	if v, ok := rows[KeyDirections]; ok {
		cfg.Directions = v
	}
	if v, ok := rows[KeyServerMode]; ok {
		cfg.ServerMode = v
	}
	if v, ok := rows[KeyServerID]; ok {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.ServerID = &id
		}
	}
	if v, ok := rows[KeyDerateThreshold]; ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.DerateThresh = f
		}
	}
	if v, ok := rows[KeyBaselineDays]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.BaselineDays = n
		}
	}
	if v, ok := rows[KeyBaselineMetric]; ok {
		cfg.BaselineMetric = v
	}
	if v, ok := rows[KeyHealthFloorMbps]; ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.HealthFloor = f
		}
	}
	if v, ok := rows[KeyPauseICMP]; ok {
		cfg.PauseICMP = v == "true"
	}
	return cfg, nil
}
