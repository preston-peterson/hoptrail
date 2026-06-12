package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// ---------- probes table ----------

func TestUpsertProbeHeartbeat_InsertAndUpdate(t *testing.T) {
	store := tempStore(t)
	ctx := context.Background()

	started := time.UnixMilli(1_716_412_800_000)
	seen1 := started.Add(1 * time.Second)
	if err := store.UpsertProbeHeartbeat(ctx, "site-east-pi", "v0.3.0", started, seen1, "192.0.2.80"); err != nil {
		t.Fatalf("first heartbeat: %v", err)
	}

	probes, err := store.ListProbes(ctx)
	if err != nil {
		t.Fatalf("ListProbes: %v", err)
	}
	if len(probes) != 1 {
		t.Fatalf("len(probes) = %d, want 1", len(probes))
	}
	p := probes[0]
	if p.ProbeID != "site-east-pi" {
		t.Errorf("ProbeID = %q, want site-east-pi", p.ProbeID)
	}
	if p.Version == nil || *p.Version != "v0.3.0" {
		t.Errorf("Version = %v, want v0.3.0", p.Version)
	}
	if p.StartedAt == nil || *p.StartedAt != started.UnixMilli() {
		t.Errorf("StartedAt = %v, want %d", p.StartedAt, started.UnixMilli())
	}
	if p.LastSeenAt != seen1.UnixMilli() {
		t.Errorf("LastSeenAt = %d, want %d", p.LastSeenAt, seen1.UnixMilli())
	}
	if p.Label != nil {
		t.Errorf("Label = %v, want nil (never set)", p.Label)
	}

	// Agent restarts with a newer binary: started_at and version move,
	// last_seen_at advances, still one row.
	started2 := started.Add(1 * time.Hour)
	seen2 := started2.Add(2 * time.Second)
	if err := store.UpsertProbeHeartbeat(ctx, "site-east-pi", "v0.3.1", started2, seen2, "192.0.2.80"); err != nil {
		t.Fatalf("second heartbeat: %v", err)
	}
	probes, err = store.ListProbes(ctx)
	if err != nil {
		t.Fatalf("ListProbes after update: %v", err)
	}
	if len(probes) != 1 {
		t.Fatalf("len(probes) after re-heartbeat = %d, want 1 (upsert, not insert)", len(probes))
	}
	p = probes[0]
	if p.Version == nil || *p.Version != "v0.3.1" {
		t.Errorf("Version after update = %v, want v0.3.1", p.Version)
	}
	if p.StartedAt == nil || *p.StartedAt != started2.UnixMilli() {
		t.Errorf("StartedAt after update = %v, want %d", p.StartedAt, started2.UnixMilli())
	}
	if p.LastSeenAt != seen2.UnixMilli() {
		t.Errorf("LastSeenAt after update = %d, want %d", p.LastSeenAt, seen2.UnixMilli())
	}
}

func TestUpsertProbeHeartbeat_PreservesLabel(t *testing.T) {
	store := tempStore(t)
	ctx := context.Background()
	now := time.Now()

	if err := store.UpsertProbeHeartbeat(ctx, "gateway-rack", "v0.3.0", now, now, "192.0.2.80"); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	// Operator sets a label out-of-band (the PATCH /api/probes surface
	// comes later; storage-level the column is just writable).
	if _, err := store.DB().Exec(`UPDATE probes SET label = ? WHERE probe_id = ?`, "Garage rack", "gateway-rack"); err != nil {
		t.Fatalf("set label: %v", err)
	}

	// Next heartbeat must not clobber the label.
	if err := store.UpsertProbeHeartbeat(ctx, "gateway-rack", "v0.3.0", now, now.Add(time.Minute), "192.0.2.80"); err != nil {
		t.Fatalf("re-heartbeat: %v", err)
	}
	probes, err := store.ListProbes(ctx)
	if err != nil {
		t.Fatalf("ListProbes: %v", err)
	}
	if len(probes) != 1 || probes[0].Label == nil || *probes[0].Label != "Garage rack" {
		t.Fatalf("label not preserved across heartbeat: %+v", probes)
	}
}

