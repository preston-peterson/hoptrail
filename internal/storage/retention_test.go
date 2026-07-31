package storage

import (
	"context"
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
