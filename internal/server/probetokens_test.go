package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// doJSON is a small helper for the UI-facing probe-management
// endpoints (no bearer token — these ride the same trust as the rest
// of the UI API).
func (f *fixture) doJSON(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, f.ts.URL+path, rdr)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	b := make([]byte, 0, 1024)
	buf := make([]byte, 1024)
	for {
		n, rerr := res.Body.Read(buf)
		b = append(b, buf[:n]...)
		if rerr != nil {
			break
		}
	}
	return res.StatusCode, b
}

func TestProbeTokens_CreateListRevoke(t *testing.T) {
	f := newFixture(t)

	// Create: returns the full token exactly once.
	code, body := f.doJSON(t, http.MethodPost, "/api/probe-tokens", `{"name":"site-east"}`)
	if code != http.StatusOK {
		t.Fatalf("POST status = %d, body %s", code, body)
	}
	var created struct {
		ID    int64  `json:"id"`
		Name  string `json:"name"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("unmarshal create: %v", err)
	}
	if created.Name != "site-east" || len(created.Token) != 43 {
		t.Errorf("created = %+v, want name site-east + 43-char token", created)
	}

	// Reserved and malformed names are rejected.
	for _, bad := range []string{`{"name":"local"}`, `{"name":"all"}`, `{"name":"Bad_Name"}`, `{"name":""}`} {
		if code, _ := f.doJSON(t, http.MethodPost, "/api/probe-tokens", bad); code != http.StatusBadRequest {
			t.Errorf("POST %s: status = %d, want 400", bad, code)
		}
	}

	// List exposes the prefix, never the full token.
	code, body = f.doJSON(t, http.MethodGet, "/api/probe-tokens", "")
	if code != http.StatusOK {
		t.Fatalf("GET status = %d", code)
	}
	if strings.Contains(string(body), created.Token) {
		t.Error("list response leaked the full token")
	}
	var list struct {
		Tokens []struct {
			ID          int64  `json:"id"`
			Name        string `json:"name"`
			TokenPrefix string `json:"token_prefix"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(list.Tokens) != 1 || list.Tokens[0].TokenPrefix != created.Token[:4] {
		t.Errorf("list = %+v, want one entry with prefix %q", list.Tokens, created.Token[:4])
	}

	// A UI-minted token authenticates ingest with no restart, and the
	// heartbeat stamps last_used_at.
	hb := fmt.Sprintf(`{"probe_id":"site-east","version":"test","started_at":%d,"targets":[]}`, time.Now().UnixMilli())
	if code, body := f.postIngest(t, "/api/ingest/heartbeat", created.Token, hb); code != http.StatusOK {
		t.Fatalf("heartbeat with minted token: status = %d, body %s", code, body)
	}
	toks, err := f.store.ListProbeTokens(context.Background())
	if err != nil {
		t.Fatalf("ListProbeTokens: %v", err)
	}
	if len(toks) != 1 || toks[0].LastUsedAt == nil {
		t.Errorf("after heartbeat: LastUsedAt = %v, want stamped", toks)
	}

	// Revoke; the same token now 401s.
	code, _ = f.doJSON(t, http.MethodDelete, fmt.Sprintf("/api/probe-tokens/%d", created.ID), "")
	if code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204", code)
	}
	if code, _ := f.doJSON(t, http.MethodDelete, fmt.Sprintf("/api/probe-tokens/%d", created.ID), ""); code != http.StatusNotFound {
		t.Errorf("second DELETE status = %d, want 404", code)
	}
	if code, _ := f.postIngest(t, "/api/ingest/heartbeat", created.Token, hb); code != http.StatusUnauthorized {
		t.Errorf("heartbeat with revoked token: status = %d, want 401", code)
	}
}

func TestProbeDelete_Endpoint(t *testing.T) {
	f := newFixture(t)

	// Register a probe via the yaml fixture token.
	hb := fmt.Sprintf(`{"probe_id":"site-west","version":"test","started_at":%d,"targets":[]}`, time.Now().UnixMilli())
	if code, body := f.postIngest(t, "/api/ingest/heartbeat", testAgentToken, hb); code != http.StatusOK {
		t.Fatalf("heartbeat: status = %d, body %s", code, body)
	}

	// Reserved identities can't be forgotten.
	if code, _ := f.doJSON(t, http.MethodDelete, "/api/probes/local", ""); code != http.StatusBadRequest {
		t.Errorf("DELETE local: status = %d, want 400", code)
	}
	if code, _ := f.doJSON(t, http.MethodDelete, "/api/probes/all", ""); code != http.StatusBadRequest {
		t.Errorf("DELETE all: status = %d, want 400", code)
	}

	if code, _ := f.doJSON(t, http.MethodDelete, "/api/probes/site-west", ""); code != http.StatusNoContent {
		t.Errorf("DELETE site-west: status = %d, want 204", code)
	}
	if code, _ := f.doJSON(t, http.MethodDelete, "/api/probes/site-west", ""); code != http.StatusNotFound {
		t.Errorf("second DELETE: status = %d, want 404", code)
	}

	// Gone from the probe list (only the synthesized local remains).
	code, body := f.doJSON(t, http.MethodGet, "/api/probes", "")
	if code != http.StatusOK {
		t.Fatalf("GET probes: status = %d", code)
	}
	if strings.Contains(string(body), "site-west") {
		t.Errorf("probe list still contains site-west: %s", body)
	}
}
