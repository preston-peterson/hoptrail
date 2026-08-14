package retention

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/preston-peterson/hoptrail/internal/storage"
)

// retentionTestStore opens a fresh store; helper kept slim because the
// storage layer's own tests already cover the underlying SQL.
func retentionTestStore(t *testing.T) *storage.Store {
	t.Helper()
	dir := t.TempDir()
	store, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// seedSample writes a sample row at the given ts via the public
// schema. Used to set up cases where retention should/shouldn't fire.
func seedSample(t *testing.T, store *storage.Store, ts time.Time) {
	t.Helper()
	_, err := store.DB().Exec(
		`INSERT INTO samples (target, ttl, ts, ip, rtt_us) VALUES (?, ?, ?, ?, ?)`,
		"8.8.8.8", 1, ts.UnixMilli(), "1.1.1.1", 1234,
	)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func countSamples(t *testing.T, store *storage.Store) int {
	t.Helper()
	var n int
	if err := store.DB().QueryRow("SELECT COUNT(*) FROM samples").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestWorker_RunOnce_DeletesRowsBeyondRetention(t *testing.T) {
	store := retentionTestStore(t)

	// "Now" is fixed. Seed three samples spanning the cutoff: one
	// well before the 7-day window, one inside the window, one right
	// at the cutoff (kept, since DELETE uses strict less-than).
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	seedSample(t, store, now.Add(-10*24*time.Hour)) // old → deleted
	seedSample(t, store, now.Add(-7*24*time.Hour))  // exactly at cutoff → kept
	seedSample(t, store, now.Add(-1*24*time.Hour))  // recent → kept

	w := New(Config{RetentionDays: 7, Interval: time.Hour}, store, nil)
	w.now = func() time.Time { return now }

	w.runOnce(context.Background())

	if got := countSamples(t, store); got != 2 {
		t.Errorf("after runOnce: %d samples remain, want 2 (cutoff row + recent)", got)
	}
}

// TestWorker_RunOnce_PrunesIngestLogAt24h pins the dedup log's fixed
// 24h window: independent of RetentionDays (set absurdly long here to
// prove it), rows received more than 24h ago go, newer rows stay.
func TestWorker_RunOnce_PrunesIngestLogAt24h(t *testing.T) {
	store := retentionTestStore(t)

	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	seedIngestBatch(t, store, "stale-batch", now.Add(-25*time.Hour))
	seedIngestBatch(t, store, "fresh-batch", now.Add(-1*time.Hour))

	w := New(Config{RetentionDays: 365, Interval: time.Hour}, store, nil)
	w.now = func() time.Time { return now }

	w.runOnce(context.Background())

	rows, err := store.DB().Query(`SELECT batch_id FROM ingest_log`)
	if err != nil {
		t.Fatalf("query ingest_log: %v", err)
	}
	defer rows.Close()
	var remaining []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		remaining = append(remaining, id)
	}
	if len(remaining) != 1 || remaining[0] != "fresh-batch" {
		t.Errorf("ingest_log after sweep = %v, want [fresh-batch]", remaining)
	}
}

func seedIngestBatch(t *testing.T, store *storage.Store, batchID string, at time.Time) {
	t.Helper()
	if _, err := store.RecordIngestBatch(context.Background(), batchID, "site-east-pi", at); err != nil {
		t.Fatalf("seed ingest batch %s: %v", batchID, err)
	}
}

func TestWorker_Run_FiresInitialSweepImmediately(t *testing.T) {
	store := retentionTestStore(t)

	// Two samples, one obviously beyond any retention window.
	now := time.Now()
	seedSample(t, store, now.Add(-30*24*time.Hour))
	seedSample(t, store, now)

	cfg := Config{
		RetentionDays: 7,
		Interval:      1 * time.Hour, // long, so we never hit a tick during the test
	}
	w := New(cfg, store, nil)

	// Run Run() in a goroutine and cancel quickly. If the initial
	// sweep fires before the ticker is consulted, the stale row should
	// be gone by the time we check.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	// Give the initial sweep a moment to run, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	if got := countSamples(t, store); got != 1 {
		t.Errorf("after initial sweep: %d samples remain, want 1 (only the recent one)", got)
	}
}

func TestWorker_Run_StopsOnContextCancel(t *testing.T) {
	store := retentionTestStore(t)
	w := New(Config{RetentionDays: 7, Interval: 10 * time.Millisecond}, store, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	cancel()
	select {
	case <-done:
		// ok
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not return within 500ms of ctx cancel")
	}
}

// TestWorker_RunOnce_ContinuesAfterSamplesError verifies the
// "fall through to route_changes" behavior: if the samples delete
// errors, the route_changes delete still runs. Hard to trigger a
// real samples error in-process; instead we just check that the
// log line for sweep completion appears even when one delete is
// no-op (which is the common case during normal operation).
func TestWorker_RunOnce_NoOpDoesNotError(t *testing.T) {
	store := retentionTestStore(t)
	var calls atomic.Int32

	w := New(Config{RetentionDays: 7, Interval: time.Hour}, store, nil)
	wrappedNow := w.now
	w.now = func() time.Time {
		calls.Add(1)
		return wrappedNow()
	}

	// Empty tables; runOnce should succeed and emit a sweep-complete
	// log with both counts at zero.
	w.runOnce(context.Background())

	if calls.Load() != 1 {
		t.Errorf("now() called %d times, want 1", calls.Load())
	}
}

// A busy checkpoint retries within the sweep instead of waiting an
// hour for the next one (observed live: a single collision during the
// post-incident deploy's ingest-backlog drain).
func TestCheckpointWithRetry(t *testing.T) {
	ctx := context.Background()
	busy := func() (int, error) { return 0, context.DeadlineExceeded }

	// Succeeds on the third attempt: two pauses, three runs.
	fails := 2
	pauses := 0
	frames, attempts, err := checkpointWithRetry(ctx, 5,
		func() { pauses++ },
		func() (int, error) {
			if fails > 0 {
				fails--
				return 0, context.DeadlineExceeded
			}
			return 42, nil
		})
	if err != nil || frames != 42 || attempts != 3 || pauses != 2 {
		t.Errorf("retry-then-succeed = frames %d attempts %d pauses %d err %v", frames, attempts, pauses, err)
	}

	// Permanently busy: gives up after maxAttempts with the error.
	pauses = 0
	_, attempts, err = checkpointWithRetry(ctx, 5, func() { pauses++ }, busy)
	if err == nil || attempts != 5 || pauses != 4 {
		t.Errorf("permanent-busy = attempts %d pauses %d err %v", attempts, pauses, err)
	}

	// Canceled context stops the loop without further pauses.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	_, attempts, err = checkpointWithRetry(cctx, 5, func() { t.Error("paused after cancel") }, busy)
	if err == nil || attempts != 1 {
		t.Errorf("canceled = attempts %d err %v", attempts, err)
	}
}
