package storage

import (
	"context"
	"testing"
)

// Pins the step-130 per-tab inline-route-changes toggle: persisted on
// the tab row (cross-browser by design), default off, updatable
// without touching the tab's other fields.
func TestTab_ShowRouteChanges(t *testing.T) {
	s := openTokenTestStore(t)
	ctx := context.Background()

	if err := s.AddActiveTarget(ctx, "192.0.2.7"); err != nil {
		t.Fatalf("AddActiveTarget: %v", err)
	}
	label := "site"
	id, err := s.CreateTab(ctx, "192.0.2.7", &label, nil, nil, LocalProbeID)
	if err != nil {
		t.Fatalf("CreateTab: %v", err)
	}

	tabs, _ := s.ListTabs(ctx)
	if len(tabs) != 1 || tabs[0].ShowRouteChanges {
		t.Fatalf("fresh tab = %+v, want show_route_changes off", tabs)
	}

	on := true
	if err := s.UpdateTab(ctx, id, nil, false, nil, nil, false, nil, &on); err != nil {
		t.Fatalf("UpdateTab on: %v", err)
	}
	tabs, _ = s.ListTabs(ctx)
	if !tabs[0].ShowRouteChanges {
		t.Error("toggle on did not persist")
	}
	if tabs[0].Label == nil || *tabs[0].Label != "site" {
		t.Errorf("label disturbed by toggle update: %+v", tabs[0].Label)
	}

	off := false
	if err := s.UpdateTab(ctx, id, nil, false, nil, nil, false, nil, &off); err != nil {
		t.Fatalf("UpdateTab off: %v", err)
	}
	tabs, _ = s.ListTabs(ctx)
	if tabs[0].ShowRouteChanges {
		t.Error("toggle off did not persist")
	}
}
