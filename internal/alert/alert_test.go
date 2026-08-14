package alert

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/preston-peterson/hoptrail/internal/storage"
)

func testStore(t *testing.T) *storage.Store {
	t.Helper()
	s, err := storage.Open(filepath.Join(t.TempDir(), "alert.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func drainQueue(t *testing.T, s *storage.Store) []storage.AlertQueueItem {
	t.Helper()
	ctx := context.Background()
	out := []storage.AlertQueueItem{}
	for {
		item, err := s.NextQueuedAlert(ctx)
		if err != nil {
			t.Fatalf("NextQueuedAlert: %v", err)
		}
		if item == nil {
			return out
		}
		out = append(out, *item)
		if err := s.DeleteQueuedAlert(ctx, item.ID); err != nil {
			t.Fatalf("DeleteQueuedAlert: %v", err)
		}
	}
}

// engineAt builds an engine with a controllable clock and a
// probe-offline provider driven by the test.
func engineAt(t *testing.T, s *storage.Store, cfg Config, lastSeen *time.Time) (*Engine, *time.Time) {
	t.Helper()
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.Local)
	e := NewEngine(s, cfg, Providers{
		Probes: func(ctx context.Context) (map[string]time.Time, error) {
			return map[string]time.Time{"site-east": *lastSeen}, nil
		},
		Targets: func() map[string]Thresholds { return nil },
	}, nil)
	e.now = func() time.Time { return now }
	// Return a pointer the test mutates; capture via closure.
	np := &now
	e.now = func() time.Time { return *np }
	return e, np
}

func TestEngine_ProbeOfflineRaiseAndRecover(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.Enabled, cfg.ServerURL, cfg.Topic = true, "http://127.0.0.1:1", "t"

	lastSeen := time.Date(2026, 6, 11, 11, 59, 0, 0, time.Local)
	e, now := engineAt(t, s, cfg, &lastSeen)

	// Healthy heartbeat: nothing.
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if q := drainQueue(t, s); len(q) != 0 {
		t.Fatalf("healthy probe queued %v", q)
	}

	// Heartbeat goes stale (>180s): probe_offline has no extra sustain
	// — staleness IS the debounce — so it fires on the next tick.
	*now = now.Add(10 * time.Minute)
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	q := drainQueue(t, s)
	if len(q) != 1 || !strings.Contains(q[0].Body, "site-east offline") || q[0].Priority != "high" {
		t.Fatalf("offline alert = %+v", q)
	}

	// Still down: no duplicate.
	*now = now.Add(time.Minute)
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if q := drainQueue(t, s); len(q) != 0 {
		t.Fatalf("duplicate alert %v", q)
	}

	// Heartbeat returns: paired recovery.
	lastSeen = *now
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	q = drainQueue(t, s)
	if len(q) != 1 || !strings.Contains(q[0].Body, "recovered") {
		t.Fatalf("recovery = %+v", q)
	}
	states, _ := s.ListAlertStates(ctx)
	if len(states) != 0 {
		t.Fatalf("state rows remain: %+v", states)
	}

	// Step-149: the append-only history recorded both halves.
	hist, err := s.ListAlertHistory(ctx, 50)
	if err != nil {
		t.Fatalf("ListAlertHistory: %v", err)
	}
	if len(hist) != 2 || hist[0].Kind != "recovered" || hist[1].Kind != "alert" {
		t.Fatalf("history = %+v, want recovered-then-alert (newest first)", hist)
	}
	if hist[1].EventType != "probe_offline" || hist[1].Subject != "site-east" {
		t.Errorf("history entry = %+v", hist[1])
	}
}

func TestEngine_CapacityRaiseRecoverAndActiveFlag(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.Enabled, cfg.ServerURL, cfg.Topic = true, "http://127.0.0.1:1", "t"

	var tripped bool
	var lastActive bool // what the engine reported on the most recent call
	e := NewEngine(s, cfg, Providers{
		Capacity: func(_ context.Context, active bool) (CapacityVerdict, error) {
			lastActive = active
			return CapacityVerdict{
				Valid:      true,
				Tripped:    tripped,
				AlertMsg:   "low disk on the central: only 200 MB free",
				RecoverMsg: "disk recovered: 8 GB free",
			}, nil
		},
	}, nil)

	// Healthy: nothing, and the engine reports not-active.
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if q := drainQueue(t, s); len(q) != 0 {
		t.Fatalf("healthy capacity queued %v", q)
	}
	if lastActive {
		t.Fatal("active should be false before any alert")
	}

	// Disk drops below floor: capacity has no extra sustain, so it fires
	// on the next tick at high priority.
	tripped = true
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	q := drainQueue(t, s)
	if len(q) != 1 || !strings.Contains(q[0].Body, "only 200 MB free") || q[0].Priority != "high" {
		t.Fatalf("capacity alert = %+v", q)
	}

	// Still tripped: no duplicate, and now the engine reports active so
	// the provider can apply recovery hysteresis.
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if q := drainQueue(t, s); len(q) != 0 {
		t.Fatalf("duplicate capacity alert %v", q)
	}
	if !lastActive {
		t.Fatal("active should be true while the incident is raised")
	}

	// Recovered: paired recovery message.
	tripped = false
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	q = drainQueue(t, s)
	if len(q) != 1 || !strings.Contains(q[0].Body, "disk recovered") {
		t.Fatalf("capacity recovery = %+v", q)
	}
	states, _ := s.ListAlertStates(ctx)
	if len(states) != 0 {
		t.Fatalf("state rows remain: %+v", states)
	}
}

// TestEngine_CapacityDisabledAndInvalid: the per-event toggle and an
// invalid measurement both leave any state untouched.
func TestEngine_CapacityDisabledAndInvalid(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.Enabled, cfg.ServerURL, cfg.Topic = true, "http://127.0.0.1:1", "t"
	cfg.EventCapacity = false

	called := false
	e := NewEngine(s, cfg, Providers{
		Capacity: func(_ context.Context, _ bool) (CapacityVerdict, error) {
			called = true
			return CapacityVerdict{Valid: true, Tripped: true, AlertMsg: "x", RecoverMsg: "y"}, nil
		},
	}, nil)
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("capacity provider must not be consulted when EventCapacity is off")
	}
	if q := drainQueue(t, s); len(q) != 0 {
		t.Fatalf("queued while disabled: %v", q)
	}

	// Enabled but the measurement is unavailable (Valid=false): no alert.
	cfg.EventCapacity = true
	e.Reconfigure(cfg)
	e2 := NewEngine(s, cfg, Providers{
		Capacity: func(_ context.Context, _ bool) (CapacityVerdict, error) {
			return CapacityVerdict{Valid: false, Tripped: true}, nil
		},
	}, nil)
	if err := e2.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if q := drainQueue(t, s); len(q) != 0 {
		t.Fatalf("queued on invalid measurement: %v", q)
	}
}

