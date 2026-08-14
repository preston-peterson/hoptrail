package storage

import (
	"context"
	"testing"
	"time"
)

func TestProbeUpdate_Lifecycle(t *testing.T) {
	store := tempStore(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	if pu, err := store.GetProbeUpdate(ctx, "site-east"); err != nil || pu != nil {
		t.Fatalf("GetProbeUpdate empty = (%v, %v), want (nil, nil)", pu, err)
	}

	cmd := ProbeUpdate{ProbeID: "site-east", TargetVersion: "0.7.0", Arch: "arm64", SHA256: "abc123", RequestedAt: now}
	if err := store.CommandProbeUpdate(ctx, cmd); err != nil {
		t.Fatalf("CommandProbeUpdate: %v", err)
	}
	pu, err := store.GetProbeUpdate(ctx, "site-east")
	if err != nil || pu == nil {
		t.Fatalf("GetProbeUpdate: (%v, %v)", pu, err)
	}
	if pu.State != ProbeUpdatePending || pu.TargetVersion != "0.7.0" || pu.Deliveries != 0 {
		t.Errorf("commanded row = %+v", pu)
	}

	n, err := store.IncrementProbeUpdateDeliveries(ctx, "site-east", now+1)
	if err != nil || n != 1 {
		t.Errorf("first delivery = (%d, %v), want 1", n, err)
	}
	n, _ = store.IncrementProbeUpdateDeliveries(ctx, "site-east", now+2)
	if n != 2 {
		t.Errorf("second delivery = %d, want 2", n)
	}

	if err := store.SetProbeUpdateState(ctx, "site-east", ProbeUpdateApplying, "", now+3); err != nil {
		t.Fatalf("SetProbeUpdateState applying: %v", err)
	}
	if err := store.SetProbeUpdateState(ctx, "site-east", ProbeUpdateFailed, "sha mismatch", now+4); err != nil {
		t.Fatalf("SetProbeUpdateState failed: %v", err)
	}
	pu, _ = store.GetProbeUpdate(ctx, "site-east")
	if pu.State != ProbeUpdateFailed || pu.Error != "sha mismatch" || pu.UpdatedAt != now+4 {
		t.Errorf("failed row = %+v", pu)
	}

	// A new command resets everything.
	if err := store.CommandProbeUpdate(ctx, ProbeUpdate{ProbeID: "site-east", TargetVersion: "0.7.1", Arch: "arm64", SHA256: "def", RequestedAt: now + 5}); err != nil {
		t.Fatalf("recommand: %v", err)
	}
	pu, _ = store.GetProbeUpdate(ctx, "site-east")
	if pu.State != ProbeUpdatePending || pu.Error != "" || pu.Deliveries != 0 || pu.TargetVersion != "0.7.1" {
		t.Errorf("recommanded row = %+v", pu)
	}

	if err := store.ClearProbeUpdate(ctx, "site-east"); err != nil {
		t.Fatalf("ClearProbeUpdate: %v", err)
	}
	if pu, _ := store.GetProbeUpdate(ctx, "site-east"); pu != nil {
		t.Errorf("after clear = %+v, want nil", pu)
	}

	// State change on a missing row errors (callers treat as conflict).
	if err := store.SetProbeUpdateState(ctx, "site-east", ProbeUpdateApplying, "", now); err == nil {
		t.Error("SetProbeUpdateState on missing row should error")
	}
}

func TestProbe_PinAndArch(t *testing.T) {
	store := tempStore(t)
	ctx := context.Background()
	now := time.Now()

	if err := store.UpsertProbeHeartbeat(ctx, "site-east", "v0.7.0", now, now, "192.0.2.9", "arm64"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := store.SetProbePin(ctx, "site-east", true); err != nil {
		t.Fatalf("SetProbePin: %v", err)
	}
	probes, err := store.ListProbes(ctx)
	if err != nil || len(probes) != 1 {
		t.Fatalf("ListProbes: (%v, %v)", probes, err)
	}
	p := probes[0]
	if p.Arch == nil || *p.Arch != "arm64" || !p.Pin {
		t.Errorf("probe = %+v, want arch arm64 + pinned", p)
	}

	// An old probe (no arch) must not wipe the stored arch.
	if err := store.UpsertProbeHeartbeat(ctx, "site-east", "v0.7.0", now, now.Add(time.Minute), "192.0.2.9", ""); err != nil {
		t.Fatalf("upsert no-arch: %v", err)
	}
	probes, _ = store.ListProbes(ctx)
	if probes[0].Arch == nil || *probes[0].Arch != "arm64" {
		t.Errorf("arch wiped by archless heartbeat: %+v", probes[0].Arch)
	}

	if err := store.SetProbePin(ctx, "nope", true); err == nil {
		t.Error("SetProbePin on unknown probe should error")
	}
}
