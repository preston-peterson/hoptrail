package bandwidth

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/preston-peterson/hoptrail/internal/storage"
)

func testStore(t *testing.T) *storage.Store {
	t.Helper()
	s, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// ---------- config ----------

func TestLoadConfig_DefaultsAndOverlay(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	cfg, err := LoadConfig(ctx, store)
	if err != nil {
		t.Fatalf("LoadConfig empty: %v", err)
	}
	def := DefaultConfig()
	if cfg.Enabled != def.Enabled || cfg.DerateThresh != 0.5 || len(cfg.ScheduledTimes) != 1 || cfg.ScheduledTimes[0] != "02:00" {
		t.Errorf("empty-table config = %+v, want defaults", cfg)
	}

	// Overlay a few rows, including one corrupt one (must keep that
	// field's default, not break the load).
	for k, v := range map[string]string{
		KeyEnabled:         "true",
		KeyScheduledTimes:  `["02:00","14:30"]`,
		KeyDerateThreshold: "0.3",
		KeyBaselineDays:    "not-a-number", // corrupt → default kept
		KeyServerID:        "4242",
	} {
		if err := store.SetConfig(ctx, k, v); err != nil {
			t.Fatalf("SetConfig: %v", err)
		}
	}
	cfg, err = LoadConfig(ctx, store)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.Enabled || cfg.DerateThresh != 0.3 || len(cfg.ScheduledTimes) != 2 {
		t.Errorf("overlay config = %+v", cfg)
	}
	if cfg.BaselineDays != 7 {
		t.Errorf("corrupt baseline_days: got %d, want default 7 kept", cfg.BaselineDays)
	}
	if cfg.ServerID == nil || *cfg.ServerID != 4242 {
		t.Errorf("server_id = %v, want 4242", cfg.ServerID)
	}
}

func TestConfigValidate(t *testing.T) {
	valid := DefaultConfig()
	if err := valid.Validate(); err != nil {
		t.Fatalf("defaults rejected: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"no times", func(c *Config) { c.ScheduledTimes = nil }},
		{"7 times", func(c *Config) { c.ScheduledTimes = make([]string, 7) }},
		{"bad hh:mm", func(c *Config) { c.ScheduledTimes = []string{"25:00"} }},
		{"bad tz", func(c *Config) { c.Timezone = "Mars/Olympus" }},
		{"bad directions", func(c *Config) { c.Directions = "sideways" }},
		{"pinned without id", func(c *Config) { c.ServerMode = "pinned" }},
		{"threshold too high", func(c *Config) { c.DerateThresh = 0.95 }},
		{"bad baseline days", func(c *Config) { c.BaselineDays = 5 }},
		{"bad metric", func(c *Config) { c.BaselineMetric = "mode" }},
		{"floor too low", func(c *Config) { c.HealthFloor = 0.5 }},
		{"floor absurd (>100Gbps)", func(c *Config) { c.HealthFloor = 100001 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tc.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Errorf("mutation %q validated, want error", tc.name)
			}
		})
	}

	// Multi-gig floors are legitimate — the old 1000 ceiling wrongly
	// rejected them on 2.5G/5G/10G lines (backlog #14). 2300 is the
	// operator's real link.
	for _, mbps := range []float64{1500, 2300, 10000} {
		cfg := DefaultConfig()
		cfg.HealthFloor = mbps
		if err := cfg.Validate(); err != nil {
			t.Errorf("multi-gig floor %v Mbps rejected: %v", mbps, err)
		}
	}
}

// ---------- capability ----------

func TestDetectCapability(t *testing.T) {
	ok := func(ctx context.Context, args ...string) ([]byte, error) {
		return []byte("Speedtest by Ookla 1.2.0.84 (ea6b6773cf) Linux/x86_64\n"), nil
	}
	cap := DetectCapability(context.Background(), ok)
	if !cap.Available || cap.Version != "1.2.0.84" {
		t.Errorf("capability = %+v, want available 1.2.0.84", cap)
	}

	missing := func(ctx context.Context, args ...string) ([]byte, error) {
		return nil, errors.New(`speedtest: exec: "speedtest": executable file not found in $PATH`)
	}
	cap = DetectCapability(context.Background(), missing)
	if cap.Available || cap.Error == "" {
		t.Errorf("missing CLI: %+v, want unavailable with error", cap)
	}
}

// ---------- schedule ----------

func TestNextScheduled(t *testing.T) {
	chi, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("tz: %v", err)
	}

	// 2026-06-11 10:00 CDT; entries 02:00 + 14:30 → next is 14:30 today.
	now := time.Date(2026, 6, 11, 10, 0, 0, 0, chi)
	next, ok := NextScheduled(now, []string{"02:00", "14:30"}, chi)
	if !ok || !next.Equal(time.Date(2026, 6, 11, 14, 30, 0, 0, chi)) {
		t.Errorf("next = %v ok=%v, want today 14:30", next, ok)
	}

	// 23:00 → next is 02:00 tomorrow.
	now = time.Date(2026, 6, 11, 23, 0, 0, 0, chi)
	next, ok = NextScheduled(now, []string{"02:00"}, chi)
	if !ok || !next.Equal(time.Date(2026, 6, 12, 2, 0, 0, 0, chi)) {
		t.Errorf("next = %v ok=%v, want tomorrow 02:00", next, ok)
	}

	// DST spring-forward (US 2026: March 8, 02:00→03:00 — 02:30 does
	// not exist that day). From 01:00 CST the 02:30 entry must skip to
	// March 9, not shift to 03:30 on the 8th.
	now = time.Date(2026, 3, 8, 1, 0, 0, 0, chi)
	next, ok = NextScheduled(now, []string{"02:30"}, chi)
	want := time.Date(2026, 3, 9, 2, 30, 0, 0, chi)
	if !ok || !next.Equal(want) {
		t.Errorf("DST skip: next = %v ok=%v, want %v", next, ok, want)
	}

	// Garbage entries → ok=false.
	if _, ok = NextScheduled(now, []string{"nonsense"}, chi); ok {
		t.Error("garbage schedule produced a next time")
	}
}

