package storage

import (
	"context"
	"fmt"
)

// DBSizeSample is one point in the database-size time series.
type DBSizeSample struct {
	Ts    int64 // unix ms
	Bytes int64 // main database file size at that moment
}

// AppendDBSizeSample records the database file size at ts. Called once
// per retention sweep (hourly); the capacity monitor reads the series
// to estimate growth. Append-only; pruning is a separate fixed-window
// delete on the retention path.
func (s *Store) AppendDBSizeSample(ctx context.Context, ts, bytes int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO db_size_samples (ts, bytes) VALUES (?, ?)`, ts, bytes)
	if err != nil {
		return fmt.Errorf("storage: append db_size_sample: %w", err)
	}
	return nil
}

// DBSizeSamples returns size samples with ts >= since, oldest first.
func (s *Store) DBSizeSamples(ctx context.Context, since int64) ([]DBSizeSample, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT ts, bytes FROM db_size_samples WHERE ts >= ? ORDER BY ts ASC`, since)
	if err != nil {
		return nil, fmt.Errorf("storage: query db_size_samples: %w", err)
	}
	defer rows.Close()
	var out []DBSizeSample
	for rows.Next() {
		var smp DBSizeSample
		if err := rows.Scan(&smp.Ts, &smp.Bytes); err != nil {
			return nil, fmt.Errorf("storage: scan db_size_sample: %w", err)
		}
		out = append(out, smp)
	}
	return out, rows.Err()
}

// DeleteDBSizeSamplesOlderThan prunes the series to a fixed window. The
// growth estimate only needs a couple of weeks of history; keeping more
// is pointless churn. Returns the number of rows removed.
func (s *Store) DeleteDBSizeSamplesOlderThan(ctx context.Context, cutoffMs int64) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM db_size_samples WHERE ts < ?`, cutoffMs)
	if err != nil {
		return 0, fmt.Errorf("storage: prune db_size_samples: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