func TestEngine_LossSustainAndDisabled(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.Enabled, cfg.ServerURL, cfg.Topic = true, "http://127.0.0.1:1", "t"
	cfg.SustainS = 120

	lossy := true
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.Local)
	e := NewEngine(s, cfg, Providers{
		Targets: func() map[string]Thresholds { return map[string]Thresholds{"1.1.1.1": {}} },
		WindowStats: func(ctx context.Context, target string, since, until time.Time) (storage.TargetWindowStats, error) {
			if lossy {
				return storage.TargetWindowStats{Sent: 100, Received: 40}, nil // 60% loss
			}
			return storage.TargetWindowStats{Sent: 100, Received: 100}, nil
		},
	}, nil)
	np := &now
	e.now = func() time.Time { return *np }

	// First sighting starts the sustain clock — no alert yet.
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if q := drainQueue(t, s); len(q) != 0 {
		t.Fatalf("alert before sustain: %v", q)
	}
	// Still lossy after the sustain window: fires.
	*np = np.Add(3 * time.Minute)
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	q := drainQueue(t, s)
	if len(q) != 1 || !strings.Contains(q[0].Body, "60% loss") {
		t.Fatalf("loss alert = %+v", q)
	}
	// Recovery pairs.
	lossy = false
	*np = np.Add(time.Minute)
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if q := drainQueue(t, s); len(q) != 1 || !strings.Contains(q[0].Body, "reachable again") {
		t.Fatalf("recovery = %+v", q)
	}

	// Master toggle off: even a lossy target queues nothing.
	lossy = true
	cfg.Enabled = false
	e.Reconfigure(cfg)
	*np = np.Add(10 * time.Minute)
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if q := drainQueue(t, s); len(q) != 0 {
		t.Fatalf("disabled engine queued %v", q)
	}
}

