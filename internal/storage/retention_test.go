package storage

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// seedSampleAt inserts a single sample with the given ts. Used to set
// up precise time-based scenarios for the retention tests.
func seedSampleAt(t *testing.T, store *Store, ts time.Time) {
	t.Helper()
	_, err := store.db.Exec(
		`INSERT INTO samples (target, ttl, ts, ip, rtt_us) VALUES (?, ?, ?, ?, ?)`,
		"8.8.8.8", 1, ts.UnixMilli(), "1.1.1.1", 12345,
	)
	if err != nil {
		t.Fatalf("seed sample at %v: %v", ts, err)
	}
}

func seedRouteChangeAt(t *testing.T, store *Store, ts time.Time) {
	t.Helper()
	_, err := store.db.Exec(
		`INSERT INTO route_changes (target, ttl, ts, old_ip, new_ip) VALUES (?, ?, ?, ?, ?)`,
		"8.8.8.8", 1, ts.UnixMilli(), "1.1.1.1", "2.2.2.2",
	)
	if err != nil {
		t.Fatalf("seed route_change at %v: %v", ts, err)
	}
}

func countRows(t *testing.T, store *Store, table string) int {
	t.Helper()
	var n int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestDeleteSamplesOlderThan_DeletesOnlyOlderRows(t *testing.T) {
	store := tempStore(t)
	now := time.Now()

	// Three samples: one well before cutoff, one right at cutoff, one after.
	seedSampleAt(t, store, now.Add(-48*time.Hour)) // old
	seedSampleAt(t, store, now.Add(-24*time.Hour)) // cutoff boundary
	seedSampleAt(t, store, now.Add(-1*time.Hour))  // recent

	cutoff := now.Add(-24 * time.Hour)
	deleted, err := store.DeleteSamplesOlderThan(context.Background(), cutoff)
	if err != nil {
		t.Fatalf("DeleteSamplesOlderThan: %v", err)
	}

	// Strictly less than the cutoff means the -48h row is gone and the
	// -24h row is kept. Document this is the intent: equality with the
	// cutoff is treated as "within retention."
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}
	if got := countRows(t, store, "samples"); got != 2 {
		t.Errorf("samples remaining = %d, want 2 (cutoff row + recent row)", got)
	}
}

func TestDeleteSamplesOlderThan_EmptyTableNoError(t *testing.T) {
	store := tempStore(t)
	deleted, err := store.DeleteSamplesOlderThan(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("DeleteSamplesOlderThan on empty table: %v", err)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0", deleted)
	}
}

