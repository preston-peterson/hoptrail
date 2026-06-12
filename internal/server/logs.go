// GET /api/logs (step-128): the web-UI log viewer's feed, served
// from the in-memory ring (internal/logring) teed off the daemon's
// slog pipeline. Incremental: clients pass their last-seen seq and
// get only newer records. Level filtering happens client-side — the
// payload carries the level and the ring is small.

package server

import (
	"net/http"
	"strconv"

	"github.com/preston-peterson/hoptrail/internal/logring"
)

const defaultLogLimit = 500

type logsResponse struct {
	Entries   []logring.Entry `json:"entries"`
	LatestSeq int64           `json:"latest_seq"`
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.LogRing == nil {
		http.Error(w, "log buffer not wired", http.StatusServiceUnavailable)
		return
	}
	afterSeq := int64(-1)
	if v := r.URL.Query().Get("since_seq"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			http.Error(w, "since_seq must be an integer", http.StatusBadRequest)
			return
		}
		afterSeq = n
	}
	limit := defaultLogLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 2000 {
			http.Error(w, "limit must be 1-2000", http.StatusBadRequest)
			return
		}
		limit = n
	}
	entries, latest := s.cfg.LogRing.Since(afterSeq, limit)
	writeJSON(w, logsResponse{Entries: entries, LatestSeq: latest})
}
