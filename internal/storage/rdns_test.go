package storage

import (
	"context"
	"testing"
	"time"
)

// rdnsTestSetup seeds the samples table with a known set of IPs at
// various TTLs, so the rdns-method tests have realistic data to query
// against. Returns the store; caller owns nothing it needs to close.
//
// Layout written:
//
//	TTL 1, IP 192.168.1.1   (will be left unresolved in some tests)
//	TTL 2, IP 10.0.0.1
//	TTL 3, IP 8.8.8.8
//	TTL 4, IP NULL          (timeout — has no IP, must not appear in unresolved list)
//	TTL 5, IP 8.8.4.4
func rdnsTestSetup(t *testing.T) *Store {
	t.Helper()
	store := tempStore(t)

	now := time.Now().UnixMilli()
	samples := []struct {
		ttl int
		ip  any // nil or string
	}{
		{1, "192.168.1.1"},
		{2, "10.0.0.1"},
		{3, "8.8.8.8"},
		{4, nil}, // timeout row
		{5, "8.8.4.4"},
	}
	for _, s := range samples {
		_, err := store.db.Exec(
			`INSERT INTO samples (target, ttl, ts, ip, rtt_us) VALUES (?, ?, ?, ?, ?)`,
			"8.8.8.8", s.ttl, now, s.ip, 12345,
		)
		if err != nil {
			t.Fatalf("seed sample TTL=%d: %v", s.ttl, err)
		}
	}
	return store
}

func TestUnresolvedIPs_ReturnsDistinctIPsNotInRDNS(t *testing.T) {
	store := rdnsTestSetup(t)

	ips, err := store.UnresolvedIPs(context.Background(), 100)
	if err != nil {
		t.Fatalf("UnresolvedIPs: %v", err)
	}

	// Expect the four IPs (TTL=4 had NULL ip, so it's excluded). Order
	// is not guaranteed; collect into a set for comparison.
	got := make(map[string]bool, len(ips))
	for _, ip := range ips {
		got[ip] = true
	}
	want := map[string]bool{
		"192.168.1.1": true,
		"10.0.0.1":    true,
		"8.8.8.8":     true,
		"8.8.4.4":     true,
	}
	if len(got) != len(want) {
		t.Errorf("unresolved IPs: got %d, want %d (got=%v)", len(got), len(want), got)
	}
	for ip := range want {
		if !got[ip] {
			t.Errorf("expected IP %q missing from unresolved list", ip)
		}
	}
}

func TestUnresolvedIPs_ExcludesAlreadyResolved(t *testing.T) {
	store := rdnsTestSetup(t)
	ctx := context.Background()

	// Resolve two of the four. After this, only the other two should
	// appear in UnresolvedIPs — regardless of whether the recorded
	// hostname was a real name or NULL (state 2 from the schema comment).
	if err := store.UpsertRDNS(ctx, "8.8.8.8", "dns.google"); err != nil {
		t.Fatalf("UpsertRDNS hit: %v", err)
	}
	if err := store.UpsertRDNS(ctx, "192.168.1.1", ""); err != nil { // NULL row — "no PTR"
		t.Fatalf("UpsertRDNS empty: %v", err)
	}

	ips, err := store.UnresolvedIPs(ctx, 100)
	if err != nil {
		t.Fatalf("UnresolvedIPs: %v", err)
	}
	got := map[string]bool{}
	for _, ip := range ips {
		got[ip] = true
	}
	if got["8.8.8.8"] {
		t.Error("8.8.8.8 was resolved with hostname; should not appear in unresolved")
	}
	if got["192.168.1.1"] {
		t.Error("192.168.1.1 was resolved with NULL hostname; still counts as attempted, should not appear")
	}
	if !got["10.0.0.1"] || !got["8.8.4.4"] {
		t.Errorf("expected two remaining unresolved IPs (10.0.0.1 and 8.8.4.4), got %v", got)
	}
}

func TestUnresolvedIPs_HonorsLimit(t *testing.T) {
	store := rdnsTestSetup(t)
	ips, err := store.UnresolvedIPs(context.Background(), 2)
	if err != nil {
		t.Fatalf("UnresolvedIPs: %v", err)
	}
	if len(ips) != 2 {
		t.Errorf("expected exactly 2 IPs with limit=2, got %d (%v)", len(ips), ips)
	}
}

func TestUpsertRDNS_EmptyHostnameStoredAsNULL(t *testing.T) {
	store := rdnsTestSetup(t)
	ctx := context.Background()

	if err := store.UpsertRDNS(ctx, "10.0.0.1", ""); err != nil {
		t.Fatalf("UpsertRDNS: %v", err)
	}

	// Verify via direct query: the row exists, hostname is NULL.
	var hostname any
	err := store.db.QueryRow("SELECT hostname FROM rdns WHERE ip = ?", "10.0.0.1").Scan(&hostname)
	if err != nil {
		t.Fatalf("row not found or scan failed: %v", err)
	}
	if hostname != nil {
		t.Errorf("hostname stored as %v, want NULL", hostname)
	}
}

func TestUpsertRDNS_OverwritesExisting(t *testing.T) {
	store := rdnsTestSetup(t)
	ctx := context.Background()

	if err := store.UpsertRDNS(ctx, "8.8.8.8", "old.example.com"); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := store.UpsertRDNS(ctx, "8.8.8.8", "dns.google"); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	names, err := store.LookupHostnames(ctx, []string{"8.8.8.8"})
	if err != nil {
		t.Fatalf("LookupHostnames: %v", err)
	}
	if names["8.8.8.8"] != "dns.google" {
		t.Errorf("hostname after overwrite = %q, want %q", names["8.8.8.8"], "dns.google")
	}
}

func TestLookupHostnames_ReturnsOnlyResolvedNames(t *testing.T) {
	store := rdnsTestSetup(t)
	ctx := context.Background()

	if err := store.UpsertRDNS(ctx, "8.8.8.8", "dns.google"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertRDNS(ctx, "8.8.4.4", "dns.google"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertRDNS(ctx, "10.0.0.1", ""); err != nil { // no PTR
		t.Fatal(err)
	}

	// Query for all four — only the two with hostnames should come back.
	names, err := store.LookupHostnames(ctx, []string{
		"192.168.1.1", // never resolved → absent from map
		"10.0.0.1",    // NULL hostname → absent from map
		"8.8.8.8",     // resolved → present
		"8.8.4.4",     // resolved → present
	})
	if err != nil {
		t.Fatalf("LookupHostnames: %v", err)
	}
	if len(names) != 2 {
		t.Errorf("expected 2 names, got %d: %v", len(names), names)
	}
	if names["8.8.8.8"] != "dns.google" || names["8.8.4.4"] != "dns.google" {
		t.Errorf("unexpected names: %v", names)
	}
	if _, ok := names["192.168.1.1"]; ok {
		t.Error("unresolved IP should not appear in result")
	}
	if _, ok := names["10.0.0.1"]; ok {
		t.Error("NULL-hostname IP should not appear in result")
	}
}

func TestLookupHostnames_EmptyInputReturnsEmptyMap(t *testing.T) {
	store := rdnsTestSetup(t)
	names, err := store.LookupHostnames(context.Background(), nil)
	if err != nil {
		t.Fatalf("LookupHostnames(nil): %v", err)
	}
	if len(names) != 0 {
		t.Errorf("expected empty map, got %v", names)
	}

	names, err = store.LookupHostnames(context.Background(), []string{})
	if err != nil {
		t.Fatalf("LookupHostnames([]): %v", err)
	}
	if len(names) != 0 {
		t.Errorf("expected empty map, got %v", names)
	}
}