func TestDeleteRouteChangesOlderThan_DeletesOnlyOlderRows(t *testing.T) {
	store := tempStore(t)
	now := time.Now()

	seedRouteChangeAt(t, store, now.Add(-10*24*time.Hour))
	seedRouteChangeAt(t, store, now.Add(-2*time.Hour))

	cutoff := now.Add(-7 * 24 * time.Hour)
	deleted, err := store.DeleteRouteChangesOlderThan(context.Background(), cutoff)
	if err != nil {
		t.Fatalf("DeleteRouteChangesOlderThan: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}
	if got := countRows(t, store, "route_changes"); got != 1 {
		t.Errorf("route_changes remaining = %d, want 1", got)
	}
}

// Sanity: retention must not touch the rdns table. That's a cache bounded
// by unique IPs, not time-series; deleting rdns rows would force
// re-resolution of the same IPs and defeat the cache.
func TestRetention_DoesNotTouchRDNS(t *testing.T) {
	store := tempStore(t)
	ctx := context.Background()

	// Seed an rdns row. Note that UpsertRDNS writes resolved_at as
	// "now," which is well within any sane retention window.
	if err := store.UpsertRDNS(ctx, "8.8.8.8", "dns.google"); err != nil {
		t.Fatalf("UpsertRDNS: %v", err)
	}

	// Even running retention with a cutoff in the future (== "delete
	// everything older than tomorrow") must not affect rdns.
	if _, err := store.DeleteSamplesOlderThan(ctx, time.Now().Add(24*time.Hour)); err != nil {
		t.Fatalf("DeleteSamples: %v", err)
	}
	if _, err := store.DeleteRouteChangesOlderThan(ctx, time.Now().Add(24*time.Hour)); err != nil {
		t.Fatalf("DeleteRouteChanges: %v", err)
	}

	names, err := store.LookupHostnames(ctx, []string{"8.8.8.8"})
	if err != nil {
		t.Fatalf("LookupHostnames after retention: %v", err)
	}
	if names["8.8.8.8"] != "dns.google" {
		t.Errorf("rdns row missing after retention; cache must not be cleared")
	}
}

// Pins the 2026-08 batching fix: a sweep larger than one batch still
// deletes everything (the loop continues until a short batch) and
// leaves newer rows alone.
func TestDeleteSamplesOlderThan_Batched(t *testing.T) {
	store := tempStore(t)
	ctx := context.Background()

	orig := deleteBatchRows
	deleteBatchRows = 10
	t.Cleanup(func() { deleteBatchRows = orig })

	cutoff := time.Now()
	for i := 0; i < 25; i++ { // 2.5 batches worth of expired rows
		seedSampleAt(t, store, cutoff.Add(-time.Hour))
	}
	for i := 0; i < 5; i++ {
		seedSampleAt(t, store, cutoff.Add(time.Hour))
	}

	deleted, err := store.DeleteSamplesOlderThan(ctx, cutoff)
	if err != nil {
		t.Fatalf("DeleteSamplesOlderThan: %v", err)
	}
	if deleted != 25 {
		t.Errorf("deleted = %d, want 25", deleted)
	}
	if got := countRows(t, store, "samples"); got != 5 {
		t.Errorf("samples remaining = %d, want 5", got)
	}
}

// Pins the 2026-08 WAL fix: CheckpointWAL(TRUNCATE) folds the log into
// the main file and truncates the -wal file to zero bytes. (Passive
// autocheckpoints never guarantee this; the incident WAL reached
// 12.5 GiB because a reset moment never arrived under read load.)
func TestCheckpointWAL_TruncatesTheLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ckpt.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()

	for i := 0; i < 500; i++ {
		seedSampleAt(t, store, time.Now())
	}
	before, err := os.Stat(path + "-wal")
	if err != nil || before.Size() == 0 {
		t.Fatalf("expected a non-empty WAL before checkpoint (size err %v)", err)
	}

	// Note: the returned frame count may be 0 when a passive
	// autocheckpoint already back-copied the frames — the truncation
	// is the property that matters, so that's what gets asserted.
	if _, err := store.CheckpointWAL(ctx); err != nil {
		t.Fatalf("CheckpointWAL: %v", err)
	}
	after, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatalf("stat wal after: %v", err)
	}
	if after.Size() != 0 {
		t.Errorf("wal size after TRUNCATE = %d bytes, want 0", after.Size())
	}
}

// The live failure mode (2026-08 deploy): overlapping readers keep the
// WAL read-mark slots shared-locked with near-100% duty cycle, and
// SQLite's exclusive acquisition has no fairness — the truncate
// starves forever no matter the busy timeout. CheckpointWAL quiesces
// the pool to manufacture its zero-reader moment; this pins that it
// wins under a reader storm and restores the pool afterwards.
func TestCheckpointWAL_WinsUnderReadStorm(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "storm.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()

	for i := 0; i < 2000; i++ {
		seedSampleAt(t, store, time.Now())
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				var n int
				_ = store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM samples").Scan(&n)
			}
		}()
	}
	time.Sleep(100 * time.Millisecond) // let the storm establish

	if _, err := store.CheckpointWAL(ctx); err != nil {
		close(stop)
		wg.Wait()
		t.Fatalf("CheckpointWAL under read storm: %v", err)
	}
	close(stop)
	wg.Wait()

	if got := store.db.Stats().MaxOpenConnections; got != 8 {
		t.Errorf("pool after checkpoint = %d conns, want 8 restored", got)
	}
	fi, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatalf("stat wal: %v", err)
	}
	if fi.Size() != 0 {
		t.Errorf("wal after storm-checkpoint = %d bytes, want 0", fi.Size())
	}
}
