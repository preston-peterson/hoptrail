// Package capacity measures disk headroom and database growth so the
// daemon can warn before the disk fills rather than after. It reports
// free/total disk on the database's filesystem, the database footprint,
// an empirical growth rate (MB/day) from the hourly size series, and a
// projection of where the database settles at the current retention.
//
// Convention: depends only on internal/storage and stdlib. Threshold
// policy is passed in (Thresholds) rather than imported from the alert
// package, so both the alert engine and the status endpoint can share
// the same measurement and evaluation without a dependency cycle.
package capacity

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/preston-peterson/hoptrail/internal/storage"
)

const (
	miB = 1 << 20
	day = 24 * time.Hour

	// growthWindow caps how far back the growth estimate looks. A
	// couple of weeks is plenty to establish a slope.
	growthWindow = 14 * day
	// minGrowthSpan is the minimum time the size series must cover
	// before a slope is trustworthy. Below this the monitor reports
	// "still collecting" and only the absolute free-space floor is
	// armed (no projection-based alerting on noise).
	minGrowthSpan = 2 * time.Hour
	// recoverHysteresis: an active capacity alert clears only when the
	// metric recovers this far past its threshold, so a value hovering
	// at the line doesn't flap raise/recover.
	recoverHysteresis = 1.10
)

// Metrics is one capacity measurement.
type Metrics struct {
	FreeBytes  int64 // available to unprivileged users on the DB's filesystem
	TotalBytes int64 // total size of that filesystem
	DBBytes    int64 // main database file
	WALBytes   int64 // write-ahead log (transient, but occupies disk)

	RetentionDays int

	// Growth fields are only meaningful when HasGrowth is true.
	HasGrowth      bool
	MBPerDay       float64 // measured slope of the database file size
	DaysOfData     float64 // span the size series covers
	ProjectedBytes int64   // size the DB settles at: growth/day × retention
	HeadroomRatio  float64 // (free + db) / projected; <1 means it will fill
}

// Thresholds is the operator-tunable policy, supplied by the caller.
type Thresholds struct {
	FreeFloorMB  int     // alert when free disk drops below this many MB…
	FreePctFloor float64 // …or below this fraction of total (whichever is larger)
	HeadroomMin  float64 // alert when the headroom ratio drops below this
}

// Verdict is the evaluation of Metrics against Thresholds.
type Verdict struct {
	Health  string // "ok" | "warn" | "critical" | "unknown"
	Tripped bool   // an alert condition is met (with hysteresis when active)
	Reason  string // human-readable cause, for the alert body
}

// Measure gathers the current capacity picture. retentionDays drives
// the projection; pass the effective (live) value.
func Measure(ctx context.Context, store *storage.Store, dbPath string, retentionDays int) (Metrics, error) {
	m := Metrics{RetentionDays: retentionDays}

	if dbPath != "" {
		if fi, err := os.Stat(dbPath); err == nil {
			m.DBBytes = fi.Size()
		}
		if fi, err := os.Stat(dbPath + "-wal"); err == nil {
			m.WALBytes = fi.Size()
		}
		free, total, err := diskUsage(filepath.Dir(dbPath))
		if err != nil {
			return m, fmt.Errorf("capacity: statfs %s: %w", filepath.Dir(dbPath), err)
		}
		m.FreeBytes, m.TotalBytes = free, total
	}

	// Growth from the hourly size series.
	sinceMs := time.Now().Add(-growthWindow).UnixMilli()
	samples, err := store.DBSizeSamples(ctx, sinceMs)
	if err != nil {
		return m, fmt.Errorf("capacity: db size samples: %w", err)
	}
	if len(samples) >= 2 {
		first, last := samples[0], samples[len(samples)-1]
		span := time.Duration(last.Ts-first.Ts) * time.Millisecond
		if span >= minGrowthSpan {
			m.HasGrowth = true
			m.DaysOfData = span.Hours() / 24
			bytesPerDay := float64(last.Bytes-first.Bytes) / (span.Hours() / 24)
			m.MBPerDay = bytesPerDay / miB
			// Projection: data accumulates for retentionDays before the
			// oldest is pruned, so the steady-state size ≈ growth/day ×
			// retention. A flat or shrinking DB has no growth risk.
			if bytesPerDay > 0 && retentionDays > 0 {
				m.ProjectedBytes = int64(bytesPerDay * float64(retentionDays))
				// The DB's own space is reclaimable (cut retention), so it
				// counts toward what's available to absorb the projection.
				m.HeadroomRatio = float64(m.FreeBytes+m.DBBytes) / float64(m.ProjectedBytes)
			}
		}
	}
	return m, nil
}

