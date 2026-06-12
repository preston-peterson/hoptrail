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
	store := rdnsTestStore(t, "8.8.8.8", "1.1.1.1", "10.0.0.1")
	lookup, calls := fakeLookup(map[string]string{
		"8.8.8.8":  "dns.google",
		"1.1.1.1":  "one.one.one.one",
		"10.0.0.1": "", // no PTR
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
		"8.8.8.8", "1.1.1.1", "10.0.0.1",
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
	if _, ok := names["10.0.0.1"]; ok {
		t.Errorf("10.0.0.1 should be absent (NULL hostname); got %q", names["10.0.0.1"])
	}

	// And after that batch, UnresolvedIPs should return nothing —
	// the NULL row counts as "attempted" so it's not re-queried.
	remaining, err := store.UnresolvedIPs(context.Background(), 50)
	if err != nil {
		t.Fatalf("UnresolvedIPs: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("expected no remaining unresolved IPs, got %v", remaining)
	}
}

func TestResolver_LookupErrorRecordsNULLRow(t *testing.T) {
	store := rdnsTestStore(t, "192.168.1.1")
	// Empty answers map → fakeLookup returns synthetic error for any IP.
	lookup, _ := fakeLookup(map[string]string{})

	r := New(testConfig(), store, lookup, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	r.Run(ctx)

	// IP should be absent from LookupHostnames (NULL row).
	names, _ := store.LookupHostnames(context.Background(), []string{"192.168.1.1"})
	if _, ok := names["192.168.1.1"]; ok {
		t.Errorf("expected NULL row for failed lookup, got hostname %q", names["192.168.1.1"])
	}

	// And it should NOT appear in UnresolvedIPs — failed lookups are
	// recorded so we don't keep retrying.
	remaining, _ := store.UnresolvedIPs(context.Background(), 50)
	for _, ip := range remaining {
		if ip == "192.168.1.1" {
			t.Errorf("failed lookup left IP in unresolved list — would cause retry loop")
		}
	}
}

func TestResolver_RespectsBatchSize(t *testing.T) {
	// Seed 10 IPs, batch size of 3 — first poll should only resolve 3.
	ips := []string{
		"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5",
		"10.0.0.6", "10.0.0.7", "10.0.0.8", "10.0.0.9", "10.0.0.10",
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
	remaining, err := store.UnresolvedIPs(context.Background(), 50)
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
