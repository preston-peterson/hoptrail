package rdns

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/preston-peterson/hoptrail/internal/storage"
)

// rdnsTestStore opens a fresh store and seeds it with sample rows
// for the given IPs. Returns the store and the IPs (so tests can
// assert against them).
func rdnsTestStore(t *testing.T, ips ...string) *storage.Store {
	t.Helper()
	dir := t.TempDir()
	store, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	now := time.Now().UnixMilli()
	for i, ip := range ips {
		_, err := store.DB().Exec(
			`INSERT INTO samples (target, ttl, ts, ip, rtt_us) VALUES (?, ?, ?, ?, ?)`,
			"8.8.8.8", i+1, now, ip, 12345,
		)
		if err != nil {
			t.Fatalf("seed sample %s: %v", ip, err)
		}
	}
	return store
}

// fakeLookup returns a controllable LookupFunc for tests. The map
// drives behavior: an entry with a non-empty hostname returns it;
// an entry with empty string returns ("", nil) — the "no PTR" case;
// an absent IP returns ("", errors.New("synthetic")) — the "lookup
// errored" case.
//
// The atomic counter lets tests verify how many times the lookup
// was called (e.g., "did the worker stop after the batch limit?").
func fakeLookup(answers map[string]string) (LookupFunc, *atomic.Int32) {
	var calls atomic.Int32
	fn := func(_ context.Context, ip string) (string, error) {
		calls.Add(1)
		name, ok := answers[ip]
		if !ok {
			return "", errors.New("synthetic lookup error")
		}
		return name, nil
	}
	return fn, &calls
}

// testConfig returns a Config tuned for fast tests: poll every few
// ms, no inter-lookup delay, generous timeout (delay matters more
// than timeout when the lookup is in-memory).
func testConfig() Config {
	return Config{
		PollInterval:     10 * time.Millisecond,
		LookupTimeout:    1 * time.Second,
		InterLookupDelay: 0,
		BatchSize:        50,
	}
}

