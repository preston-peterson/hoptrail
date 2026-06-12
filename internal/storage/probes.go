// Storage methods for the v0.3 per-probe tables (migration v11):
// probes (registered agents, heartbeat-upserted), path_snapshots
// (most recent path per probe+target, overwritten on ingest), and
// ingest_log (batch_id dedup for at-least-once delivery).
//
// See docs/v0.3-protocol-design.md §4 for the schema rationale.

package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// LocalProbeID is the reserved probe identifier for the central
// daemon's own on-host probe engine. Existing v0.2 rows were
// backfilled with it at migration v11, and the in-process sink's
// inserts pick it up via the column DEFAULT. Agents may not register
// with this name (enforced at the ingest layer, not here).
const LocalProbeID = "local"

// Probe is one row of the probes table — a registered agent as last
// seen by its heartbeats. Version and StartedAt are nullable in the
// schema (an agent row could in principle exist before its first
// full heartbeat); Label is operator-set display metadata that the
// heartbeat path never touches.
type Probe struct {
	ProbeID    string
	Version    *string
	StartedAt  *int64 // unix ms; agent process start time
	LastSeenAt int64  // unix ms; central's clock at last heartbeat
	Label      *string
	// LastIP is the source address of the probe's last heartbeat as
	// seen by the central (step-142) — NAT/VPN-aware "where is this
	// probe reaching me from," not the probe's own idea of its IP.
	LastIP *string
}

// UpsertProbeHeartbeat records a heartbeat from probeID. First
// heartbeat inserts the row; subsequent ones update version,
// started_at, and last_seen_at while preserving the operator-set
// label. Re-registration with a new started_at (agent restarted) is
// just another upsert — idempotent by design.
func (s *Store) UpsertProbeHeartbeat(ctx context.Context, probeID, version string, startedAt, lastSeenAt time.Time, lastIP string) error {
	if probeID == "" {
		return fmt.Errorf("storage: upsert probe heartbeat: empty probe_id")
	}
	var ip sql.NullString
	if lastIP != "" {
		ip = sql.NullString{String: lastIP, Valid: true}
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO probes (probe_id, version, started_at, last_seen_at, last_ip)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(probe_id) DO UPDATE SET
		     version      = excluded.version,
		     started_at   = excluded.started_at,
		     last_seen_at = excluded.last_seen_at,
		     last_ip      = COALESCE(excluded.last_ip, probes.last_ip)`,
		probeID, version, startedAt.UnixMilli(), lastSeenAt.UnixMilli(), ip,
	)
	if err != nil {
		return fmt.Errorf("storage: upsert probe heartbeat: %w", err)
	}
	return nil
}

// ListProbes returns every registered probe, ordered by probe_id for
// a stable UI listing. Offline detection (last_seen_at older than
// N× heartbeat interval) is the caller's policy, not storage's.
func (s *Store) ListProbes(ctx context.Context) ([]Probe, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT probe_id, version, started_at, last_seen_at, label, last_ip
		 FROM probes ORDER BY probe_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("storage: list probes: %w", err)
	}
	defer rows.Close()

	out := []Probe{}
	for rows.Next() {
		var p Probe
		var version, label, lastIP sql.NullString
		var startedAt sql.NullInt64
		if err := rows.Scan(&p.ProbeID, &version, &startedAt, &p.LastSeenAt, &label, &lastIP); err != nil {
			return nil, fmt.Errorf("storage: scan probe: %w", err)
		}
		if version.Valid {
			p.Version = &version.String
		}
		if startedAt.Valid {
			p.StartedAt = &startedAt.Int64
		}
		if label.Valid {
			p.Label = &label.String
		}
		if lastIP.Valid {
			p.LastIP = &lastIP.String
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: probes rows: %w", err)
	}
	return out, nil
}

// PathSnapshot is one row of the path_snapshots table — the most
// recent path a probe reported for a target. HopsJSON is stored
// opaquely (the same hopJSON wire shape the agent sent); storage
// doesn't parse it.
type PathSnapshot struct {
	ProbeID   string
	Target    string
	Ts        int64 // unix ms; agent-local snapshot time
	HopCount  int
	TargetTTL int
	HopsJSON  string
}

// UpsertPathSnapshot stores the current path for (probeID, target),
// replacing any previous snapshot — the table holds current state,
// not history.
func (s *Store) UpsertPathSnapshot(ctx context.Context, snap PathSnapshot) error {
	if snap.ProbeID == "" || snap.Target == "" {
		return fmt.Errorf("storage: upsert path snapshot: empty probe_id or target")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO path_snapshots (probe_id, target, ts, hop_count, target_ttl, hops_json)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(probe_id, target) DO UPDATE SET
		     ts         = excluded.ts,
		     hop_count  = excluded.hop_count,
		     target_ttl = excluded.target_ttl,
		     hops_json  = excluded.hops_json`,
		snap.ProbeID, snap.Target, snap.Ts, snap.HopCount, snap.TargetTTL, snap.HopsJSON,
	)
	if err != nil {
		return fmt.Errorf("storage: upsert path snapshot: %w", err)
	}
	return nil
}

// GetPathSnapshot returns the stored snapshot for (probeID, target),
// or nil (no error) when the probe hasn't reported a path for that
// target yet — absence is an expected state, not a failure.
func (s *Store) GetPathSnapshot(ctx context.Context, probeID, target string) (*PathSnapshot, error) {
	var snap PathSnapshot
	err := s.db.QueryRowContext(ctx,
		`SELECT probe_id, target, ts, hop_count, target_ttl, hops_json
		 FROM path_snapshots WHERE probe_id = ? AND target = ?`,
		probeID, target,
	).Scan(&snap.ProbeID, &snap.Target, &snap.Ts, &snap.HopCount, &snap.TargetTTL, &snap.HopsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("storage: get path snapshot: %w", err)
	}
	return &snap, nil
}

