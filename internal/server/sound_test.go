package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func soundGET(t *testing.T, url string) soundConfigJSON {
	t.Helper()
	res, err := http.Get(url + "/api/alerts/sound")
	if err != nil {
		t.Fatalf("GET sound: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET sound = %d", res.StatusCode)
	}
	var cfg soundConfigJSON
	if err := json.NewDecoder(res.Body).Decode(&cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return cfg
}

func soundPATCH(t *testing.T, url, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPatch, url+"/api/alerts/sound", strings.NewReader(body))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH sound: %v", err)
	}
	return res
}

func TestSoundConfig_Defaults(t *testing.T) {
	uf := newUpdateFixture(t)
	cfg := soundGET(t, uf.ts.URL)
	if cfg.Enabled {
		t.Error("enabled = true by default, want false (no surprise noise)")
	}
	if len(cfg.Events) != 4 {
		t.Fatalf("events = %v, want all 4 types", cfg.Events)
	}
	for k, v := range cfg.Events {
		if !v {
			t.Errorf("event %s = false by default, want true", k)
		}
	}
}

func TestSoundConfig_PatchRoundTrip(t *testing.T) {
	uf := newUpdateFixture(t)
	res := soundPATCH(t, uf.ts.URL, `{"enabled":true,"events":{"derate":false}}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("PATCH = %d", res.StatusCode)
	}
	cfg := soundGET(t, uf.ts.URL)
	if !cfg.Enabled {
		t.Error("enabled not persisted")
	}
	if cfg.Events["derate"] {
		t.Error("derate toggle not persisted")
	}
	// Omitted toggles keep their prior values, not get reset.
	for _, k := range []string{"probe_offline", "target_loss", "latency"} {
		if !cfg.Events[k] {
			t.Errorf("event %s reset by partial patch", k)
		}
	}
}

func TestSoundConfig_MasterFlipPreservesEvents(t *testing.T) {
	uf := newUpdateFixture(t)
	soundPATCH(t, uf.ts.URL, `{"enabled":true,"events":{"latency":false}}`).Body.Close()
	soundPATCH(t, uf.ts.URL, `{"enabled":false,"events":{}}`).Body.Close()
	soundPATCH(t, uf.ts.URL, `{"enabled":true,"events":{}}`).Body.Close()
	cfg := soundGET(t, uf.ts.URL)
	if !cfg.Enabled || cfg.Events["latency"] || !cfg.Events["derate"] {
		t.Errorf("after master off/on: %+v — per-event choices must survive", cfg)
	}
}

func TestSoundConfig_RejectsUnknownEvent(t *testing.T) {
	uf := newUpdateFixture(t)
	res := soundPATCH(t, uf.ts.URL, `{"enabled":true,"events":{"phase_of_moon":true}}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown event = %d, want 400", res.StatusCode)
	}
}
