package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"github.com/preston-peterson/hoptrail/internal/storage"
)

func TestStatus_Aggregate(t *testing.T) {
	base := newFixture(t)
	// Register one stale probe + one active incident + one queued alert.
	ctx := context.Background()
	stale := time.Now().Add(-10 * time.Minute)
	if err := base.store.UpsertProbeHeartbeat(ctx, "site-east", "v9", stale, stale, "192.0.2.60", "amd64"); err != nil {
		t.Fatal(err)
	}
	if err := base.store.UpsertAlertState(ctx, storage.AlertState{
		EventType: "probe_offline", Subject: "site-east", State: "active", Since: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := base.store.EnqueueAlert(ctx, "t", "b", "default", time.Now()); err != nil {
		t.Fatal(err)
	}

	srv, err := New(Config{
		ListenAddr: ":8080",
		Supervisor: base.supervisor,
		Store:      base.store,
		WebFS:      fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("x")}},
		Version:    "v9.9.9",
		StartedAt:  time.Now().Add(-90 * time.Minute),
		UpdateHooks: &UpdateHooks{
			ReadSudoers: func(ctx context.Context) ([]byte, error) {
				return []byte("# SUDOERS_VERSION: 2\n"), nil
			},
		},
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(withTestCSRF(srv.routes()))
	defer ts.Close()

	res, err := http.Get(ts.URL + "/api/status")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", res.StatusCode)
	}
	var st statusResponse
	if err := json.NewDecoder(res.Body).Decode(&st); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if st.Version != "v9.9.9" || st.Listen != ":8080" {
		t.Errorf("identity = %q %q", st.Version, st.Listen)
	}
	if st.UptimeS < 5300 || st.UptimeS > 5500 {
		t.Errorf("uptime_s = %d, want ~5400", st.UptimeS)
	}
	// local synthesized online + the stale remote offline.
	if len(st.Probes) != 2 || st.Probes[0].ProbeID != "local" || !st.Probes[0].Online {
		t.Errorf("probes = %+v", st.Probes)
	}
	if st.Probes[1].ProbeID != "site-east" || st.Probes[1].Online {
		t.Errorf("stale probe = %+v", st.Probes[1])
	}
	if st.Alerts.ActiveIncidents != 1 || st.Alerts.QueueDepth != 1 || st.Alerts.Enabled {
		t.Errorf("alerts = %+v", st.Alerts)
	}
	if st.Database.SchemaVersion < 16 || st.Database.RetentionDays < 0 {
		t.Errorf("database = %+v", st.Database)
	}
	if !st.Update.SudoersOK {
		t.Errorf("update = %+v", st.Update)
	}
}
