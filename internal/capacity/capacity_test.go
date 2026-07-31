package capacity

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/preston-peterson/hoptrail/internal/storage"
)

func testStore(t *testing.T) *storage.Store {
	t.Helper()
	s, err := storage.Open(filepath.Join(t.TempDir(), "cap.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

const gb = 1 << 30

var stdThresholds = Thresholds{FreeFloorMB: 1024, FreePctFloor: 0.05, HeadroomMin: 1.2}

func TestEvaluate_FreeFloor(t *testing.T) {
	// 100 GB volume, 4 GB free → below the 5% (5 GB) floor.
	m := Metrics{FreeBytes: 4 * gb, TotalBytes: 100 * gb}
	v := m.Evaluate(stdThresholds, false)
	if !v.Tripped {
		t.Fatalf("expected tripped on low free: %+v", v)
	}
	if v.Health != "critical" {
		t.Errorf("health = %q, want critical", v.Health)
	}

	// 20 GB free on the same volume clears the floor and has no growth.
	m.FreeBytes = 20 * gb
	v = m.Evaluate(stdThresholds, false)
	if v.Tripped || v.Health != "ok" {
		t.Errorf("expected ok/not-tripped: %+v", v)
	}
}

func TestEvaluate_MBFloorWinsOnSmallDisk(t *testing.T) {
	// 8 GB volume: 5% = 0.4 GB, so the 1 GB absolute floor dominates.
	// 700 MB free is under 1 GB → tripped.
	m := Metrics{FreeBytes: 700 * (1 << 20), TotalBytes: 8 * gb}
	if v := m.Evaluate(stdThresholds, false); !v.Tripped {
		t.Fatalf("MB floor should dominate on a small disk: %+v", v)
	}
}

func TestEvaluate_Headroom(t *testing.T) {
	// Free space above the floor (so only headroom drives the verdict),
	// but the projected settled size dwarfs what's available → headroom
	// 0.3 → critical. HeadroomRatio is taken from the field directly.
	m := Metrics{
		FreeBytes: 50 * gb, TotalBytes: 100 * gb, DBBytes: 1 * gb,
		HasGrowth: true, ProjectedBytes: 200 * gb, HeadroomRatio: 0.3,
		MBPerDay: 500, RetentionDays: 7,
	}
	v := m.Evaluate(stdThresholds, false)
	if !v.Tripped || v.Health != "critical" {
		t.Fatalf("expected critical headroom trip: %+v", v)
	}

	// Headroom 1.5 is comfortably above the 1.2 threshold → ok.
	m.HeadroomRatio = 1.5
	if v := m.Evaluate(stdThresholds, false); v.Tripped || v.Health != "ok" {
		t.Errorf("expected ok at 1.5x: %+v", v)
	}

	// Headroom 1.1: between 1.0 and 1.2 → warn, and tripped.
	m.HeadroomRatio = 1.1
	if v := m.Evaluate(stdThresholds, false); !v.Tripped || v.Health != "warn" {
		t.Errorf("expected warn+tripped at 1.1x: %+v", v)
	}
}

func TestEvaluate_Hysteresis(t *testing.T) {
	// Headroom exactly at the threshold: not tripped when inactive…
	m := Metrics{
		FreeBytes: 50 * gb, TotalBytes: 100 * gb, DBBytes: 1 * gb,
		HasGrowth: true, ProjectedBytes: 10 * gb, HeadroomRatio: 1.25,
		RetentionDays: 7,
	}
	if v := m.Evaluate(stdThresholds, false); v.Tripped {
		t.Fatalf("should not trip above threshold when inactive: %+v", v)
	}
	// …but while active, the clear bar is threshold×1.10 = 1.32, so 1.25
	// keeps the incident raised (no premature recovery / flap).
	if v := m.Evaluate(stdThresholds, true); !v.Tripped {
		t.Fatalf("hysteresis should keep it tripped while active at 1.25x: %+v", v)
	}
}

func TestEvaluate_Unknown(t *testing.T) {
	// No filesystem reading (TotalBytes 0) → unknown, never tripped.
	if v := (Metrics{}).Evaluate(stdThresholds, false); v.Health != "unknown" || v.Tripped {
		t.Errorf("empty metrics = %+v, want unknown/not-tripped", v)
	}
}

func TestMeasure_Growth(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "data.db")

	// Two samples 24h apart, +600 MB → 600 MB/day. retention 7 →
	// projected ≈ 4.2 GB.
	now := time.Now()
	if err := s.AppendDBSizeSample(ctx, now.Add(-24*time.Hour).UnixMilli(), 1*gb); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendDBSizeSample(ctx, now.UnixMilli(), 1*gb+600*(1<<20)); err != nil {
		t.Fatal(err)
	}

	m, err := Measure(ctx, s, dbPath, 7)
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if !m.HasGrowth {
		t.Fatal("expected HasGrowth")
	}
	if m.MBPerDay < 590 || m.MBPerDay > 610 {
		t.Errorf("MBPerDay = %.1f, want ~600", m.MBPerDay)
	}
	wantProjected := int64(600 * (1 << 20) * 7)
	if d := m.ProjectedBytes - wantProjected; d < -gb/10 || d > gb/10 {
		t.Errorf("ProjectedBytes = %d, want ~%d", m.ProjectedBytes, wantProjected)
	}
	if m.HeadroomRatio <= 0 {
		t.Errorf("HeadroomRatio = %v, want > 0", m.HeadroomRatio)
	}
	// Real filesystem under the temp dir — should report a nonzero total.
	if m.TotalBytes <= 0 {
		t.Errorf("TotalBytes = %d, want > 0 (statfs)", m.TotalBytes)
	}
}

func TestMeasure_InsufficientData(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "data.db")

	// One sample, or two samples spanning < minGrowthSpan → no slope.
	now := time.Now()
	if err := s.AppendDBSizeSample(ctx, now.Add(-30*time.Minute).UnixMilli(), 1*gb); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendDBSizeSample(ctx, now.UnixMilli(), 1*gb+10*(1<<20)); err != nil {
		t.Fatal(err)
	}
	m, err := Measure(ctx, s, dbPath, 7)
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if m.HasGrowth {
		t.Errorf("span %v < %v should not yield growth", 30*time.Minute, minGrowthSpan)
	}
	if m.HeadroomRatio != 0 || m.ProjectedBytes != 0 {
		t.Errorf("no projection without growth: %+v", m)
	}
}

func TestEffectiveRetentionDays(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if d := EffectiveRetentionDays(ctx, s, 7); d != 7 {
		t.Errorf("fallback = %d, want 7", d)
	}
	if err := s.SetConfig(ctx, "retention.days", "2"); err != nil {
		t.Fatal(err)
	}
	if d := EffectiveRetentionDays(ctx, s, 7); d != 2 {
		t.Errorf("override = %d, want 2", d)
	}
	// Out-of-range override falls back.
	if err := s.SetConfig(ctx, "retention.days", "99999"); err != nil {
		t.Fatal(err)
	}
	if d := EffectiveRetentionDays(ctx, s, 7); d != 7 {
		t.Errorf("bad override should fall back, got %d", d)
	}
}

func TestHumanize(t *testing.T) {
	cases := map[int64]string{
		512:           "512 B",
		2 * (1 << 10): "2 KB",
		5 * (1 << 20): "5 MB",
		3 * (1 << 30): "3.0 GB",
	}
	for in, want := range cases {
		if got := humanize(in); got != want {
			t.Errorf("humanize(%d) = %q, want %q", in, got, want)
		}
	}
}