func TestQuietWindow(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 6, 11, h, m, 0, 0, time.Local) }
	cases := []struct {
		start, end string
		h, m       int
		want       bool
	}{
		{"22:00", "07:00", 23, 0, true},  // wraps midnight, evening side
		{"22:00", "07:00", 3, 0, true},   // wraps midnight, morning side
		{"22:00", "07:00", 12, 0, false}, // midday
		{"09:00", "17:00", 12, 0, true},  // plain window
		{"09:00", "17:00", 18, 0, false},
		{"", "", 3, 0, false}, // disabled
	}
	for _, c := range cases {
		if got := inQuietWindow(at(c.h, c.m), c.start, c.end); got != c.want {
			t.Errorf("inQuietWindow(%02d:%02d, %s-%s) = %v, want %v", c.h, c.m, c.start, c.end, got, c.want)
		}
	}
}

func TestEngine_QuietHoursSummaryFoldsPairs(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.Enabled, cfg.ServerURL, cfg.Topic = true, "http://127.0.0.1:1", "t"
	cfg.QuietStart, cfg.QuietEnd = "22:00", "07:00"

	lastSeen := time.Date(2026, 6, 11, 23, 0, 0, 0, time.Local)
	e, now := engineAt(t, s, cfg, &lastSeen)
	*now = time.Date(2026, 6, 12, 2, 11, 0, 0, time.Local) // inside quiet hours

	// Offline raises at 02:11 — buffered, not queued.
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if q := drainQueue(t, s); len(q) != 0 {
		t.Fatalf("quiet-hours alert leaked to queue: %v", q)
	}
	// Recovers at 02:54 — also buffered.
	*now = time.Date(2026, 6, 12, 2, 54, 0, 0, time.Local)
	lastSeen = *now
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if q := drainQueue(t, s); len(q) != 0 {
		t.Fatalf("quiet-hours recovery leaked: %v", q)
	}

	// Window ends: ONE summary with the pair folded to one line.
	// (Heartbeat kept fresh — otherwise 02:54→07:05 is stale again
	// and a legitimate new offline alert would fire.)
	*now = time.Date(2026, 6, 12, 7, 5, 0, 0, time.Local)
	lastSeen = *now
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	q := drainQueue(t, s)
	if len(q) != 1 {
		t.Fatalf("summary count = %d (%+v)", len(q), q)
	}
	if !strings.Contains(q[0].Title, "1 alert(s) during quiet hours") {
		t.Errorf("summary title = %q", q[0].Title)
	}
	if !strings.Contains(q[0].Body, "02:11") || !strings.Contains(q[0].Body, "recovered 02:54") {
		t.Errorf("summary body = %q", q[0].Body)
	}
	if strings.Count(q[0].Body, "\n") != 0 {
		t.Errorf("pair did not fold to one line: %q", q[0].Body)
	}
	// Buffer cleared — a later tick must not resend.
	*now = now.Add(time.Minute)
	lastSeen = *now
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if q := drainQueue(t, s); len(q) != 0 {
		t.Fatalf("summary resent: %v", q)
	}
}

