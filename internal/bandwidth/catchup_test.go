package bandwidth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/preston-peterson/hoptrail/internal/storage"
)

// Pins the step-155 catch-up: a missed past slot with no sample after
// it schedules a prompt run; a covered slot defers to the next
// occurrence as before.
func TestPrevScheduled_AndCatchupLogic(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 6, 12, 8, 0, 0, 0, loc) // 08:00; daily slot 02:00

	prev, ok := PrevScheduled(now, []string{"02:00"}, loc)
	if !ok || !prev.Equal(time.Date(2026, 6, 12, 2, 0, 0, 0, loc)) {
		t.Fatalf("PrevScheduled = %v %v, want today 02:00", prev, ok)
	}

	// Before the day's slot: yesterday's occurrence.
	early := time.Date(2026, 6, 12, 1, 0, 0, 0, loc)
	prev, ok = PrevScheduled(early, []string{"02:00"}, loc)
	if !ok || !prev.Equal(time.Date(2026, 6, 11, 2, 0, 0, 0, loc)) {
		t.Fatalf("PrevScheduled early = %v %v, want yesterday 02:00", prev, ok)
	}

	// Sample-vs-slot comparison semantics (the nextRun guard):
	missed := &storage.BandwidthSample{Ts: time.Date(2026, 6, 11, 14, 0, 0, 0, loc).UnixMilli()}
	covered := &storage.BandwidthSample{Ts: time.Date(2026, 6, 12, 2, 0, 30, 0, loc).UnixMilli()}
	slot := time.Date(2026, 6, 12, 2, 0, 0, 0, loc)
	if !time.UnixMilli(missed.Ts).Before(slot) {
		t.Error("yesterday-afternoon sample should count as a missed slot")
	}
	if time.UnixMilli(covered.Ts).Before(slot) {
		t.Error("a sample just after the slot must NOT trigger catch-up")
	}
}

// Pins the 2026-08 runaway fix: catch-up fires at most once per slot
// per process. The stored-sample marker alone cannot be trusted — when
// the store itself is failing (disk full), no row ever advances it,
// and pre-fix the scheduler re-armed a full-bandwidth test every 5s.
func TestCatchup_AtMostOncePerSlot(t *testing.T) {
	store := testStore(t)
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.ScheduledTimes = []string{"02:00"}
	cfg.Timezone = "UTC"
	r := NewRunner(store, cfg, nil, nil, nil)
	now := time.Date(2026, 8, 13, 8, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	ctx := context.Background()

	// Fresh process, no sample covers today's 02:00 → prompt catch-up
	// (the step-155 power-outage behavior, unchanged).
	next, ok := r.nextRun(ctx, cfg)
	if !ok || !next.Equal(now.Add(catchupDelay)) {
		t.Fatalf("first nextRun = %v %v, want prompt catch-up", next, ok)
	}

	// A test was ATTEMPTED after the slot — outcome unknown, store may
	// have swallowed the row. Catch-up must NOT re-arm; the schedule
	// moves on to tomorrow's slot.
	r.mu.Lock()
	r.lastAttempt = time.Date(2026, 8, 13, 7, 0, 0, 0, time.UTC)
	r.mu.Unlock()
	next, ok = r.nextRun(ctx, cfg)
	want := time.Date(2026, 8, 14, 2, 0, 0, 0, time.UTC)
	if !ok || !next.Equal(want) {
		t.Fatalf("post-attempt nextRun = %v %v, want tomorrow 02:00", next, ok)
	}

	// An attempt that predates the slot does not cover it — catch-up
	// still fires for a slot missed since that attempt.
	r.mu.Lock()
	r.lastAttempt = time.Date(2026, 8, 12, 23, 0, 0, 0, time.UTC)
	r.mu.Unlock()
	next, ok = r.nextRun(ctx, cfg)
	if !ok || !next.Equal(now.Add(catchupDelay)) {
		t.Fatalf("pre-slot-attempt nextRun = %v %v, want prompt catch-up", next, ok)
	}
}

// Incident replay (2026-08): reads fine, every WRITE fails (the
// disk-full condition), the catch-up attempt's test ALSO fails — and
// the schedule must still move on to tomorrow's slot instead of
// re-arming a 5-second retry forever.
func TestExecute_MarksAttemptEvenWhenStoreFails(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.ScheduledTimes = []string{"02:00"}
	cfg.Timezone = "UTC"
	canned := func(ctx context.Context, args ...string) ([]byte, error) {
		return nil, errors.New("speedtest: Limit reached: Too many requests received")
	}
	r := NewRunner(store, cfg, canned, nil, nil)
	now := time.Date(2026, 8, 13, 8, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }

	// Make every write fail while reads keep working: pin the pool to
	// one connection and flip it read-only.
	db := store.DB()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.ExecContext(ctx, "PRAGMA query_only = ON"); err != nil {
		t.Fatalf("query_only: %v", err)
	}
	if err := store.SetConfig(ctx, "bandwidth.sanity", "x"); err == nil {
		t.Fatal("store writes still succeed; simulation broken")
	}

	// The catch-up attempt: test fails AND the failure row cannot be
	// stored — pre-fix this left no trace and the loop re-armed.
	r.execute(ctx, cfg)

	r.mu.Lock()
	got := r.lastAttempt
	r.mu.Unlock()
	if !got.Equal(now) {
		t.Fatalf("lastAttempt = %v, want %v (must survive store failure)", got, now)
	}
	next, ok := r.nextRun(ctx, cfg)
	want := time.Date(2026, 8, 14, 2, 0, 0, 0, time.UTC)
	if !ok || !next.Equal(want) {
		t.Fatalf("nextRun after failed attempt = %v %v, want tomorrow 02:00 (NOT a prompt retry)", next, ok)
	}
}
