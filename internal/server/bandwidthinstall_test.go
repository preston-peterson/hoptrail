package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"
)

// TestBandwidthInstallCLI pins the step-123 flow: POST starts the
// injected installer and returns immediately; a second POST while
// running 409s; GET reports running → ok; a successful install
// triggers RecheckCapability.
func TestBandwidthInstallCLI(t *testing.T) {
	f := newFixture(t)

	block := make(chan struct{})
	rechecked := make(chan struct{}, 1)
	srv, err := New(Config{
		ListenAddr: "127.0.0.1:0",
		Supervisor: f.supervisor,
		Store:      f.store,
		WebFS:      fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("x")}},
		SpeedtestInstall: func(ctx context.Context) ([]byte, error) {
			<-block
			return []byte("speedtest CLI installed: test 1.0"), nil
		},
		RecheckCapability: func() { rechecked <- struct{}{} },
	}, nil)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	getStatus := func() cliInstallStatus {
		t.Helper()
		res, err := http.Get(ts.URL + "/api/bandwidth/install-cli")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer res.Body.Close()
		var st cliInstallStatus
		if err := json.NewDecoder(res.Body).Decode(&st); err != nil {
			t.Fatalf("decode status: %v", err)
		}
		return st
	}

	if st := getStatus(); st.Status != "idle" {
		t.Fatalf("initial status = %q, want idle", st.Status)
	}

	res, err := http.Post(ts.URL+"/api/bandwidth/install-cli", "", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202", res.StatusCode)
	}
	if st := getStatus(); st.Status != "running" {
		t.Fatalf("mid-install status = %q, want running", st.Status)
	}

	// Second POST while running → 409.
	res2, err := http.Post(ts.URL+"/api/bandwidth/install-cli", "", nil)
	if err != nil {
		t.Fatalf("POST 2: %v", err)
	}
	res2.Body.Close()
	if res2.StatusCode != http.StatusConflict {
		t.Fatalf("concurrent POST status = %d, want 409", res2.StatusCode)
	}

	close(block)

	// The goroutine finishes asynchronously — poll briefly.
	deadline := time.After(3 * time.Second)
	for {
		st := getStatus()
		if st.Status == "ok" {
			if st.Output == "" {
				t.Error("ok status with empty output")
			}
			break
		}
		select {
		case <-deadline:
			t.Fatalf("status never reached ok (last %q)", st.Status)
		case <-time.After(20 * time.Millisecond):
		}
	}
	select {
	case <-rechecked:
	case <-time.After(2 * time.Second):
		t.Error("RecheckCapability was not called after a successful install")
	}
}
