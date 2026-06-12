package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTokenTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "tokens.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestProbeTokens_CRUD(t *testing.T) {
	s := openTokenTestStore(t)
	ctx := context.Background()
	created := time.UnixMilli(1_700_000_000_000)

	id, err := s.InsertProbeToken(ctx, "tok-aaaa-1111", "site-east", created)
	if err != nil {
		t.Fatalf("InsertProbeToken: %v", err)
	}
	if _, err := s.InsertProbeToken(ctx, "tok-bbbb-2222", "site-west", created.Add(time.Minute)); err != nil {
		t.Fatalf("InsertProbeToken 2: %v", err)
	}

	// Duplicate token must be a loud error (UNIQUE constraint).
	if _, err := s.InsertProbeToken(ctx, "tok-aaaa-1111", "dup", created); err == nil {
		t.Fatal("duplicate token insert: want error, got nil")
	}

	list, err := s.ListProbeTokens(ctx)
	if err != nil {
		t.Fatalf("ListProbeTokens: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len(list) = %d, want 2", len(list))
	}
	// Newest first.
	if list[0].Name != "site-west" || list[1].Name != "site-east" {
		t.Errorf("order = %s, %s; want site-west, site-east", list[0].Name, list[1].Name)
	}
	if list[1].LastUsedAt != nil {
		t.Errorf("fresh token LastUsedAt = %v, want nil", *list[1].LastUsedAt)
	}

	// Auth set contains both secrets.
	secrets, err := s.ProbeTokenSecrets(ctx)
	if err != nil {
		t.Fatalf("ProbeTokenSecrets: %v", err)
	}
	if len(secrets) != 2 {
		t.Fatalf("len(secrets) = %d, want 2", len(secrets))
	}

	// Touch stamps last_used_at on the right row only.
	used := created.Add(time.Hour)
	if err := s.TouchProbeToken(ctx, "tok-aaaa-1111", used); err != nil {
		t.Fatalf("TouchProbeToken: %v", err)
	}
	// Touching an unknown (yaml-configured) token is a silent no-op.
	if err := s.TouchProbeToken(ctx, "not-in-table", used); err != nil {
		t.Fatalf("TouchProbeToken unknown: %v", err)
	}
	list, _ = s.ListProbeTokens(ctx)
	for _, tok := range list {
		switch tok.Token {
		case "tok-aaaa-1111":
			if tok.LastUsedAt == nil || *tok.LastUsedAt != used.UnixMilli() {
				t.Errorf("touched token LastUsedAt = %v, want %d", tok.LastUsedAt, used.UnixMilli())
			}
		case "tok-bbbb-2222":
			if tok.LastUsedAt != nil {
				t.Errorf("untouched token LastUsedAt = %v, want nil", *tok.LastUsedAt)
			}
		}
	}

	// Revoke removes the row; the auth set shrinks immediately.
	found, err := s.DeleteProbeToken(ctx, id)
	if err != nil {
		t.Fatalf("DeleteProbeToken: %v", err)
	}
	if !found {
		t.Fatal("DeleteProbeToken: found = false, want true")
	}
	if found, _ := s.DeleteProbeToken(ctx, id); found {
		t.Error("second delete: found = true, want false")
	}
	secrets, _ = s.ProbeTokenSecrets(ctx)
	if len(secrets) != 1 || secrets[0] != "tok-bbbb-2222" {
		t.Errorf("secrets after revoke = %v, want [tok-bbbb-2222]", secrets)
	}
}

func TestDeleteProbe(t *testing.T) {
	s := openTokenTestStore(t)
	ctx := context.Background()
	now := time.UnixMilli(1_700_000_000_000)

	if err := s.UpsertProbeHeartbeat(ctx, "site-east", "0.5.0", now, now, "192.0.2.50"); err != nil {
		t.Fatalf("UpsertProbeHeartbeat: %v", err)
	}
	if err := s.UpsertPathSnapshot(ctx, PathSnapshot{
		ProbeID: "site-east", Target: "192.0.2.1", Ts: now.UnixMilli(), HopCount: 3, TargetTTL: 3, HopsJSON: "[]",
	}); err != nil {
		t.Fatalf("UpsertPathSnapshot: %v", err)
	}
	// tabs.target has a FK on active_targets.
	if err := s.AddActiveTarget(ctx, "192.0.2.1"); err != nil {
		t.Fatalf("AddActiveTarget: %v", err)
	}
	tabID, err := s.CreateTab(ctx, "192.0.2.1", nil, nil, nil, "site-east")
	if err != nil {
		t.Fatalf("CreateTab: %v", err)
	}

	// Forgetting the local probe is invalid by contract.
	if _, err := s.DeleteProbe(ctx, LocalProbeID); err == nil {
		t.Error("DeleteProbe(local): want error, got nil")
	}

	found, err := s.DeleteProbe(ctx, "site-east")
	if err != nil {
		t.Fatalf("DeleteProbe: %v", err)
	}
	if !found {
		t.Fatal("DeleteProbe: found = false, want true")
	}
	if found, _ := s.DeleteProbe(ctx, "site-east"); found {
		t.Error("second DeleteProbe: found = true, want false")
	}

	probes, _ := s.ListProbes(ctx)
	if len(probes) != 0 {
		t.Errorf("probes after delete = %d rows, want 0", len(probes))
	}
	snap, err := s.GetPathSnapshot(ctx, "site-east", "192.0.2.1")
	if err != nil {
		t.Fatalf("GetPathSnapshot: %v", err)
	}
	if snap != nil {
		t.Error("path snapshot survived DeleteProbe")
	}
	// The tab survives but is repointed at the local probe.
	tabs, err := s.ListTabs(ctx)
	if err != nil {
		t.Fatalf("ListTabs: %v", err)
	}
	for _, tab := range tabs {
		if tab.TabID == tabID && tab.ProbeID != LocalProbeID {
			t.Errorf("tab probe_id after delete = %q, want %q", tab.ProbeID, LocalProbeID)
		}
	}
}
