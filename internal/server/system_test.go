package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

type systemFixture struct {
	ts *httptest.Server

	mu        sync.Mutex
	applied   []string // log levels passed to ApplyLogLevel
	restarted []string
}

func newSystemFixture(t *testing.T) *systemFixture {
	t.Helper()
	base := newFixture(t)
	sf := &systemFixture{}
	srv, err := New(Config{
		ListenAddr:  ":8080",
		Supervisor:  base.supervisor,
		Store:       base.store,
		WebFS:       fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("x")}},
		LogLevel:    "info",
		RDNSEnabled: true,
		ApplyLogLevel: func(level string) error {
			sf.mu.Lock()
			defer sf.mu.Unlock()
			sf.applied = append(sf.applied, level)
			return nil
		},
		UpdateHooks: &UpdateHooks{
			Restart: func(ctx context.Context, unit string) error {
				sf.mu.Lock()
				defer sf.mu.Unlock()
				sf.restarted = append(sf.restarted, unit)
				return nil
			},
		},
	}, nil)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	sf.ts = httptest.NewServer(srv.routes())
	t.Cleanup(sf.ts.Close)
	return sf
}

func (sf *systemFixture) get(t *testing.T) systemSettingsResponse {
	t.Helper()
	res, err := http.Get(sf.ts.URL + "/api/system")
	if err != nil {
		t.Fatalf("GET /api/system: %v", err)
	}
	defer res.Body.Close()
	var out systemSettingsResponse
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func (sf *systemFixture) patch(t *testing.T, body string) (int, systemSettingsResponse, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPatch, sf.ts.URL+"/api/system", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		b := make([]byte, 1024)
		n, _ := res.Body.Read(b)
		return res.StatusCode, systemSettingsResponse{}, string(b[:n])
	}
	var out systemSettingsResponse
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return res.StatusCode, out, ""
}

func TestSystemSettings(t *testing.T) {
	sf := newSystemFixture(t)

	st := sf.get(t)
	if st.Listen != ":8080" || st.LogLevel != "info" || !st.RDNSEnabled || st.RestartRequired {
		t.Fatalf("initial = %+v", st)
	}

	// Log level applies live: no restart flag, callback invoked.
	code, st, _ := sf.patch(t, `{"log_level":"debug"}`)
	if code != http.StatusOK || st.LogLevel != "debug" || st.RestartRequired {
		t.Fatalf("log level patch: %d %+v", code, st)
	}
	sf.mu.Lock()
	if len(sf.applied) != 1 || sf.applied[0] != "debug" {
		t.Errorf("applied = %v, want [debug]", sf.applied)
	}
	sf.mu.Unlock()

	// Listen + rdns are pending-until-restart.
	code, st, _ = sf.patch(t, `{"listen":"127.0.0.1:9090","rdns_enabled":false}`)
	if code != http.StatusOK {
		t.Fatalf("patch: %d", code)
	}
	if st.PendingListen != "127.0.0.1:9090" || !st.RestartRequired {
		t.Errorf("pending listen = %+v", st)
	}
	if st.PendingRDNSEnabled == nil || *st.PendingRDNSEnabled != false {
		t.Errorf("pending rdns = %+v", st.PendingRDNSEnabled)
	}
	// Running values unchanged.
	if st.Listen != ":8080" || !st.RDNSEnabled {
		t.Errorf("running values changed without restart: %+v", st)
	}

	// Setting a pending value back to the running one clears the flag.
	code, st, _ = sf.patch(t, `{"listen":":8080","rdns_enabled":true}`)
	if code != http.StatusOK || st.PendingListen != "" || st.PendingRDNSEnabled != nil {
		t.Errorf("revert: %d %+v", code, st)
	}
	if st.RestartRequired {
		t.Error("restart_required after revert")
	}

	// Validation: bad values 400 and nothing is half-written.
	for _, bad := range []string{
		`{"listen":"no-port"}`,
		`{"listen":"host.example:8080"}`,
		`{"listen":":99999"}`,
		`{"log_level":"loud"}`,
	} {
		if code, _, msg := sf.patch(t, bad); code != http.StatusBadRequest {
			t.Errorf("patch %s: %d (%s), want 400", bad, code, msg)
		}
	}
}

func TestSystemRestart(t *testing.T) {
	sf := newSystemFixture(t)
	res, err := http.Post(sf.ts.URL+"/api/system/restart", "", nil)
	if err != nil {
		t.Fatalf("POST restart: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("restart: %d", res.StatusCode)
	}
	deadline := time.After(3 * time.Second)
	for {
		sf.mu.Lock()
		n := len(sf.restarted)
		sf.mu.Unlock()
		if n == 1 {
			sf.mu.Lock()
			unit := sf.restarted[0]
			sf.mu.Unlock()
			if unit != "hoptrail" {
				t.Errorf("restarted unit = %q", unit)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("restart hook never called")
		case <-time.After(20 * time.Millisecond):
		}
	}
}