func TestResolver_PopulatesHostnamesFromLookup(t *testing.T) {
	store := rdnsTestStore(t, "8.8.8.8", "1.1.1.1", "203.0.113.1")
	lookup, calls := fakeLookup(map[string]string{
		"8.8.8.8":  "dns.google",
		"1.1.1.1":  "one.one.one.one",
		"203.0.113.1": "", // no PTR
	})

	r := New(testConfig(), store, lookup, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	r.Run(ctx) // blocks until ctx times out

	if calls.Load() < 3 {
		t.Errorf("expected at least 3 lookups, got %d", calls.Load())
	}

	// All three IPs should now have rdns rows. Two with hostnames,
	// one with NULL.
	names, err := store.LookupHostnames(context.Background(), []string{
		"8.8.8.8", "1.1.1.1", "203.0.113.1",
	})
	if err != nil {
		t.Fatalf("LookupHostnames: %v", err)
	}
	if names["8.8.8.8"] != "dns.google" {
		t.Errorf("8.8.8.8 hostname = %q, want dns.google", names["8.8.8.8"])
	}
	if names["1.1.1.1"] != "one.one.one.one" {
		t.Errorf("1.1.1.1 hostname = %q, want one.one.one.one", names["1.1.1.1"])
	}
	if _, ok := names["203.0.113.1"]; ok {
		t.Errorf("203.0.113.1 should be absent (NULL hostname); got %q", names["203.0.113.1"])
	}

	// And after that batch, UnresolvedIPs should return nothing —
	// the NULL row counts as "attempted" so it's not re-queried.
	remaining, _, err := store.UnresolvedIPs(context.Background(), 0, 50)
	if err != nil {
		t.Fatalf("UnresolvedIPs: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("expected no remaining unresolved IPs, got %v", remaining)
	}
}

func TestResolver_LookupErrorRecordsNULLRow(t *testing.T) {
	store := rdnsTestStore(t, "192.0.2.1")
	// Empty answers map → fakeLookup returns synthetic error for any IP.
	lookup, _ := fakeLookup(map[string]string{})

	r := New(testConfig(), store, lookup, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	r.Run(ctx)

	// IP should be absent from LookupHostnames (NULL row).
	names, _ := store.LookupHostnames(context.Background(), []string{"192.0.2.1"})
	if _, ok := names["192.0.2.1"]; ok {
		t.Errorf("expected NULL row for failed lookup, got hostname %q", names["192.0.2.1"])
	}

	// And it should NOT appear in UnresolvedIPs — failed lookups are
	// recorded so we don't keep retrying.
	remaining, _, _ := store.UnresolvedIPs(context.Background(), 0, 50)
	for _, ip := range remaining {
		if ip == "192.0.2.1" {
			t.Errorf("failed lookup left IP in unresolved list — would cause retry loop")
		}
	}
}

func TestResolver_RespectsBatchSize(t *testing.T) {
	// Seed 10 IPs, batch size of 3 — first poll should only resolve 3.
	ips := []string{
		"203.0.113.1", "203.0.113.2", "203.0.113.3", "203.0.113.4", "203.0.113.5",
		"203.0.113.6", "203.0.113.7", "203.0.113.8", "203.0.113.9", "203.0.113.10",
	}
	store := rdnsTestStore(t, ips...)

	answers := map[string]string{}
	for _, ip := range ips {
		answers[ip] = "host-" + ip
	}
	lookup, _ := fakeLookup(answers)

	cfg := testConfig()
	cfg.BatchSize = 3
	cfg.PollInterval = 1 * time.Hour // long, so we only get one poll

	r := New(cfg, store, lookup, nil)

	// Run runOnce directly to bypass the initial PollInterval wait —
	// this is the cleanest way to test "exactly one batch."
	r.runOnce(context.Background())

	// Exactly 3 IPs should have rdns rows; the other 7 should still
	// be unresolved.
	remaining, _, err := store.UnresolvedIPs(context.Background(), 0, 50)
	if err != nil {
		t.Fatalf("UnresolvedIPs: %v", err)
	}
	if len(remaining) != 7 {
		t.Errorf("expected 7 IPs still unresolved after batch of 3, got %d", len(remaining))
	}
}

func TestResolver_StopsOnContextCancel(t *testing.T) {
	store := rdnsTestStore(t, "8.8.8.8")
	lookup, _ := fakeLookup(map[string]string{"8.8.8.8": "dns.google"})

	cfg := testConfig()
	cfg.PollInterval = 100 * time.Millisecond
	r := New(cfg, store, lookup, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
		// Run returned; good.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not return within 500ms of ctx cancel")
	}
}

func TestResolver_SystemResolverStripsTrailingDot(t *testing.T) {
	// Can't easily mock net.DefaultResolver, but we can directly test
	// the SystemResolver function's trailing-dot handling by giving
	// it a localhost lookup that should succeed.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	name, err := SystemResolver(ctx, "127.0.0.1")
	if err != nil {
		t.Skipf("SystemResolver returned error (likely no resolver in test env): %v", err)
	}
	// If we got a name back, it must not end with a dot.
	if name != "" && name[len(name)-1] == '.' {
		t.Errorf("SystemResolver returned name with trailing dot: %q", name)
	}
}

// Pins the 2026-08-14 watermark fix: after a clean cycle, subsequent
// polls must not re-scan (or re-attempt) already-covered rows — only
// rows appended since. The unbounded form of this scan (full
// anti-join every 60s) is the prime suspect for the checkpoint
// quiesce stalling at 7/8 connections.
func TestResolver_WatermarkScansOnlyNewRows(t *testing.T) {
	store := rdnsTestStore(t, "203.0.113.1", "203.0.113.2")
	lookup, calls := fakeLookup(map[string]string{
		"203.0.113.1": "one.example",
		"203.0.113.2": "two.example",
		"203.0.113.3": "three.example",
	})
	r := New(testConfig(), store, lookup, nil)
	ctx := context.Background()

	// Cycle 1: full scan (watermark 0) resolves both.
	r.runOnce(ctx)
	if got := calls.Load(); got != 2 {
		t.Fatalf("cycle 1 lookups = %d, want 2", got)
	}
	if r.watermark == 0 {
		t.Fatal("watermark did not advance after a clean exhaustive cycle")
	}

	// Cycle 2: nothing new — no lookups, and critically the scan range
	// is empty (rowid > watermark), not a re-scan that finds nothing.
	r.runOnce(ctx)
	if got := calls.Load(); got != 2 {
		t.Fatalf("cycle 2 lookups = %d, want still 2", got)
	}

	// A row with an ALREADY-RESOLVED ip appends: still no lookup.
	if _, err := store.DB().Exec(
		`INSERT INTO samples (target, ttl, ts, ip, rtt_us) VALUES (?, ?, ?, ?, ?)`,
		"8.8.8.8", 3, time.Now().UnixMilli(), "203.0.113.1", 100); err != nil {
		t.Fatal(err)
	}
	r.runOnce(ctx)
	if got := calls.Load(); got != 2 {
		t.Fatalf("post-resolved-append lookups = %d, want still 2", got)
	}

	// A row with a NEW ip appends: exactly one more lookup.
	if _, err := store.DB().Exec(
		`INSERT INTO samples (target, ttl, ts, ip, rtt_us) VALUES (?, ?, ?, ?, ?)`,
		"8.8.8.8", 4, time.Now().UnixMilli(), "203.0.113.3", 100); err != nil {
		t.Fatal(err)
	}
	r.runOnce(ctx)
	if got := calls.Load(); got != 3 {
		t.Fatalf("post-new-ip lookups = %d, want 3", got)
	}
	names, err := store.LookupHostnames(ctx, []string{"203.0.113.3"})
	if err != nil || names["203.0.113.3"] != "three.example" {
		t.Fatalf("new ip not resolved: %v %v", names, err)
	}
}

// A limit-clipped batch must NOT advance the watermark — the clipped
// range still holds unresolved IPs, and re-scanning converges because
// resolved ones drop out of the anti-join.
func TestResolver_WatermarkHoldsWhenBatchClipped(t *testing.T) {
	store := rdnsTestStore(t, "203.0.113.1", "203.0.113.2", "203.0.113.3")
	lookup, calls := fakeLookup(map[string]string{
		"203.0.113.1": "one.example",
		"203.0.113.2": "two.example",
		"203.0.113.3": "three.example",
	})
	cfg := testConfig()
	cfg.BatchSize = 2 // three unresolved IPs → first cycle clips
	r := New(cfg, store, lookup, nil)
	ctx := context.Background()

	r.runOnce(ctx)
	if r.watermark != 0 {
		t.Fatalf("watermark advanced (%d) on a clipped batch", r.watermark)
	}
	r.runOnce(ctx)
	if r.watermark == 0 {
		t.Fatal("watermark did not advance once the range was exhausted")
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("total lookups = %d, want 3 (no re-attempts)", got)
	}
	var n int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM rdns`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("rdns rows = %d (%v), want 3", n, err)
	}
}
