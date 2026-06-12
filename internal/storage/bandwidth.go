// Storage for the v0.4 bandwidth feature (migration v13): the general
// config key/value store and the bandwidth_samples table with its
// baseline computation. See docs/v0.4-bandwidth-design.md §5/§7.

package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
)

// ---------- config key/value store ----------

// GetConfig returns the stored value for key, with ok=false when the
// key has never been set — callers fall back to their compiled
// default, so the table only ever holds operator overrides and state
// flags.
func (s *Store) GetConfig(ctx context.Context, key string) (value string, ok bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT value FROM config WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("storage: get config %q: %w", key, err)
	}
	return value, true, nil
}

// SetConfig upserts one config row.
func (s *Store) SetConfig(ctx context.Context, key, value string) error {
	if key == "" {
		return fmt.Errorf("storage: set config: empty key")
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO config (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value); err != nil {
		return fmt.Errorf("storage: set config %q: %w", key, err)
	}
	return nil
}

// DeleteConfig removes a key (used to clear state flags like the
// derate-banner dismissal on incident resolution). Deleting an absent
// key is a no-op.
func (s *Store) DeleteConfig(ctx context.Context, key string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM config WHERE key = ?`, key); err != nil {
		return fmt.Errorf("storage: delete config %q: %w", key, err)
	}
	return nil
}

// ConfigWithPrefix returns every key/value whose key starts with
// prefix — one query for the whole bandwidth.* family.
func (s *Store) ConfigWithPrefix(ctx context.Context, prefix string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, value FROM config WHERE key LIKE ? || '%'`, prefix)
	if err != nil {
		return nil, fmt.Errorf("storage: config prefix %q: %w", prefix, err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("storage: scan config: %w", err)
		}
		out[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: config rows: %w", err)
	}
	return out, nil
}

// ---------- bandwidth samples ----------

// BandwidthSample is one speedtest run. Failed runs are stored too
// (Ok=false, Error set, throughput zeros) so the chart shows honest
// gaps and the operator can distinguish "consistently failing" from
// "real degradation".
type BandwidthSample struct {
	Ts         int64 // ms epoch, primary key
	DownMbps   float64
	UpMbps     float64
	PingMs     float64
	BytesDown  int64
	BytesUp    int64
	DurationMs int64
	ServerID   *int64
	ServerName *string
	Ok         bool
	Error      *string
	DerateFlag bool
}

// RecordBandwidthSample inserts one run.
func (s *Store) RecordBandwidthSample(ctx context.Context, smp BandwidthSample) error {
	var serverID sql.NullInt64
	if smp.ServerID != nil {
		serverID = sql.NullInt64{Int64: *smp.ServerID, Valid: true}
	}
	var serverName, errStr sql.NullString
	if smp.ServerName != nil {
		serverName = sql.NullString{String: *smp.ServerName, Valid: true}
	}
	if smp.Error != nil {
		errStr = sql.NullString{String: *smp.Error, Valid: true}
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO bandwidth_samples
		   (ts, down_mbps, up_mbps, ping_ms, bytes_down, bytes_up, duration_ms,
		    server_id, server_name, ok, error, derate_flag)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		smp.Ts, smp.DownMbps, smp.UpMbps, smp.PingMs, smp.BytesDown, smp.BytesUp,
		smp.DurationMs, serverID, serverName, smp.Ok, errStr, smp.DerateFlag,
	); err != nil {
		return fmt.Errorf("storage: record bandwidth sample: %w", err)
	}
	return nil
}

// ListBandwidthSamples returns samples in [since, until] ascending.
// The table is tiny (1-6 rows/day) so there's no bucketing — the
// design's bucket_ms param is accepted at the API layer and ignored
// until volume ever warrants it.
func (s *Store) ListBandwidthSamples(ctx context.Context, since, until int64) ([]BandwidthSample, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT ts, down_mbps, up_mbps, ping_ms, bytes_down, bytes_up, duration_ms,
		        server_id, server_name, ok, error, derate_flag
		   FROM bandwidth_samples
		  WHERE ts >= ? AND ts <= ?
		  ORDER BY ts ASC`, since, until)
	if err != nil {
		return nil, fmt.Errorf("storage: list bandwidth samples: %w", err)
	}
	defer rows.Close()
	out := []BandwidthSample{}
	for rows.Next() {
		smp, err := scanBandwidthSample(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, smp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: bandwidth rows: %w", err)
	}
	return out, nil
}

// LatestBandwidthSample returns the most recent run, or nil when no
// test has ever completed.
func (s *Store) LatestBandwidthSample(ctx context.Context) (*BandwidthSample, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT ts, down_mbps, up_mbps, ping_ms, bytes_down, bytes_up, duration_ms,
		        server_id, server_name, ok, error, derate_flag
		   FROM bandwidth_samples ORDER BY ts DESC LIMIT 1`)
	smp, err := scanBandwidthSample(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &smp, nil
}

// scanner abstracts *sql.Row / *sql.Rows for one scan shape.
type scanner interface{ Scan(dest ...any) error }

func scanBandwidthSample(r scanner) (BandwidthSample, error) {
	var smp BandwidthSample
	var serverID sql.NullInt64
	var serverName, errStr sql.NullString
	if err := r.Scan(&smp.Ts, &smp.DownMbps, &smp.UpMbps, &smp.PingMs,
		&smp.BytesDown, &smp.BytesUp, &smp.DurationMs,
		&serverID, &serverName, &smp.Ok, &errStr, &smp.DerateFlag); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return smp, err
		}
		return smp, fmt.Errorf("storage: scan bandwidth sample: %w", err)
	}
	if serverID.Valid {
		smp.ServerID = &serverID.Int64
	}
	if serverName.Valid {
		smp.ServerName = &serverName.String
	}
	if errStr.Valid {
		smp.Error = &errStr.String
	}
	return smp, nil
}