func TestUpsertProbeHeartbeat_EmptyProbeIDRejected(t *testing.T) {
	store := tempStore(t)
	now := time.Now()
	if err := store.UpsertProbeHeartbeat(context.Background(), "", "v0.3.0", now, now, "192.0.2.80"); err == nil {
		t.Fatal("empty probe_id accepted, want error")
	}
}

func TestListProbes_OrderedByProbeID(t *testing.T) {
	store := tempStore(t)
	ctx := context.Background()
	now := time.Now()

	for _, id := range []string{"zeta-site", "alpha-site", "mid-site"} {
		if err := store.UpsertProbeHeartbeat(ctx, id, "v0.3.0", now, now, "192.0.2.80"); err != nil {
			t.Fatalf("heartbeat %s: %v", id, err)
		}
	}
	probes, err := store.ListProbes(ctx)
	if err != nil {
		t.Fatalf("ListProbes: %v", err)
	}
	want := []string{"alpha-site", "mid-site", "zeta-site"}
	if len(probes) != len(want) {
		t.Fatalf("len(probes) = %d, want %d", len(probes), len(want))
	}
	for i, w := range want {
		if probes[i].ProbeID != w {
			t.Errorf("probes[%d] = %q, want %q", i, probes[i].ProbeID, w)
		}
	}
}

// ---------- path_snapshots table ----------

func TestPathSnapshot_RoundTripAndOverwrite(t *testing.T) {
	store := tempStore(t)
	ctx := context.Background()

	snap := PathSnapshot{
		ProbeID:   "site-east-pi",
		Target:    "8.8.8.8",
		Ts:        1_716_412_800_000,
		HopCount:  11,
		TargetTTL: 11,
		HopsJSON:  `[{"ttl":1,"ip":"192.0.2.1"}]`,
	}
	if err := store.UpsertPathSnapshot(ctx, snap); err != nil {
		t.Fatalf("UpsertPathSnapshot: %v", err)
	}

	got, err := store.GetPathSnapshot(ctx, "site-east-pi", "8.8.8.8")
	if err != nil {
		t.Fatalf("GetPathSnapshot: %v", err)
	}
	if got == nil {
		t.Fatal("GetPathSnapshot returned nil for existing snapshot")
	}
	if *got != snap {
		t.Errorf("round-trip mismatch: got %+v, want %+v", *got, snap)
	}

	// Newer snapshot for the same (probe, target) replaces, never
	// accumulates — the table holds current state.
	snap2 := snap
	snap2.Ts = snap.Ts + 30_000
	snap2.HopCount = 12
	snap2.TargetTTL = 12
	snap2.HopsJSON = `[{"ttl":1,"ip":"192.0.2.99"}]`
	if err := store.UpsertPathSnapshot(ctx, snap2); err != nil {
		t.Fatalf("second UpsertPathSnapshot: %v", err)
	}
	got, err = store.GetPathSnapshot(ctx, "site-east-pi", "8.8.8.8")
	if err != nil {
		t.Fatalf("GetPathSnapshot after overwrite: %v", err)
	}
	if got == nil || *got != snap2 {
		t.Errorf("overwrite mismatch: got %+v, want %+v", got, snap2)
	}

	var n int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM path_snapshots`).Scan(&n); err != nil {
		t.Fatalf("count path_snapshots: %v", err)
	}
	if n != 1 {
		t.Errorf("path_snapshots row count = %d, want 1 (overwrite, not append)", n)
	}
}

func TestGetPathSnapshot_MissingReturnsNilNoError(t *testing.T) {
	store := tempStore(t)
	got, err := store.GetPathSnapshot(context.Background(), "never-seen", "8.8.8.8")
	if err != nil {
		t.Fatalf("GetPathSnapshot on empty table: %v", err)
	}
	if got != nil {
		t.Errorf("got %+v, want nil for missing snapshot", got)
	}
}

func TestPathSnapshot_KeyedByProbeAndTarget(t *testing.T) {
	store := tempStore(t)
	ctx := context.Background()

	// Same target from two probes — both snapshots coexist.
	for _, probeID := range []string{"site-east-pi", LocalProbeID} {
		snap := PathSnapshot{
			ProbeID: probeID, Target: "8.8.8.8",
			Ts: 1, HopCount: 1, TargetTTL: 1, HopsJSON: `[]`,
		}
		if err := store.UpsertPathSnapshot(ctx, snap); err != nil {
			t.Fatalf("UpsertPathSnapshot(%s): %v", probeID, err)
		}
	}
	var n int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM path_snapshots`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("row count = %d, want 2 (one per probe for the same target)", n)
	}
}

