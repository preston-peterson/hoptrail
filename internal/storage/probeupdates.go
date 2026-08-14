// Storage methods for probe_updates (migration v19, #22): the
// per-probe update command and its lifecycle. One row per probe — a
// new command replaces whatever came before (the old row is history
// the alert log already captured if it mattered).
//
// State machine: pending (commanded; the next heartbeat reply carries
// it) → applying (probe acknowledged and started) → applied (a
// heartbeat arrived with the target version) | failed (probe reported
// an error, rolled back, or never acknowledged). deliveries counts
// how many heartbeat replies have carried the command — a probe that
// has seen it twice and never acknowledged is running a version too
// old to understand it.

package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Probe-update lifecycle states.
const (
	ProbeUpdatePending  = "pending"
	ProbeUpdateApplying = "applying"
	ProbeUpdateApplied  = "applied"
	ProbeUpdateFailed   = "failed"
)

// ProbeUpdate is one row of probe_updates.
type ProbeUpdate struct {
	ProbeID       string
	TargetVersion string // bare semver, e.g. "0.7.0"
	Arch          string
	SHA256        string // of the extracted binary the central serves
	State         string
	Error         string
	Deliveries    int
	RequestedAt   int64 // unix ms
	UpdatedAt     int64 // unix ms
}

// CommandProbeUpdate records a new update command, replacing any
// prior row for the probe.
func (s *Store) CommandProbeUpdate(ctx context.Context, pu ProbeUpdate) error {
	if pu.ProbeID == "" || pu.TargetVersion == "" || pu.SHA256 == "" || pu.Arch == "" {
		return fmt.Errorf("storage: command probe update: missing fields")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO probe_updates
		     (probe_id, target_version, arch, sha256, state, error, deliveries, requested_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, '', 0, ?, ?)`,
		pu.ProbeID, pu.TargetVersion, pu.Arch, pu.SHA256, ProbeUpdatePending, pu.RequestedAt, pu.RequestedAt)
	if err != nil {
		return fmt.Errorf("storage: command probe update: %w", err)
	}
	return nil
}

// GetProbeUpdate returns the probe's update row, nil when none exists.
func (s *Store) GetProbeUpdate(ctx context.Context, probeID string) (*ProbeUpdate, error) {
	var pu ProbeUpdate
	err := s.db.QueryRowContext(ctx,
		`SELECT probe_id, target_version, arch, sha256, state, error, deliveries, requested_at, updated_at
		 FROM probe_updates WHERE probe_id = ?`, probeID,
	).Scan(&pu.ProbeID, &pu.TargetVersion, &pu.Arch, &pu.SHA256, &pu.State, &pu.Error, &pu.Deliveries, &pu.RequestedAt, &pu.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("storage: get probe update: %w", err)
	}
	return &pu, nil
}

// ListProbeUpdates returns every update row, for the UI's state chips.
func (s *Store) ListProbeUpdates(ctx context.Context) ([]ProbeUpdate, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT probe_id, target_version, arch, sha256, state, error, deliveries, requested_at, updated_at
		 FROM probe_updates ORDER BY probe_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("storage: list probe updates: %w", err)
	}
	defer rows.Close()
	out := []ProbeUpdate{}
	for rows.Next() {
		var pu ProbeUpdate
		if err := rows.Scan(&pu.ProbeID, &pu.TargetVersion, &pu.Arch, &pu.SHA256, &pu.State, &pu.Error, &pu.Deliveries, &pu.RequestedAt, &pu.UpdatedAt); err != nil {
			return nil, fmt.Errorf("storage: scan probe update: %w", err)
		}
		out = append(out, pu)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: probe updates rows: %w", err)
	}
	return out, nil
}

// SetProbeUpdateState transitions a probe's update row.
func (s *Store) SetProbeUpdateState(ctx context.Context, probeID, state, errMsg string, at int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE probe_updates SET state = ?, error = ?, updated_at = ? WHERE probe_id = ?`,
		state, errMsg, at, probeID)
	if err != nil {
		return fmt.Errorf("storage: set probe update state: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("storage: set probe update state: no update row for %q", probeID)
	}
	return nil
}

// IncrementProbeUpdateDeliveries bumps the delivery counter and
// returns the new count.
func (s *Store) IncrementProbeUpdateDeliveries(ctx context.Context, probeID string, at int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`UPDATE probe_updates SET deliveries = deliveries + 1, updated_at = ?
		 WHERE probe_id = ?
		 RETURNING deliveries`, at, probeID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("storage: increment probe update deliveries: %w", err)
	}
	return n, nil
}

// ClearProbeUpdate removes the probe's update row (cancel, or
// tidy-up before a new command).
func (s *Store) ClearProbeUpdate(ctx context.Context, probeID string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM probe_updates WHERE probe_id = ?`, probeID); err != nil {
		return fmt.Errorf("storage: clear probe update: %w", err)
	}
	return nil
}
