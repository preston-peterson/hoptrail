package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestLayout_RoundTripAndNormalize(t *testing.T) {
	f := newFixture(t)

	// Default: the three sections in the main stack, empty dock
	// (route_changes retired in step-131; logs never made it past
	// step-129 — both must be absent).
	code, body := f.doJSON(t, http.MethodGet, "/api/layout", "")
	if code != http.StatusOK {
		t.Fatalf("GET: %d", code)
	}
	var l layoutJSON
	if err := json.Unmarshal(body, &l); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if strings.Join(l.Order, ",") != "latency,bandwidth,hops" {
		t.Errorf("default order = %v", l.Order)
	}
	if l.Side == nil || len(*l.Side) != 0 || l.SidePosition != "right" {
		t.Errorf("default side = %+v pos %s", l.Side, l.SidePosition)
	}

	// PATCH a rearrangement with a dock; round-trips.
	code, body = f.doJSON(t, http.MethodPatch, "/api/layout",
		`{"order":["bandwidth","latency"],"side":["hops"],"side_position":"left","side_width":420,"collapsed":{"bandwidth":true}}`)
	if code != http.StatusOK {
		t.Fatalf("PATCH: %d (%s)", code, body)
	}
	code, body = f.doJSON(t, http.MethodGet, "/api/layout", "")
	if code != http.StatusOK {
		t.Fatalf("GET 2: %d", code)
	}
	var l2 layoutJSON
	if err := json.Unmarshal(body, &l2); err != nil {
		t.Fatalf("unmarshal 2: %v", err)
	}
	if l2.SideWidth != 420 {
		t.Errorf("side_width = %d, want 420", l2.SideWidth)
	}
	if strings.Join(l2.Order, ",") != "bandwidth,latency" ||
		l2.Side == nil || strings.Join(*l2.Side, ",") != "hops" ||
		l2.SidePosition != "left" {
		t.Errorf("stored = order %v side %v pos %s", l2.Order, l2.Side, l2.SidePosition)
	}
	if !l2.Collapsed["bandwidth"] || len(l2.Collapsed) != 1 {
		t.Errorf("collapsed = %v", l2.Collapsed)
	}

	// Garbage normalizes instead of erroring: unknown/retired ids
	// dropped, dups deduped (side wins), missing appended to main,
	// unknown collapse keys filtered, bogus side_position → right.
	code, body = f.doJSON(t, http.MethodPatch, "/api/layout",
		`{"order":["hops","nonsense","hops","latency","route_changes"],"side":["hops","logs"],"side_position":"top","side_width":9999,"collapsed":{"nonsense":true,"hops":true,"route_changes":true,"latency":false}}`)
	if code != http.StatusOK {
		t.Fatalf("PATCH garbage: %d (%s)", code, body)
	}
	var norm layoutJSON
	if err := json.Unmarshal(body, &norm); err != nil {
		t.Fatalf("unmarshal 3: %v", err)
	}
	if strings.Join(norm.Order, ",") != "latency,bandwidth" {
		t.Errorf("normalized order = %v", norm.Order)
	}
	if norm.Side == nil || strings.Join(*norm.Side, ",") != "hops" {
		t.Errorf("normalized side = %v", norm.Side)
	}
	if norm.SidePosition != "right" {
		t.Errorf("normalized side_position = %q", norm.SidePosition)
	}
	if norm.SideWidth != 340 {
		t.Errorf("garbage side_width should clamp to default 340, got %d", norm.SideWidth)
	}
	if !norm.Collapsed["hops"] || len(norm.Collapsed) != 1 {
		t.Errorf("normalized collapsed = %v", norm.Collapsed)
	}
}

// A stored layout from the steps 126-130 era (route_changes docked)
// degrades gracefully: the retired section vanishes, everything else
// keeps its arrangement.
func TestLayout_RetiredSectionDropsFromStoredRows(t *testing.T) {
	f := newFixture(t)
	if err := f.store.SetConfig(context.Background(), "ui.layout",
		`{"order":["hops","latency","bandwidth"],"side":["route_changes","bandwidth"],"side_position":"left","collapsed":{"route_changes":true,"hops":true}}`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	code, body := f.doJSON(t, http.MethodGet, "/api/layout", "")
	if code != http.StatusOK {
		t.Fatalf("GET: %d", code)
	}
	var l layoutJSON
	if err := json.Unmarshal(body, &l); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if strings.Join(l.Order, ",") != "hops,latency" {
		t.Errorf("migrated order = %v", l.Order)
	}
	// bandwidth was in BOTH lists in the stored row — side wins.
	if l.Side == nil || strings.Join(*l.Side, ",") != "bandwidth" || l.SidePosition != "left" {
		t.Errorf("migrated side = %v pos %s", l.Side, l.SidePosition)
	}
	if !l.Collapsed["hops"] || len(l.Collapsed) != 1 {
		t.Errorf("migrated collapsed = %v", l.Collapsed)
	}
}