// ---------- ingest_log dedup ----------

func TestRecordIngestBatch_FirstTrueDuplicateFalse(t *testing.T) {
	store := tempStore(t)
	ctx := context.Background()
	now := time.Now()

	fresh, err := store.RecordIngestBatch(ctx, "01HZX5J9Q0BATCH", "site-east-pi", now)
	if err != nil {
		t.Fatalf("first RecordIngestBatch: %v", err)
	}
	if !fresh {
		t.Error("first delivery reported as duplicate, want fresh")
	}

	// Agent retry after a lost ack: same batch_id, must dedup.
	fresh, err = store.RecordIngestBatch(ctx, "01HZX5J9Q0BATCH", "site-east-pi", now.Add(time.Second))
	if err != nil {
		t.Fatalf("duplicate RecordIngestBatch: %v", err)
	}
	if fresh {
		t.Error("duplicate delivery reported as fresh, want dedup")
	}

	// A different batch_id is fresh again.
	fresh, err = store.RecordIngestBatch(ctx, "01HZX5J9Q1OTHER", "site-east-pi", now)
	if err != nil {
		t.Fatalf("second batch RecordIngestBatch: %v", err)
	}
	if !fresh {
		t.Error("distinct batch_id reported as duplicate")
	}
}

func TestRecordIngestBatch_EmptyBatchIDRejected(t *testing.T) {
	store := tempStore(t)
	if _, err := store.RecordIngestBatch(context.Background(), "", "site-east-pi", time.Now()); err == nil {
		t.Fatal("empty batch_id accepted, want error")
	}
}

func TestIngestBatch_WritesRowsUnderProbeID(t *testing.T) {
	store := tempStore(t)
	ctx := context.Background()
	ip := "192.0.2.1"
	oldIP := "203.0.113.12"

	fresh, err := store.IngestBatch(ctx, "site-east-pi", "01HZXBATCH", time.Now(),
		[]IngestSample{
			{Target: "8.8.8.8", TTL: 1, Ts: 1000, IP: &ip, RTTUs: 400},
			{Target: "8.8.8.8", TTL: 2, Ts: 1001, IP: nil, RTTUs: 0}, // timeout
		},
		[]IngestRouteChange{
			{Target: "8.8.8.8", TTL: 3, Ts: 1002, OldIP: &oldIP, NewIP: "203.0.113.45"},
		},
	)
	if err != nil {
		t.Fatalf("IngestBatch: %v", err)
	}
	if !fresh {
		t.Fatal("first delivery reported as duplicate")
	}

	var n int
	if err := store.DB().QueryRow(
		`SELECT COUNT(*) FROM samples WHERE probe_id = 'site-east-pi'`).Scan(&n); err != nil {
		t.Fatalf("count samples: %v", err)
	}
	if n != 2 {
		t.Errorf("sample rows = %d, want 2", n)
	}
	var nullIP int
	if err := store.DB().QueryRow(
		`SELECT COUNT(*) FROM samples WHERE probe_id = 'site-east-pi' AND ip IS NULL`).Scan(&nullIP); err != nil {
		t.Fatalf("count null-ip samples: %v", err)
	}
	if nullIP != 1 {
		t.Errorf("NULL-ip rows = %d, want 1 (the timeout sample)", nullIP)
	}
	if err := store.DB().QueryRow(
		`SELECT COUNT(*) FROM route_changes WHERE probe_id = 'site-east-pi'`).Scan(&n); err != nil {
		t.Fatalf("count route_changes: %v", err)
	}
	if n != 1 {
		t.Errorf("route_change rows = %d, want 1", n)
	}
}

