package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/preston-peterson/hoptrail/internal/alert"
	"github.com/preston-peterson/hoptrail/internal/storage"
)

func TestAlerts_ConfigTestStatus(t *testing.T) {
	base := newFixture(t)
	var reconfigured *alert.Config
	var posted *storage.AlertQueueItem
	postErr := error(nil)
	srv, err := New(Config{
		ListenAddr: "127.0.0.1:0",
		Supervisor: base.supervisor,
		Store:      base.store,
		WebFS:      fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("x")}},
		AlertReconfigure: func(c alert.Config) { reconfigured = &c },
		AlertSenderStatus: func() (time.Time, string) {
			return time.UnixMilli(1_700_000_000_000), "ntfy 502: down"
		},
		AlertPost: func(ctx context.Context, cfg alert.Config, item storage.AlertQueueItem) error {
			posted = &item
			return postErr
		},
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(withTestCSRF(srv.routes()))
	defer ts.Close()
	f := &fixture{ts: ts, store: base.store}

	// Defaults served.
	code, body := f.doJSON(t, http.MethodGet, "/api/alerts/config", "")
	if code != http.StatusOK {
		t.Fatalf("GET config: %d", code)
	}
	var cfg alertConfigJSON
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Enabled || cfg.LossPct != 20 || !cfg.EventProbeOffline {
		t.Errorf("defaults = %+v", cfg)
	}

	// Test send blocked until transport configured.
	if code, msg := f.doJSON(t, http.MethodPost, "/api/alerts/test", ""); code != http.StatusConflict {
		t.Errorf("test without transport: %d (%s)", code, msg)
	}

	// PATCH full config: stored + engine reconfigured.
	cfg.Enabled, cfg.ServerURL, cfg.Topic = true, "http://127.0.0.1:2586", "hoptrail-x"
	raw, _ := json.Marshal(cfg)
	if code, msg := f.doJSON(t, http.MethodPatch, "/api/alerts/config", string(raw)); code != http.StatusOK {
		t.Fatalf("PATCH: %d (%s)", code, msg)
	}
	if reconfigured == nil || !reconfigured.Enabled || reconfigured.Topic != "hoptrail-x" {
		t.Errorf("engine not reconfigured: %+v", reconfigured)
	}
	// Invalid composite 400s and does not reconfigure.
	reconfigured = nil
	bad := cfg
	bad.SustainS = 1
	raw, _ = json.Marshal(bad)
	if code, _ := f.doJSON(t, http.MethodPatch, "/api/alerts/config", string(raw)); code != http.StatusBadRequest {
		t.Errorf("bad PATCH accepted")
	}
	if reconfigured != nil {
		t.Error("engine reconfigured with invalid config")
	}

	// Test send works regardless of enabled (flip it off first).
	cfg.Enabled = false
	raw, _ = json.Marshal(cfg)
	if code, _ := f.doJSON(t, http.MethodPatch, "/api/alerts/config", string(raw)); code != http.StatusOK {
		t.Fatal("disable PATCH failed")
	}
	if code, _ := f.doJSON(t, http.MethodPost, "/api/alerts/test", ""); code != http.StatusOK {
		t.Errorf("test send with alerts disabled should still work")
	}
	if posted == nil || !strings.Contains(posted.Title, "test notification") {
		t.Errorf("posted = %+v", posted)
	}
	// Delivery failure surfaces as 502 with the message.
	postErr = &alert.PosterPermanentError{}
	if code, _ := f.doJSON(t, http.MethodPost, "/api/alerts/test", ""); code != http.StatusBadGateway {
		t.Errorf("failed test send: want 502")
	}

	// Status: queue depth + incidents + sender state.
	if err := base.store.EnqueueAlert(context.Background(), "t", "b", "default", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := base.store.UpsertAlertState(context.Background(), storage.AlertState{
		EventType: "probe_offline", Subject: "site-east", State: "active", Since: 5,
	}); err != nil {
		t.Fatal(err)
	}
	code, body = f.doJSON(t, http.MethodGet, "/api/alerts/status", "")
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	var st alertStatusResponse
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatal(err)
	}
	if st.QueueDepth != 1 || len(st.Incidents) != 1 || st.Incidents[0].Subject != "site-east" {
		t.Errorf("status = %+v", st)
	}
	if st.LastDeliveryAt == nil || st.LastDeliveryErr != "ntfy 502: down" {
		t.Errorf("sender status = %+v", st)
	}
}

// TestAlertsConfig_TokenNeverLeaks pins audit #9: GET must not return
// the ntfy token, and an empty token on PATCH preserves the stored one.
func TestAlertsConfig_TokenNeverLeaks(t *testing.T) {
	f := newFixture(t)
	srv, err := New(Config{
		ListenAddr: "127.0.0.1:0", Supervisor: f.supervisor, Store: f.store,
		WebFS: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("x")}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(withTestCSRF(srv.routes()))
	defer ts.Close()

	patch := func(body string) {
		req, _ := http.NewRequest(http.MethodPatch, ts.URL+"/api/alerts/config", strings.NewReader(body))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("patch: %v", err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("patch = %d", res.StatusCode)
		}
	}
	get := func() map[string]any {
		res, err := http.Get(ts.URL + "/api/alerts/config")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer res.Body.Close()
		var m map[string]any
		json.NewDecoder(res.Body).Decode(&m)
		return m
	}

	patch(`{"server_url":"http://ntfy.local:2586","topic":"t","token":"SECRET-abc123","loss_pct":50,"sustain_s":60,"latency_level":"warning","cooldown_s":1800,"rate_limit_per_h":10}`)
	m := get()
	if m["token"] != "" {
		t.Errorf("GET leaked token: %v", m["token"])
	}
	if m["token_set"] != true {
		t.Errorf("token_set = %v, want true", m["token_set"])
	}

	// PATCH with empty token preserves the stored secret.
	patch(`{"server_url":"http://ntfy.local:2586","topic":"changed","token":"","loss_pct":50,"sustain_s":60,"latency_level":"warning","cooldown_s":1800,"rate_limit_per_h":10}`)
	cfg, _, _ := alert.LoadConfig(context.Background(), f.store)
	if cfg.Token != "SECRET-abc123" {
		t.Errorf("token after empty PATCH = %q, want preserved", cfg.Token)
	}
	if cfg.Topic != "changed" {
		t.Errorf("topic did not update: %q", cfg.Topic)
	}
}
