// Storage methods for retention.
//
// Two tables accumulate over time and need bounded retention:
//   - samples       (per-probe-per-TTL rows; the hot table)
//   - route_changes (per-route-change rows; low-volume but still timestamped)
//
// The rdns table is deliberately excluded: it's a cache bounded by unique
// IPs ever observed, not a time-series. Removing rdns rows would mean
// re-resolving the same IPs every time they reappear, which is wasteful
// and noisy. The rdns table grows slowly even over months.

package storage

import (
	"context"
	"fmt"
	"time"
)

// deleteBatchRows caps how many sample rows one DELETE transaction may
// touch. A var, not a const, so tests can shrink it to exercise the
// batching loop without seeding 50k rows.
var deleteBatchRows = 50_000

// DeleteSamplesOlderThan removes sample rows with ts strictly less than
// the given cutoff, in batches of deleteBatchRows per transaction.
// Returns the total number of rows deleted.
//
// Batched (2026-08 WAL incident): at 1-second probing across dozens of
// hops, an hourly sweep deletes six figures of rows, and a single
// DELETE transaction pushes every touched page into the WAL while
// holding the write lock throughout. Bounded batches keep each
// transaction's WAL churn and lock hold small; the worker's TRUNCATE
// checkpoint after the sweep folds the churn back out of the log.
//
// Note that DELETE writes to the WAL and the database file does not
// shrink — SQLite reuses freed pages as new rows arrive, so the file
// stabilizes at "active retention window worth of data" over time.
// VACUUM would reclaim the space, but it's a blocking operation we
// don't run automatically.
func (s *Store) DeleteSamplesOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	const q = `DELETE FROM samples WHERE rowid IN
		(SELECT rowid FROM samples WHERE ts < ? LIMIT ?)`
	var total int64
	for {
		res, err := s.db.ExecContext(ctx, q, cutoff.UnixMilli(), deleteBatchRows)
		if err != nil {
			return total, fmt.Errorf("storage: delete old samples: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("storage: rows affected (samples): %w", err)
		}
		total += n
		if n < int64(deleteBatchRows) {
			return total, nil
		}
	}
}

// DeleteRouteChangesOlderThan removes route_change rows with ts strictly
// less than the given cutoff. Returns the number of rows deleted.
//
// Route-change rows are much lower volume than samples (one per real
// route change, not one per probe), so this delete is typically fast
// even with a large retention window.
func (s *Store) DeleteRouteChangesOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	const q = `DELETE FROM route_changes WHERE ts < ?`
	res, err := s.db.ExecContext(ctx, q, cutoff.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("storage: delete old route_changes: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("storage: rows affected (route_changes): %w", err)
	}
	return n, nil
}

// DeleteTargetHistory wipes a target's samples and route changes
// across ALL probes, in one transaction (step-111 "start new"
// semantics — operator chose clean deletion over generation-keying).
// Annotations are deliberately kept: they're operator notes, not
// measurements. Returns deleted row counts.
func (s *Store) DeleteTargetHistory(ctx context.Context, target string) (samples, routeChanges int64, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("storage: target history wipe: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	res, err := tx.ExecContext(ctx, `DELETE FROM samples WHERE target = ?`, target)
	if err != nil {
		return 0, 0, fmt.Errorf("storage: target history wipe: samples: %w", err)
	}
	samples, _ = res.RowsAffected()
	res, err = tx.ExecContext(ctx, `DELETE FROM route_changes WHERE target = ?`, target)
	if err != nil {
		return 0, 0, fmt.Errorf("storage: target history wipe: route_changes: %w", err)
	}
	routeChanges, _ = res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("storage: target history wipe: commit: %w", err)
	}
	return samples, routeChanges, nil
}

// TargetStats reports whether (and how much) history exists for a
// target across all probes — drives the resume-vs-new prompt.
func (s *Store) TargetStats(ctx context.Context, target string) (count int64, oldestTs, newestTs int64, err error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(MIN(ts),0), COALESCE(MAX(ts),0) FROM samples WHERE target = ?`, target)
	if err := row.Scan(&count, &oldestTs, &newestTs); err != nil {
		return 0, 0, 0, fmt.Errorf("storage: target stats: %w", err)
	}
	return count, oldestTs, newestTs, nil
}