func TestIngestBatch_DuplicateWritesNothing(t *testing.T) {
	store := tempStore(t)
	ctx := context.Background()
	ip := "192.0.2.1"
	batch := []IngestSample{{Target: "8.8.8.8", TTL: 1, Ts: 1000, IP: &ip, RTTUs: 400}}

	for i, wantFresh := range []bool{true, false} {
		fresh, err := store.IngestBatch(ctx, "site-east-pi", "01HZXDUP", time.Now(), batch, nil)
		if err != nil {
			t.Fatalf("delivery %d: %v", i+1, err)
		}
		if fresh != wantFresh {
			t.Errorf("delivery %d: fresh = %v, want %v", i+1, fresh, wantFresh)
		}
	}

	var n int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM samples`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("samples after duplicate delivery = %d, want 1 (dedup is transactional)", n)
	}
}

func TestIngestBatch_EmptyBatchStillDedups(t *testing.T) {
	// A batch with no samples or changes is legal (idle agent flushing
	// its timer) — the batch_id is still recorded so a retry dedups.
	store := tempStore(t)
	ctx := context.Background()

	fresh, err := store.IngestBatch(ctx, "site-east-pi", "01HZXEMPTY", time.Now(), nil, nil)
	if err != nil {
		t.Fatalf("IngestBatch: %v", err)
	}
	if !fresh {
		t.Error("empty batch first delivery reported as duplicate")
	}
	fresh, err = store.IngestBatch(ctx, "site-east-pi", "01HZXEMPTY", time.Now(), nil, nil)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if fresh {
		t.Error("empty batch retry reported as fresh")
	}
}

func TestDeleteIngestLogOlderThan(t *testing.T) {
	store := tempStore(t)
	ctx := context.Background()

	cutoff := time.UnixMilli(1_716_412_800_000)
	cases := []struct {
		batchID string
		at      time.Time
	}{
		{"old-batch", cutoff.Add(-time.Hour)},
		{"edge-batch", cutoff}, // exactly at cutoff: kept (strict less-than)
		{"new-batch", cutoff.Add(time.Hour)},
	}
	for _, c := range cases {
		if _, err := store.RecordIngestBatch(ctx, c.batchID, "site-east-pi", c.at); err != nil {
			t.Fatalf("seed %s: %v", c.batchID, err)
		}
	}

	n, err := store.DeleteIngestLogOlderThan(ctx, cutoff)
	if err != nil {
		t.Fatalf("DeleteIngestLogOlderThan: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted = %d, want 1", n)
	}

	rows, err := store.DB().Query(`SELECT batch_id FROM ingest_log ORDER BY batch_id`)
	if err != nil {
		t.Fatalf("query remaining: %v", err)
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
	want := []string{"edge-batch", "new-batch"}
	if len(remaining) != len(want) || remaining[0] != want[0] || remaining[1] != want[1] {
		t.Errorf("remaining = %v, want %v", remaining, want)
	}
}

// ---------- per-tab probe (step-96, migration v12) ----------

func TestTabProbeID_DefaultAndRoundTrip(t *testing.T) {
	store := tempStore(t)
	ctx := context.Background()
	if _, err := store.DB().Exec(
		`INSERT INTO active_targets (target, added_at) VALUES ('8.8.8.8', 1)`); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	// Empty probeID → local (the v0.2 behavior).
	defID, err := store.CreateTab(ctx, "8.8.8.8", nil, nil, nil, "")
	if err != nil {
		t.Fatalf("CreateTab default: %v", err)
	}
	// Explicit remote probe sticks.
	remID, err := store.CreateTab(ctx, "8.8.8.8", nil, nil, nil, "site-east-pi")
	if err != nil {
		t.Fatalf("CreateTab remote: %v", err)
	}

	tabs, err := store.ListTabs(ctx)
	if err != nil {
		t.Fatalf("ListTabs: %v", err)
	}
	byID := map[int64]Tab{}
	for _, tab := range tabs {
		byID[tab.TabID] = tab
	}
	if byID[defID].ProbeID != LocalProbeID {
		t.Errorf("default tab probe = %q, want local", byID[defID].ProbeID)
	}
	if byID[remID].ProbeID != "site-east-pi" {
		t.Errorf("remote tab probe = %q, want site-east-pi", byID[remID].ProbeID)
	}

	// Update flips it; other fields untouched.
	newProbe := "gateway-rack"
	if err := store.UpdateTab(ctx, defID, nil, false, nil, nil, false, &newProbe, nil); err != nil {
		t.Fatalf("UpdateTab probe: %v", err)
	}
	tabs, _ = store.ListTabs(ctx)
	for _, tab := range tabs {
		if tab.TabID == defID && tab.ProbeID != "gateway-rack" {
			t.Errorf("updated tab probe = %q, want gateway-rack", tab.ProbeID)
		}
	}
}

// TestMigrationV12_BackfillsTabsAsLocal pins the upgrade: tabs created
// under schema v11 get probe_id='local' when v12 applies.
func TestMigrationV12_BackfillsTabsAsLocal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	db, err := sql.Open("sqlite3", path+dsnParams)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	pre := &Store{db: db}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_version (
			version INTEGER NOT NULL PRIMARY KEY,
			applied_at INTEGER NOT NULL
		)
	`); err != nil {
		t.Fatalf("create schema_version: %v", err)
	}
	for _, m := range migrations {
		if m.version > 11 {
			break
		}
		if err := pre.applyMigration(m); err != nil {
			t.Fatalf("apply migration v%d: %v", m.version, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO active_targets (target, added_at) VALUES ('8.8.8.8', 1)`); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO tabs (target, position, created_at) VALUES ('8.8.8.8', 0, 1)`); err != nil {
		t.Fatalf("seed v11 tab: %v", err)
	}
	if err := pre.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open (v12 migration): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	tabs, err := store.ListTabs(context.Background())
	if err != nil {
		t.Fatalf("ListTabs: %v", err)
	}
	if len(tabs) != 1 || tabs[0].ProbeID != LocalProbeID {
		t.Errorf("pre-v12 tab = %+v, want probe_id local", tabs)
	}
}

func TestBundleTab_ProbeIDRoundTrip(t *testing.T) {
	store := tempStore(t)
	ctx := context.Background()
	if err := store.SaveBundle(ctx, "multi-site", []BundleTab{
		{Target: "8.8.8.8"},
		{Target: "8.8.8.8", ProbeID: "site-east-pi"},
	}); err != nil {
		t.Fatalf("SaveBundle: %v", err)
	}
	bundles, err := store.ListBundles(ctx)
	if err != nil {
		t.Fatalf("ListBundles: %v", err)
	}
	if len(bundles) != 1 || len(bundles[0].Tabs) != 2 {
		t.Fatalf("bundles = %+v, want 1 bundle with 2 tabs", bundles)
	}
	if bundles[0].Tabs[0].ProbeID != "" {
		t.Errorf("tab0 probe = %q, want empty (local)", bundles[0].Tabs[0].ProbeID)
	}
	if bundles[0].Tabs[1].ProbeID != "site-east-pi" {
		t.Errorf("tab1 probe = %q, want site-east-pi", bundles[0].Tabs[1].ProbeID)
	}
}

// ---------- v10 → v11 upgrade path ----------

// TestMigrationV11_BackfillsExistingRowsAsLocal pins the design's
// §9 no-data-loss promise for a real upgrade: a database stopped at
// schema v10 with v0.2-era rows gets probe_id='local' backfilled on
// every existing sample and route_change when v11 applies. Fresh-DB
// tests can't catch a backfill regression — they never have
// pre-migration rows.
func TestMigrationV11_BackfillsExistingRowsAsLocal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	// Build a v10 database by hand: same DSN, but stop the migration
	// replay before v11 instead of using Open.
	db, err := sql.Open("sqlite3", path+dsnParams)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	pre := &Store{db: db}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_version (
			version INTEGER NOT NULL PRIMARY KEY,
			applied_at INTEGER NOT NULL
		)
	`); err != nil {
		t.Fatalf("create schema_version: %v", err)
	}
	for _, m := range migrations {
		if m.version > 10 {
			break
		}
		if err := pre.applyMigration(m); err != nil {
			t.Fatalf("apply migration v%d: %v", m.version, err)
		}
	}

	// v0.2-era data: neither insert knows about probe_id.
	if _, err := db.Exec(
		`INSERT INTO samples (target, ttl, ts, ip, rtt_us) VALUES (?, ?, ?, ?, ?)`,
		"8.8.8.8", 1, 1_716_412_800_000, "10.0.0.1", 500,
	); err != nil {
		t.Fatalf("seed v10 sample: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO route_changes (target, ttl, ts, old_ip, new_ip) VALUES (?, ?, ?, ?, ?)`,
		"8.8.8.8", 3, 1_716_412_800_000, "10.0.0.2", "10.0.0.3",
	); err != nil {
		t.Fatalf("seed v10 route_change: %v", err)
	}
	if err := pre.Close(); err != nil {
		t.Fatalf("close v10 store: %v", err)
	}

	// Reopen through the public path — v11 applies here.
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open (v11 migration): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	v, err := store.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != latestSchemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", v, latestSchemaVersion)
	}

	var probeID string
	if err := store.DB().QueryRow(`SELECT probe_id FROM samples`).Scan(&probeID); err != nil {
		t.Fatalf("read backfilled sample probe_id: %v", err)
	}
	if probeID != LocalProbeID {
		t.Errorf("pre-existing sample probe_id = %q, want %q", probeID, LocalProbeID)
	}
	if err := store.DB().QueryRow(`SELECT probe_id FROM route_changes`).Scan(&probeID); err != nil {
		t.Fatalf("read backfilled route_change probe_id: %v", err)
	}
	if probeID != LocalProbeID {
		t.Errorf("pre-existing route_change probe_id = %q, want %q", probeID, LocalProbeID)
	}
}

// ---------- probe_id back-compat hook ----------

// TestBatchedSink_WritesLocalProbeID pins the v0.2→v0.3 compat
// mechanism: the in-process sink's INSERT doesn't name probe_id, so
// the column DEFAULT must attribute its rows to the local probe. If
// this fails, either the migration default changed or the sink began
// naming the column — both would break zero-agent deploys' reads.
func TestBatchedSink_WritesLocalProbeID(t *testing.T) {
	store := tempStore(t)
	sink, cleanup := startSink(t, store)

	if err := sink.WriteSample(sampleAt(t, 1, "10.0.0.1", time.Millisecond)); err != nil {
		t.Fatalf("WriteSample: %v", err)
	}
	cleanup() // cancel + final flush

	var probeID string
	if err := store.DB().QueryRow(`SELECT probe_id FROM samples`).Scan(&probeID); err != nil {
		t.Fatalf("read probe_id: %v", err)
	}
	if probeID != LocalProbeID {
		t.Errorf("sink-written sample probe_id = %q, want %q", probeID, LocalProbeID)
	}
}