// BandwidthBaseline is the rolling "what's normal" used for derate
// detection and the chart's baseline annotation.
type BandwidthBaseline struct {
	DownMbps   float64
	UpMbps     float64
	N          int   // successful, above-floor samples in the window
	ComputedAt int64 // ms epoch (the `until` bound used)
}

// ComputeBandwidthBaseline aggregates successful samples in the
// baseline window ending at `untilMs`, excluding rows below
// floorMbps in BOTH directions (a test that can't move 10 Mbps is
// pathological — server-side trouble or a full outage — and would
// poison "normal" for weeks; design §5).
//
// metric: "median"/"p50" (identical) or "trimmed_mean" (drop the top
// and bottom 10%, mean the rest). Unknown metric falls back to
// median rather than erroring — config values arrive from the API
// and a bad row shouldn't break derate detection.
//
// Returns nil (no error) when fewer than minSamples qualify — the
// baseline-bootstrap rule (design: derate detection stays dormant
// until at least 7 successful tests; the caller passes that floor).
func (s *Store) ComputeBandwidthBaseline(ctx context.Context, metric string, days int, floorMbps float64, untilMs int64, minSamples int) (*BandwidthBaseline, error) {
	sinceMs := untilMs - int64(days)*86_400_000
	rows, err := s.db.QueryContext(ctx,
		`SELECT down_mbps, up_mbps FROM bandwidth_samples
		  WHERE ts >= ? AND ts <= ? AND ok = 1 AND down_mbps >= ? AND up_mbps >= ?
		  ORDER BY ts ASC`,
		sinceMs, untilMs, floorMbps, floorMbps)
	if err != nil {
		return nil, fmt.Errorf("storage: baseline query: %w", err)
	}
	defer rows.Close()
	var downs, ups []float64
	for rows.Next() {
		var d, u float64
		if err := rows.Scan(&d, &u); err != nil {
			return nil, fmt.Errorf("storage: baseline scan: %w", err)
		}
		downs = append(downs, d)
		ups = append(ups, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: baseline rows: %w", err)
	}
	if len(downs) < minSamples {
		return nil, nil
	}
	agg := median
	if metric == "trimmed_mean" {
		agg = trimmedMean
	}
	return &BandwidthBaseline{
		DownMbps:   agg(downs),
		UpMbps:     agg(ups),
		N:          len(downs),
		ComputedAt: untilMs,
	}, nil
}

func median(vals []float64) float64 {
	v := append([]float64(nil), vals...)
	sort.Float64s(v)
	n := len(v)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return v[n/2]
	}
	return (v[n/2-1] + v[n/2]) / 2
}

// trimmedMean drops the top and bottom 10% (rounded down, so small
// sets degrade gracefully toward a plain mean) and averages the rest.
func trimmedMean(vals []float64) float64 {
	v := append([]float64(nil), vals...)
	sort.Float64s(v)
	n := len(v)
	if n == 0 {
		return 0
	}
	trim := int(math.Floor(float64(n) * 0.1))
	v = v[trim : n-trim]
	var sum float64
	for _, x := range v {
		sum += x
	}
	return sum / float64(len(v))
}