// ---------- runner ----------

const cannedResult = `{"type":"result","ping":{"latency":12.3},
"download":{"bandwidth":117500000,"bytes":1000000000,"elapsed":15000},
"upload":{"bandwidth":110000000,"bytes":900000000,"elapsed":15000},
"server":{"id":4242,"name":"Test ISP - Milwaukee"}}`

func TestRunner_ManualRunStoresSample(t *testing.T) {
	store := testStore(t)
	run := func(ctx context.Context, args ...string) ([]byte, error) {
		return []byte(cannedResult), nil
	}
	r := NewRunner(store, DefaultConfig(), run, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	if !r.RunNow() {
		t.Fatal("RunNow rejected with nothing in flight")
	}
	deadline := time.Now().Add(2 * time.Second)
	var smp *storage.BandwidthSample
	for time.Now().Before(deadline) {
		var err error
		smp, err = store.LatestBandwidthSample(context.Background())
		if err != nil {
			t.Fatalf("Latest: %v", err)
		}
		if smp != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if smp == nil {
		t.Fatal("no sample stored after manual run")
	}
	// 117500000 bytes/sec → 940 Mbps.
	if !smp.Ok || smp.DownMbps != 940 || smp.UpMbps != 880 {
		t.Errorf("sample = %+v, want ok 940/880 Mbps", smp)
	}
	if smp.ServerID == nil || *smp.ServerID != 4242 || smp.PingMs != 12.3 {
		t.Errorf("metadata = %+v", smp)
	}
	if smp.DerateFlag {
		t.Error("first-ever sample flagged derated — baseline gate should keep detection dormant")
	}
	// run_in_flight row must be cleared after completion.
	if _, ok, _ := store.GetConfig(context.Background(), KeyRunInFlight); ok {
		t.Error("run_in_flight row still set after test completed")
	}
}

func TestRunner_FailedTestStoredWithError(t *testing.T) {
	store := testStore(t)
	run := func(ctx context.Context, args ...string) ([]byte, error) {
		return nil, errors.New("speedtest: socket timeout")
	}
	r := NewRunner(store, DefaultConfig(), run, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	r.RunNow()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		smp, _ := store.LatestBandwidthSample(context.Background())
		if smp != nil {
			if smp.Ok || smp.Error == nil {
				t.Errorf("failed run stored as %+v, want ok=false with error", smp)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no failure sample stored")
}

// TestRunner_DerateFlagAndDismissalClear seeds a healthy baseline,
// runs a derated test (upload at 20% of baseline), and verifies the
// flag + that a subsequent healthy test clears the dismissal row.
func TestRunner_DerateFlagAndDismissalClear(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	day := int64(86_400_000)
	nowMs := time.Now().UnixMilli()
	for i := 1; i <= 8; i++ {
		if err := store.RecordBandwidthSample(ctx, storage.BandwidthSample{
			Ts: nowMs - int64(i)*day/2, Ok: true,
			DownMbps: 1000, UpMbps: 950, PingMs: 12,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// Operator dismissed an earlier incident.
	if err := store.SetConfig(ctx, KeyDerateDismissedTs, "12345"); err != nil {
		t.Fatal(err)
	}

	derated := `{"type":"result","ping":{"latency":12},
"download":{"bandwidth":117500000,"bytes":1,"elapsed":1},
"upload":{"bandwidth":23500000,"bytes":1,"elapsed":1},
"server":{"id":1,"name":"x"}}` // upload 188 Mbps vs 950 baseline → derated
	healthy := cannedResult

	outputs := []string{derated, healthy}
	idx := 0
	run := func(ctx context.Context, args ...string) ([]byte, error) {
		out := outputs[idx]
		if idx < len(outputs)-1 {
			idx++
		}
		return []byte(out), nil
	}
	r := NewRunner(store, DefaultConfig(), run, nil, nil)
	rctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(rctx)

	waitSample := func(wantTsAfter int64) *storage.BandwidthSample {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			smp, _ := store.LatestBandwidthSample(ctx)
			if smp != nil && smp.Ts > wantTsAfter {
				return smp
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("timed out waiting for sample")
		return nil
	}

	r.RunNow()
	smp := waitSample(nowMs - 1)
	if !smp.DerateFlag {
		t.Fatalf("derated run not flagged: %+v", smp)
	}
	if _, ok, _ := store.GetConfig(ctx, KeyDerateDismissedTs); !ok {
		t.Fatal("dismissal cleared by a DERATED sample — must persist until resolution")
	}

	// Healthy test resolves the incident → dismissal row cleared.
	for !r.RunNow() {
		time.Sleep(10 * time.Millisecond)
	}
	smp2 := waitSample(smp.Ts)
	if smp2.DerateFlag {
		t.Fatalf("healthy run flagged: %+v", smp2)
	}
	if _, ok, _ := store.GetConfig(ctx, KeyDerateDismissedTs); ok {
		t.Error("dismissal row not cleared on resolution")
	}
}

// fakePauser records pause/resume around a test run.
type fakePauser struct{ paused, resumed int }

func (p *fakePauser) PauseProbing()  { p.paused++ }
func (p *fakePauser) ResumeProbing() { p.resumed++ }

func TestRunner_PausesICMPWhenConfigured(t *testing.T) {
	store := testStore(t)
	p := &fakePauser{}
	run := func(ctx context.Context, args ...string) ([]byte, error) {
		if p.paused != 1 || p.resumed != 0 {
			return nil, fmt.Errorf("pause state during test = %d/%d, want 1/0", p.paused, p.resumed)
		}
		return []byte(cannedResult), nil
	}
	r := NewRunner(store, DefaultConfig(), run, p, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	r.RunNow()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		smp, _ := store.LatestBandwidthSample(context.Background())
		if smp != nil {
			if smp.Error != nil {
				t.Fatalf("test errored: %s", *smp.Error)
			}
			if p.paused != 1 || p.resumed != 1 {
				t.Errorf("pause/resume = %d/%d, want 1/1", p.paused, p.resumed)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no sample stored")
}

func TestRunner_PinnedServerArg(t *testing.T) {
	store := testStore(t)
	var gotArgs []string
	run := func(ctx context.Context, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(cannedResult), nil
	}
	cfg := DefaultConfig()
	id := int64(777)
	cfg.ServerMode = "pinned"
	cfg.ServerID = &id
	r := NewRunner(store, cfg, run, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	r.RunNow()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if smp, _ := store.LatestBandwidthSample(context.Background()); smp != nil {
			found := false
			for _, a := range gotArgs {
				if a == "--server-id=777" {
					found = true
				}
			}
			if !found {
				t.Errorf("args = %v, want --server-id=777", gotArgs)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no sample stored")
}

func TestNextIntervalRun(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	// No samples yet → 30s out.
	if got := NextIntervalRun(nil, 60, now); !got.Equal(now.Add(30 * time.Second)) {
		t.Errorf("nil latest: %v", got)
	}
	// Latest 20m ago, hourly cadence → 40m out.
	latest := &storage.BandwidthSample{Ts: now.Add(-20 * time.Minute).UnixMilli()}
	if got := NextIntervalRun(latest, 60, now); !got.Equal(now.Add(40 * time.Minute)) {
		t.Errorf("mid-interval: %v", got)
	}
	// Overdue (latest 3h ago) → clamps to now+1s, not the past.
	overdue := &storage.BandwidthSample{Ts: now.Add(-3 * time.Hour).UnixMilli()}
	if got := NextIntervalRun(overdue, 60, now); !got.Equal(now.Add(time.Second)) {
		t.Errorf("overdue: %v", got)
	}
}