// insertIngestLogSQL is the dedup gate statement, shared by
// RecordIngestBatch (standalone) and IngestBatch (inside its
// transaction). INSERT-OR-IGNORE makes check-and-record one atomic
// statement, so two concurrent retries of the same batch can't both
// see "new".
const insertIngestLogSQL = `INSERT OR IGNORE INTO ingest_log (batch_id, probe_id, received_at)
	 VALUES (?, ?, ?)`

// RecordIngestBatch is the at-least-once dedup gate. The first call
// for a batch_id records it and returns true ("new batch — process
// it"); a repeat call returns false ("duplicate — ack it but write
// nothing").
func (s *Store) RecordIngestBatch(ctx context.Context, batchID, probeID string, receivedAt time.Time) (bool, error) {
	if batchID == "" {
		return false, fmt.Errorf("storage: record ingest batch: empty batch_id")
	}
	res, err := s.db.ExecContext(ctx, insertIngestLogSQL,
		batchID, probeID, receivedAt.UnixMilli())
	if err != nil {
		return false, fmt.Errorf("storage: record ingest batch: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("storage: rows affected (ingest_log): %w", err)
	}
	return n == 1, nil
}

// IngestSample is one sample row as delivered by a remote agent —
// already in storage units (ms timestamps, µs RTTs). Conversion from
// the wire's float rtt_ms happens at the HTTP boundary, matching the
// project's "round at the wire edge" rule (lesson #13, in reverse).
type IngestSample struct {
	Target string
	TTL    int
	Ts     int64 // unix ms, agent-local clock
	IP     *string
	RTTUs  int64
}

// IngestRouteChange is one route-change row from a remote agent.
type IngestRouteChange struct {
	Target string
	TTL    int
	Ts     int64 // unix ms, agent-local clock
	OldIP  *string
	NewIP  string
}

// IngestBatch writes one agent-delivered batch: the dedup record and
// every sample/route-change row commit in a single transaction, so a
// crash can never record a batch as ingested without its data (the
// agent's retry would be deduped and the samples silently lost).
// Returns false when the batch_id was already ingested — the caller
// acks the duplicate without writing anything.
func (s *Store) IngestBatch(ctx context.Context, probeID, batchID string, receivedAt time.Time, samples []IngestSample, changes []IngestRouteChange) (bool, error) {
	if probeID == "" {
		return false, fmt.Errorf("storage: ingest batch: empty probe_id")
	}
	if batchID == "" {
		return false, fmt.Errorf("storage: ingest batch: empty batch_id")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("storage: ingest batch: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	res, err := tx.ExecContext(ctx, insertIngestLogSQL,
		batchID, probeID, receivedAt.UnixMilli())
	if err != nil {
		return false, fmt.Errorf("storage: ingest batch: record batch: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("storage: ingest batch: rows affected: %w", err)
	}
	if n == 0 {
		// Duplicate delivery (agent retry after a lost ack). Nothing
		// to write; the rollback discards the no-op transaction.
		return false, nil
	}

	if len(samples) > 0 {
		stmt, err := tx.PrepareContext(ctx,
			`INSERT INTO samples (probe_id, target, ttl, ts, ip, rtt_us) VALUES (?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return false, fmt.Errorf("storage: ingest batch: prepare samples: %w", err)
		}
		defer stmt.Close()
		for _, smp := range samples {
			var ip sql.NullString
			if smp.IP != nil {
				ip = sql.NullString{String: *smp.IP, Valid: true}
			}
			if _, err := stmt.ExecContext(ctx, probeID, smp.Target, smp.TTL, smp.Ts, ip, smp.RTTUs); err != nil {
				return false, fmt.Errorf("storage: ingest batch: insert sample (ttl=%d): %w", smp.TTL, err)
			}
		}
	}

	if len(changes) > 0 {
		stmt, err := tx.PrepareContext(ctx,
			`INSERT INTO route_changes (probe_id, target, ttl, ts, old_ip, new_ip) VALUES (?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return false, fmt.Errorf("storage: ingest batch: prepare route_changes: %w", err)
		}
		defer stmt.Close()
		for _, rc := range changes {
			var old sql.NullString
			if rc.OldIP != nil {
				old = sql.NullString{String: *rc.OldIP, Valid: true}
			}
			if _, err := stmt.ExecContext(ctx, probeID, rc.Target, rc.TTL, rc.Ts, old, rc.NewIP); err != nil {
				return false, fmt.Errorf("storage: ingest batch: insert route_change (ttl=%d): %w", rc.TTL, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("storage: ingest batch: commit: %w", err)
	}
	return true, nil
}

// DeleteIngestLogOlderThan removes dedup rows received strictly
// before the cutoff. The dedup window only needs to cover the
// agent-side retry horizon — an agent still retrying a batch after
// 24h has a real problem — so the retention worker calls this with
// a fixed 24h cutoff, independent of the operator's retention_days.
func (s *Store) DeleteIngestLogOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM ingest_log WHERE received_at < ?`, cutoff.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("storage: delete old ingest_log: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("storage: rows affected (ingest_log): %w", err)
	}
	return n, nil
}