func TestSender_RetryThenDeliver_AndPoison(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.EnqueueAlert(ctx, "t1", "b1", "default", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueAlert(ctx, "t2-poison", "b2", "default", time.Now()); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	attempts := map[string]int{}
	post := func(ctx context.Context, cfg Config, item storage.AlertQueueItem) error {
		mu.Lock()
		defer mu.Unlock()
		attempts[item.Title]++
		if item.Title == "t1" && attempts["t1"] < 3 {
			return fmt.Errorf("connection refused") // transient ×2
		}
		if item.Title == "t2-poison" {
			return &PosterPermanentError{msg: "ntfy 400: bad topic"}
		}
		return nil
	}
	cfg := DefaultConfig()
	cfg.ServerURL, cfg.Topic = "http://127.0.0.1:1", "t"
	sender := NewSender(s, func() Config { return cfg }, post, nil)
	go sender.Run(ctx)

	deadline := time.After(40 * time.Second) // backoff 5s+10s before 3rd try
	for {
		depth, err := s.AlertQueueDepth(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if depth == 0 {
			break
		}
		select {
		case <-deadline:
			mu.Lock()
			t.Fatalf("queue never drained; attempts=%v depth=%d", attempts, depth)
			mu.Unlock()
		case <-time.After(100 * time.Millisecond):
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts["t1"] != 3 {
		t.Errorf("t1 attempts = %d, want 3 (retry-retry-deliver)", attempts["t1"])
	}
	if attempts["t2-poison"] != 1 {
		t.Errorf("poison attempts = %d, want 1 (dropped, not retried)", attempts["t2-poison"])
	}
}

func TestSanitizeLatin1(t *testing.T) {
	if got := sanitizeLatin1("loss — 60% über"); got != "loss ? 60% über" {
		t.Errorf("sanitize = %q", got) // em-dash >0xFF replaced; ü (0xFC) kept
	}
}

func TestConfig_ValidateAndRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	bad := []func(*Config){
		func(c *Config) { c.Enabled = true; c.ServerURL = "" },
		func(c *Config) { c.ServerURL = "ftp://x" },
		func(c *Config) { c.Topic = "has space"; c.ServerURL = "http://x" },
		func(c *Config) { c.LossPct = 0 },
		func(c *Config) { c.SustainS = 5 },
		func(c *Config) { c.LatencyLevel = "loud" },
		func(c *Config) { c.QuietStart = "22:00" }, // end missing
		func(c *Config) { c.QuietStart = "25:00"; c.QuietEnd = "07:00" },
	}
	for i, mod := range bad {
		c := DefaultConfig()
		mod(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("bad config %d validated", i)
		}
	}

	c := DefaultConfig()
	c.Enabled, c.ServerURL, c.Topic, c.Token = true, "https://ntfy.example", "hoptrail-x9", "tok"
	c.QuietStart, c.QuietEnd = "22:00", "07:00"
	c.EventLatency = true
	if err := c.Validate(); err != nil {
		t.Fatalf("good config rejected: %v", err)
	}
	if err := SaveConfig(ctx, s, c); err != nil {
		t.Fatal(err)
	}
	got, warnings, err := LoadConfig(ctx, s)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("LoadConfig: %v %v", err, warnings)
	}
	if got != c {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, c)
	}

	// Corrupt row keeps the field default.
	if err := s.SetConfig(ctx, "alert.sustain_s", "garbage"); err != nil {
		t.Fatal(err)
	}
	got, warnings, err = LoadConfig(ctx, s)
	if err != nil || len(warnings) != 1 {
		t.Fatalf("corrupt row: %v %v", err, warnings)
	}
	if got.SustainS != DefaultConfig().SustainS {
		t.Errorf("corrupt sustain_s = %d, want default", got.SustainS)
	}
}

// Pins the 2026-08 bug-4 fix: with the store failing every WRITE (the
// disk-full condition), the alert state row never persists, so each
// tick re-raises the same incident as brand new. The in-memory
// cooldown marker must be recorded at raise-ATTEMPT time — pre-fix it
// was only recorded on delivery success, so nothing bounded the loop
// (240 attempts/hour from one subject vs a 12/hour global budget,
// starving every other alert).
func TestEngine_RaiseLoopBoundedWhenStoreFails(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.Enabled, cfg.ServerURL, cfg.Topic = true, "http://127.0.0.1:1", "t"

	lastSeen := time.Date(2026, 6, 11, 11, 0, 0, 0, time.Local) // stale → offline
	e, now := engineAt(t, s, cfg, &lastSeen)

	// Reads keep working, every write fails — the disk-full analogue
	// (pin the pool to one connection so the pragma sticks).
	db := s.DB()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.ExecContext(ctx, "PRAGMA query_only = ON"); err != nil {
		t.Fatalf("query_only: %v", err)
	}

	// 20 ticks over 5 simulated minutes, all raising probe_offline for
	// the same subject against a dead store.
	for i := 0; i < 20; i++ {
		if err := e.Tick(ctx); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
		*now = now.Add(15 * time.Second)
	}

	// Exactly ONE raise attempt reached the delivery gate; the other 19
	// were suppressed by the attempt-time cooldown marker.
	e.mu.Lock()
	attempts := len(e.sent)
	_, marked := e.lastRaised[EventProbeOffline+"|site-east"]
	e.mu.Unlock()
	if attempts != 1 {
		t.Errorf("delivery attempts = %d, want 1 (raise loop not bounded)", attempts)
	}
	if !marked {
		t.Error("lastRaised marker missing — must be set at attempt time")
	}
}

// Pins the step-195 orphan sweep: a probe that vanishes from the
// registry (operator removed it) must have its active incident
// cleared — with a "cleared" history entry, NOT a recovery
// notification — instead of sitting on the status card forever
// (live case: 40+ days). A provider error must never sweep.
func TestEngine_OrphanedIncidentSwept(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.Enabled, cfg.ServerURL, cfg.Topic = true, "http://127.0.0.1:1", "t"

	present := true
	failing := false
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.Local)
	stale := now.Add(-10 * time.Minute)
	e := NewEngine(s, cfg, Providers{
		Probes: func(ctx context.Context) (map[string]time.Time, error) {
			if failing {
				return nil, errors.New("registry unavailable")
			}
			if present {
				return map[string]time.Time{"ghost": stale}, nil
			}
			return map[string]time.Time{}, nil
		},
		Targets: func() map[string]Thresholds { return nil },
	}, nil)
	e.now = func() time.Time { return now }

	// Raise: stale heartbeat → active incident + one notification.
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if q := drainQueue(t, s); len(q) != 1 {
		t.Fatalf("raise queued %d notifications, want 1", len(q))
	}

	// A transient provider error must NOT sweep.
	failing = true
	if err := e.Tick(ctx); err == nil {
		t.Fatal("tick should surface the provider error")
	}
	failing = false
	if states, _ := s.ListAlertStates(ctx); len(states) != 1 {
		t.Fatalf("state swept on provider error: %+v", states)
	}

	// Probe removed from the registry → swept: state gone, history
	// gets a "cleared" entry, and NOTHING is enqueued for delivery.
	present = false
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if states, _ := s.ListAlertStates(ctx); len(states) != 0 {
		t.Fatalf("state not swept: %+v", states)
	}
	if q := drainQueue(t, s); len(q) != 0 {
		t.Fatalf("sweep enqueued %d notifications, want 0", len(q))
	}
	hist, err := s.ListAlertHistory(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) == 0 || hist[0].Kind != "cleared" || hist[0].Subject != "ghost" {
		t.Fatalf("history head = %+v, want kind=cleared subject=ghost", hist)
	}
}
