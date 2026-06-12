// Dashboard layout endpoints (step-126): the operator-arranged
// section order + collapsed state for the dashboard's four sections
// (latency, bandwidth, hops, route_changes). One global layout —
// stored as a JSON config row, served to every browser/tab. The
// per-tab alternative was offered and declined (operator: one
// arrangement everywhere).

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// sectionIDs is the closed set of dashboard sections. Adding a
// section later means appending here + a merge rule in normalize
// (unknown-in-stored-order is already handled: missing sections are
// appended in default order; brand-new sections also start collapsed
// so an upgrade doesn't shove a surprise card into a tuned layout).
// "logs" was briefly a section in step-128 and demoted to a settings
// overlay in step-129; "route_changes" retired in step-131 (inline
// route changes in the hop list replaced it, operator's call) —
// normalize drops both from stored layouts automatically.
var sectionIDs = []string{"latency", "bandwidth", "hops"}

// bornCollapsed are sections that default to collapsed when first
// added to a layout that predates them (and in the default layout).
// Empty today; the mechanism stays for the next section addition.
var bornCollapsed = map[string]bool{}

const layoutConfigKey = "ui.layout"

// layoutJSON v2 (step-127): sections live either in the full-width
// main stack (Order) or in a vertical side dock (Side) pinned to
// SidePosition ("right"|"left") — the operator's "route changes back
// on the side" request after the step-126 single-stack proved too
// condensed. Side is a pointer so a stored v1 row (no "side" key,
// all four sections in Order) is distinguishable from an explicitly
// empty dock: legacy rows get route_changes docked right, matching
// the pre-126 look.
type layoutJSON struct {
	Order        []string        `json:"order"`
	Side         *[]string       `json:"side"`
	SidePosition string          `json:"side_position"`
	// SideWidth is the dock column's width in px (step-150: the
	// operator drags a splitter between dock and main). Clamped
	// 240-680 by normalize; 0/absent → 340 (the historical fixed
	// width).
	SideWidth int             `json:"side_width"`
	Collapsed map[string]bool `json:"collapsed"`
}

func defaultLayout() layoutJSON {
	side := []string{}
	return layoutJSON{
		Order:        []string{"latency", "bandwidth", "hops"},
		Side:         &side,
		SidePosition: "right",
		SideWidth:    340,
		Collapsed:    map[string]bool{},
	}
}

// normalizeLayout validates a client-submitted (or stored) layout
// against the known section set: unknown ids are dropped, duplicates
// collapse to the first occurrence (side wins over main), missing
// ids are appended to the main stack, collapsed keys are filtered.
// The result always covers every section exactly once — a future
// binary with a new section degrades a stored old layout gracefully
// instead of 500ing.
func normalizeLayout(in layoutJSON) layoutJSON {
	known := map[string]bool{}
	for _, id := range sectionIDs {
		known[id] = true
	}
	out := layoutJSON{Order: []string{}, SidePosition: "right", SideWidth: 340, Collapsed: map[string]bool{}}
	if in.SidePosition == "left" {
		out.SidePosition = "left"
	}
	if in.SideWidth >= 240 && in.SideWidth <= 680 {
		out.SideWidth = in.SideWidth
	}

	seen := map[string]bool{}
	side := []string{}
	if in.Side != nil {
		for _, id := range *in.Side {
			if known[id] && !seen[id] {
				side = append(side, id)
				seen[id] = true
			}
		}
	}
	for _, id := range in.Order {
		if known[id] && !seen[id] {
			out.Order = append(out.Order, id)
			seen[id] = true
		}
	}
	for _, id := range sectionIDs {
		if !seen[id] {
			out.Order = append(out.Order, id)
			if bornCollapsed[id] {
				out.Collapsed[id] = true
			}
		}
	}
	out.Side = &side
	for id, v := range in.Collapsed {
		if known[id] && v {
			out.Collapsed[id] = true
		}
	}
	return out
}

func (s *Server) loadLayout(ctx context.Context) (layoutJSON, error) {
	v, ok, err := s.cfg.Store.GetConfig(ctx, layoutConfigKey)
	if err != nil {
		return defaultLayout(), err
	}
	if !ok {
		return defaultLayout(), nil
	}
	var stored layoutJSON
	if err := json.Unmarshal([]byte(v), &stored); err != nil {
		// Corrupt row: serve the default rather than wedging the
		// dashboard; the next PATCH overwrites it.
		s.log.Warn("layout: corrupt ui.layout row, serving default", "err", err)
		return defaultLayout(), nil
	}
	return normalizeLayout(stored), nil
}

func (s *Server) handleLayout(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		layout, err := s.loadLayout(r.Context())
		if err != nil {
			http.Error(w, fmt.Sprintf("layout: %v", err), http.StatusInternalServerError)
			return
		}
		writeJSON(w, layout)

	case http.MethodPatch:
		var req layoutJSON
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
			return
		}
		norm := normalizeLayout(req)
		raw, err := json.Marshal(norm)
		if err != nil {
			http.Error(w, fmt.Sprintf("layout: %v", err), http.StatusInternalServerError)
			return
		}
		if err := s.cfg.Store.SetConfig(r.Context(), layoutConfigKey, string(raw)); err != nil {
			http.Error(w, fmt.Sprintf("layout store: %v", err), http.StatusInternalServerError)
			return
		}
		writeJSON(w, norm)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
