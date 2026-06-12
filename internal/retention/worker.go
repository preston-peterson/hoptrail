// Package retention runs a background worker that enforces the
// storage.retention_days policy by periodically deleting rows older
// than the cutoff from the samples and route_changes tables.
//
// The worker has a simple lifecycle:
//  1. Run once immediately at startup. The daemon may have been off
//     for hours or days; the database can have far more than the
//     configured retention window worth of rows. An immediate cleanup
//     is correct, and small relative to the wall-clock cost of just
//     getting the daemon started.
//  2. Run again every Interval (default 1h). Hourly is a balance: too
//     fast wastes I/O on a mostly-no-op delete, too slow lets the
//     over-retention grow.
//
// The rdns table is deliberately excluded from retention (it's a
// cache, not a time-series). See internal/storage/retention.go for
// the rationale.

package retention

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/preston-peterson/hoptrail/internal/storage"
)

// Config controls the worker's cadence and policy.
type Config struct {
	// RetentionDays is how many days of data to keep. Rows with ts
	// strictly less than (now - RetentionDays) are deleted. Must be
	// >= 1 (validation in internal/config ensures this).
	RetentionDays int

	// Interval is how often the worker re-runs after the initial
	// startup sweep. One hour is a sensible default; shorter means
	// less over-retention drift, longer means less DB churn.
	Interval time.Duration
}

// DefaultConfig returns the production defaults. The RetentionDays
// is set by the caller from cfg.Storage.RetentionDays.
func DefaultConfig(retentionDays int) Config {
	return Config{
		RetentionDays: retentionDays,
		Interval:      1 * time.Hour,
	}
}

// Worker is the retention enforcer. Construct with New, then call
// Run in a goroutine to start it.
type Worker struct {
	cfg   Config
	store *storage.Store
	log   *slog.Logger

	// now is injected for tests so we can fast-forward through the
	// cutoff math without manipulating system time.
	now func() time.Time
}

// New constructs a Worker. The logger may be nil; a discard logger
// is used in that case.
func New(cfg Config, store *storage.Store, log *slog.Logger) *Worker {
	if log == nil {
		log = slog.New(slog.NewTextHandler(nopWriter{}, nil))
	}
	return &Worker{
		cfg:   cfg,
		store: store,
		log:   log,
		now:   time.Now,
	}
}

// Run starts the worker loop and blocks until ctx is canceled.
// Performs one delete pass immediately, then re-runs every
// cfg.Interval. Storage errors are logged but do not terminate the
// loop — a transient SQLite busy or context error shouldn't take
// down retention; the next interval is likely to succeed.
func (w *Worker) Run(ctx context.Context) {
	w.log.Info("retention: worker started",
		"retention_days", w.cfg.RetentionDays,
		"interval", w.cfg.Interval)

	// Initial sweep — clears anything stale that built up while the
	// daemon was off, before the steady-state hourly cadence takes
	// over.
	w.runOnce(ctx)

	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			w.log.Info("retention: worker stopped")
			return
		case <-ticker.C:
			w.runOnce(ctx)
		}
	}
}

// EffectiveDays returns the live retention policy: the operator-set
// SQLite config row when present (step-110, settings-panel editable),
// else the yaml value the worker was constructed with. Read per
// sweep so a PATCH applies without a restart.
func (w *Worker) EffectiveDays(ctx context.Context) int {
	if v, ok, err := w.store.GetConfig(ctx, "retention.days"); err == nil && ok {
		if n, perr := strconv.Atoi(v); perr == nil && n >= 1 && n <= 3650 {
			return n
		}
	}
	return w.cfg.RetentionDays
}

// runOnce performs a single retention sweep across both time-series
// tables. Failure on one table is logged but does not prevent the
// other from being processed — partial cleanup is better than none.
func (w *Worker) runOnce(ctx context.Context) {
	now := w.now()
	cutoff := now.Add(-time.Duration(w.EffectiveDays(ctx)) * 24 * time.Hour)

	samples, err := w.store.DeleteSamplesOlderThan(ctx, cutoff)
	if err != nil {
		w.log.Error("retention: delete samples", "err", err)
		// Fall through to route_changes — independent failure domain.
	}

	routeChanges, err := w.store.DeleteRouteChangesOlderThan(ctx, cutoff)
	if err != nil {
		w.log.Error("retention: delete route_changes", "err", err)
	}

	// Alert history: fixed 90-day window, independent of
	// retention_days — it's an operator log, not telemetry.
	alertHist, err := w.store.DeleteAlertHistoryOlderThan(ctx, now.Add(-90*24*time.Hour))
	if err != nil {
		w.log.Error("retention: delete alert_history", "err", err)
	}
	_ = alertHist

	// The ingest dedup log has its own fixed 24h window, independent
	// of retention_days — it only needs to cover the agent-side retry
	// horizon, not the operator's data-history policy.
	ingestLog, err := w.store.DeleteIngestLogOlderThan(ctx, now.Add(-24*time.Hour))
	if err != nil {
		w.log.Error("retention: delete ingest_log", "err", err)
	}

	// Log even when both counts are zero so operators can confirm the
	// worker is running. The first sweep after an upgrade may have
	// large counts; subsequent sweeps on a steady-state daemon
	// typically show small numbers (the past hour's worth of new
	// arrivals that aged out of the window).
	w.log.Info("retention: sweep complete",
		"cutoff", cutoff.Format(time.RFC3339),
		"samples_deleted", samples,
		"route_changes_deleted", routeChanges,
		"ingest_log_deleted", ingestLog)
}

// nopWriter is an io.Writer that discards everything. Used when the
// caller passes a nil logger to New.
type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