// freeFloor is the effective absolute floor in bytes: the larger of the
// MB floor and the percentage-of-total floor, so one setting adapts to
// both a tiny SD card and a large volume.
func (m Metrics) freeFloor(t Thresholds) int64 {
	mbFloor := int64(t.FreeFloorMB) * miB
	pctFloor := int64(t.FreePctFloor * float64(m.TotalBytes))
	if pctFloor > mbFloor {
		return pctFloor
	}
	return mbFloor
}

// Evaluate classifies the metrics against thresholds. When active is
// true (an alert is already raised) the trip thresholds are widened by
// recoverHysteresis, so the alert clears only after a clear margin.
func (m Metrics) Evaluate(t Thresholds, active bool) Verdict {
	if m.TotalBytes == 0 {
		return Verdict{Health: "unknown"}
	}
	floor := m.freeFloor(t)
	hyst := 1.0
	if active {
		hyst = recoverHysteresis
	}

	freeLow := m.FreeBytes < int64(float64(floor)*hyst)
	headroomLow := false
	if m.HasGrowth && m.ProjectedBytes > 0 {
		headroomLow = m.HeadroomRatio < t.HeadroomMin*hyst
	}

	v := Verdict{Tripped: freeLow || headroomLow}

	// Health dot is threshold-relative and independent of the active
	// hysteresis (the card shows current standing, not alert state).
	switch {
	case m.FreeBytes < floor || (m.HasGrowth && m.ProjectedBytes > 0 && m.HeadroomRatio < 1.0):
		v.Health = "critical"
	case (m.HasGrowth && m.ProjectedBytes > 0 && m.HeadroomRatio < t.HeadroomMin) || m.FreeBytes < floor*2:
		v.Health = "warn"
	default:
		v.Health = "ok"
	}

	// Reason, for the alert body.
	switch {
	case freeLow && headroomLow:
		v.Reason = fmt.Sprintf("only %s free (floor %s) and %s",
			humanize(m.FreeBytes), humanize(floor), m.headroomPhrase())
	case freeLow:
		v.Reason = fmt.Sprintf("only %s free of %s (floor %s)",
			humanize(m.FreeBytes), humanize(m.TotalBytes), humanize(floor))
	case headroomLow:
		v.Reason = m.headroomPhrase()
	}
	return v
}

// headroomPhrase describes the growth projection, with an estimated
// time-to-full when the disk is actively shrinking.
func (m Metrics) headroomPhrase() string {
	s := fmt.Sprintf("DB %s growing %.0f MB/day → projected %s at %dd retention, headroom %.2f×",
		humanize(m.DBBytes), m.MBPerDay, humanize(m.ProjectedBytes), m.RetentionDays, m.HeadroomRatio)
	if m.MBPerDay > 0 {
		daysToFull := float64(m.FreeBytes) / (m.MBPerDay * miB)
		s += fmt.Sprintf(" (fills disk in ~%.0f days)", daysToFull)
	}
	return s
}

// AlertMessage is the ntfy body for a raised capacity alert.
func (m Metrics) AlertMessage(reason string) string {
	return fmt.Sprintf("low disk on the central: %s. Reduce retention or grow the volume.", reason)
}

// RecoverMessage is the ntfy body for the paired recovery.
func (m Metrics) RecoverMessage() string {
	return fmt.Sprintf("disk recovered: %s free of %s", humanize(m.FreeBytes), humanize(m.TotalBytes))
}

// diskUsage reports bytes available to unprivileged users and the total
// size of the filesystem at path (Linux statfs).
func diskUsage(path string) (free, total int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bsize := int64(st.Bsize)
	return int64(st.Bavail) * bsize, int64(st.Blocks) * bsize, nil
}

// EffectiveRetentionDays returns the live retention policy: the
// operator-set SQLite row when present, else the supplied fallback
// (the yaml value). Mirrors the retention worker's own precedence so
// the projection uses the same window the worker enforces.
func EffectiveRetentionDays(ctx context.Context, store *storage.Store, fallback int) int {
	if v, ok, err := store.GetConfig(ctx, "retention.days"); err == nil && ok {
		if n, perr := strconv.Atoi(v); perr == nil && n >= 1 && n <= 3650 {
			return n
		}
	}
	return fallback
}

// humanize formats a byte count as a compact GB/MB/KB string.
func humanize(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
