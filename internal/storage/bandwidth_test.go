package storage

import (
	"context"
	"testing"
)

// ---------- config key/value store ----------

func TestConfig_RoundTripAndAbsence(t *testing.T) {
	store := tempStore(t)
	ctx := context.Background()

	if _, ok, err := store.GetConfig(ctx, "bandwidth.enabled"); err != nil || ok {
		t.Fatalf("unset key: ok=%v err=%v, want absent no-error", ok, err)
	}
	if err := store.SetConfig(ctx, "bandwidth.enabled", "true"); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	v, ok, err := store.GetConfig(ctx, "bandwidth.enabled")
	if err != nil || !ok || v != "true" {
		t.Fatalf("GetConfig = (%q, %v, %v), want (true, true, nil)", v, ok, err)
	}
	// Upsert overwrites.
	if err := store.SetConfig(ctx, "bandwidth.enabled", "false"); err != nil {
		t.Fatalf("SetConfig overwrite: %v", err)
	}
	v, _, _ = store.GetConfig(ctx, "bandwidth.enabled")
	if v != "false" {
		t.Errorf("after overwrite = %q, want false", v)
	}
	// Delete clears; deleting absent keys is a no-op.
	if err := store.DeleteConfig(ctx, "bandwidth.enabled"); err != nil {
		t.Fatalf("DeleteConfig: %v", err)
	}
	if _, ok, _ := store.GetConfig(ctx, "bandwidth.enabled"); ok {
		t.Error("key still present after delete")
	}
	if err := store.DeleteConfig(ctx, "never-existed"); err != nil {
		t.Errorf("deleting absent key errored: %v", err)
	}
}

func TestConfigWithPrefix(t *testing.T) {
	store := tempStore(t)
	ctx := context.Background()
	for k, v := range map[string]string{
		"bandwidth.enabled":  "true",
		"bandwidth.timezone": "America/Chicago",
		"other.key":          "x",
	} {
		if err := store.SetConfig(ctx, k, v); err != nil {
			t.Fatalf("SetConfig %s: %v", k, err)
		}
	}
	got, err := store.ConfigWithPrefix(ctx, "bandwidth.")
	if err != nil {
		t.Fatalf("ConfigWithPrefix: %v", err)
	}
	if len(got) != 2 || got["bandwidth.enabled"] != "true" || got["bandwidth.timezone"] != "America/Chicago" {
		t.Errorf("prefix result = %v, want exactly the two bandwidth keys", got)
	}
}

// ---------- bandwidth samples ----------

func bwSample(ts int64, down, up float64, ok bool) BandwidthSample {
	return BandwidthSample{
		Ts: ts, DownMbps: down, UpMbps: up, PingMs: 12.3,
		BytesDown: 1 << 27, BytesUp: 1 << 27, DurationMs: 30_000, Ok: ok,
	}
}

func TestBandwidthSamples_RecordListLatest(t *testing.T) {
	store := tempStore(t)
	ctx := context.Background()

	name := "Test ISP — Milwaukee"
	id := int64(4242)
	errMsg := "speedtest: socket timeout"
	full := bwSample(1000, 940.5, 880.2, true)
	full.ServerID = &id
	full.ServerName = &name
	failed := BandwidthSample{Ts: 2000, Ok: false, Error: &errMsg}

	for _, smp := range []BandwidthSample{full, failed, bwSample(3000, 950, 890, true)} {
		if err := store.RecordBandwidthSample(ctx, smp); err != nil {
			t.Fatalf("Record ts=%d: %v", smp.Ts, err)
		}
	}

	got, err := store.ListBandwidthSamples(ctx, 1000, 2500)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("window [1000,2500] = %d rows, want 2", len(got))
	}
	if got[0].ServerID == nil || *got[0].ServerID != 4242 || got[0].ServerName == nil || *got[0].ServerName != name {
		t.Errorf("row0 server = %v/%v, want 4242/%q", got[0].ServerID, got[0].ServerName, name)
	}
	if got[1].Ok || got[1].Error == nil || *got[1].Error != errMsg {
		t.Errorf("row1 = %+v, want failed row with error preserved", got[1])
	}

	latest, err := store.LatestBandwidthSample(ctx)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if latest == nil || latest.Ts != 3000 {
		t.Errorf("latest = %+v, want ts=3000", latest)
	}
}

func TestLatestBandwidthSample_EmptyTableNil(t *testing.T) {
	store := tempStore(t)
	latest, err := store.LatestBandwidthSample(context.Background())
	if err != nil || latest != nil {
		t.Errorf("empty table: (%v, %v), want (nil, nil)", latest, err)
	}
}

// ---------- baseline computation ----------

func TestComputeBandwidthBaseline(t *testing.T) {
	store := tempStore(t)
	ctx := context.Background()
	day := int64(86_400_000)
	now := int64(100 * day)

	// 8 good samples in-window with one outlier; plus rows that must
	// be EXCLUDED: a failed run, a below-floor pathological run, and
	// an out-of-window old run.
	downs := []float64{1000, 1010, 990, 1020, 980, 1005, 995, 400} // 400 = outlier
	for i, d := range downs {
		if err := store.RecordBandwidthSample(ctx, bwSample(now-int64(i+1)*day/2, d, d-50, true)); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	excluded := []BandwidthSample{
		{Ts: now - day + 7, Ok: false},         // failed (ts offset avoids the half-day seed grid)
		bwSample(now-day-1, 5, 5, true),        // below 10 Mbps floor
		bwSample(now-10*day, 2000, 2000, true), // outside 7d window
	}
	for _, smp := range excluded {
		if err := store.RecordBandwidthSample(ctx, smp); err != nil {
			t.Fatalf("seed excluded: %v", err)
		}
	}

	b, err := store.ComputeBandwidthBaseline(ctx, "median", 7, 10.0, now, 7)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if b == nil {
		t.Fatal("baseline nil, want computed (8 qualifying samples ≥ 7 min)")
	}
	if b.N != 8 {
		t.Errorf("N = %d, want 8 (failed/floor/old rows excluded)", b.N)
	}
	// Median of {400,980,990,995,1000,1005,1010,1020} = (995+1000)/2.
	if b.DownMbps != 997.5 {
		t.Errorf("median down = %v, want 997.5", b.DownMbps)
	}
	if b.ComputedAt != now {
		t.Errorf("ComputedAt = %d, want %d", b.ComputedAt, now)
	}

	// Bootstrap rule: not enough samples → nil, no error.
	b, err = store.ComputeBandwidthBaseline(ctx, "median", 7, 10.0, now, 9)
	if err != nil || b != nil {
		t.Errorf("below minSamples: (%v, %v), want (nil, nil)", b, err)
	}

	// trimmed_mean drops the 400 outlier (and the 1020 top) at n=8
	// (10% of 8 floors to 0... so trims nothing; verify fallback math
	// instead at a size where trimming bites: use the same set with
	// minSamples satisfied and metric unknown → median fallback).
	b, err = store.ComputeBandwidthBaseline(ctx, "definitely-not-a-metric", 7, 10.0, now, 7)
	if err != nil || b == nil || b.DownMbps != 997.5 {
		t.Errorf("unknown metric: %+v, %v — want median fallback 997.5", b, err)
	}
}

func TestTrimmedMean(t *testing.T) {
	// 10 values → trim 1 from each end: drops 0 and 1000.
	vals := []float64{0, 100, 100, 100, 100, 100, 100, 100, 100, 1000}
	if got := trimmedMean(vals); got != 100 {
		t.Errorf("trimmedMean = %v, want 100", got)
	}
	// Small set (n=4): 10% floors to 0, degrades to plain mean.
	if got := trimmedMean([]float64{1, 2, 3, 4}); got != 2.5 {
		t.Errorf("small-set trimmedMean = %v, want 2.5", got)
	}
}
