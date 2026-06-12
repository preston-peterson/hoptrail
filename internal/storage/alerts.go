// Storage for the v0.6 alerting tables (migration v16): incident
// state (alert_state) and the persistent notification queue
// (alert_queue). See docs/design/v0.6-alerting-design.md §3.

package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AlertState is one incident's persisted state-machine row.
// State ∈ "raising" (condition seen, sustain timer running) |
// "active" (alert sent, awaiting recovery).
type AlertState struct {
	EventType  string
	Subject    string
	State      string
	Since      int64  // unix ms condition first seen
	NotifiedAt *int64 // unix ms the alert was sent; nil while raising
}

// ListAlertStates returns every persisted incident row.
func (s *Store) ListAlertStates(ctx context.Context) ([]AlertState, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT event_type, subject, state, since, notified_at FROM alert_state`)
	if err != nil {
		return nil, fmt.Errorf("storage: list alert states: %w", err)
	}
	defer rows.Close()
	out := []AlertState{}
	for rows.Next() {
		var a AlertState
		var notified sql.NullInt64
		if err := rows.Scan(&a.EventType, &a.Subject, &a.State, &a.Since, &notified); err != nil {
			return nil, fmt.Errorf("storage: scan alert state: %w", err)
		}
		if notified.Valid {
			a.NotifiedAt = &notified.Int64
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UpsertAlertState writes one incident row, replacing any previous
// state for (event_type, subject).
func (s *Store) UpsertAlertState(ctx context.Context, a AlertState) error {
	var notified sql.NullInt64
	if a.NotifiedAt != nil {
		notified = sql.NullInt64{Int64: *a.NotifiedAt, Valid: true}
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO alert_state (event_type, subject, state, since, notified_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(event_type, subject) DO UPDATE SET
		     state = excluded.state, since = excluded.since, notified_at = excluded.notified_at`,
		a.EventType, a.Subject, a.State, a.Since, notified)
	if err != nil {
		return fmt.Errorf("storage: upsert alert state: %w", err)
	}
	return nil
}

// DeleteAlertState removes an incident row (condition fully cleared).
func (s *Store) DeleteAlertState(ctx context.Context, eventType, subject string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM alert_state WHERE event_type = ? AND subject = ?`, eventType, subject); err != nil {
		return fmt.Errorf("storage: delete alert state: %w", err)
	}
	return nil
}

// AlertQueueItem is one undelivered notification.
type AlertQueueItem struct {
	ID        int64
	CreatedAt int64 // unix ms
	Title     string
	Body      string
	Priority  string
	Attempts  int
}

// EnqueueAlert persists a notification for the sender to deliver.
func (s *Store) EnqueueAlert(ctx context.Context, title, body, priority string, createdAt time.Time) error {
	if priority == "" {
		priority = "default"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO alert_queue (created_at, title, body, priority) VALUES (?, ?, ?, ?)`,
		createdAt.UnixMilli(), title, body, priority)
	if err != nil {
		return fmt.Errorf("storage: enqueue alert: %w", err)
	}
	return nil
}

// NextQueuedAlert returns the oldest undelivered notification, or
// nil (no error) when the queue is empty.
func (s *Store) NextQueuedAlert(ctx context.Context) (*AlertQueueItem, error) {
	var a AlertQueueItem
	err := s.db.QueryRowContext(ctx,
		`SELECT id, created_at, title, body, priority, attempts
		 FROM alert_queue ORDER BY id ASC LIMIT 1`).
		Scan(&a.ID, &a.CreatedAt, &a.Title, &a.Body, &a.Priority, &a.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("storage: next queued alert: %w", err)
	}
	return &a, nil
}

// AlertQueueDepth returns how many notifications await delivery.
func (s *Store) AlertQueueDepth(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_queue`).Scan(&n); err != nil {
		return 0, fmt.Errorf("storage: alert queue depth: %w", err)
	}
	return n, nil
}

// DeleteQueuedAlert removes a delivered (or poison) notification.
func (s *Store) DeleteQueuedAlert(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM alert_queue WHERE id = ?`, id); err != nil {
		return fmt.Errorf("storage: delete queued alert: %w", err)
	}
	return nil
}

// BumpAlertAttempts increments a queued notification's retry counter.
func (s *Store) BumpAlertAttempts(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE alert_queue SET attempts = attempts + 1 WHERE id = ?`, id); err != nil {
		return fmt.Errorf("storage: bump alert attempts: %w", err)
	}
	return nil
}

// AlertHistoryEntry is one row of the append-only alert log.
type AlertHistoryEntry struct {
	ID        int64  `json:"id"`
	Ts        int64  `json:"ts"`
	EventType string `json:"event_type"`
	Subject   string `json:"subject"`
	Kind      string `json:"kind"` // alert | recovered
	Message   string `json:"message"`
}

// AppendAlertHistory records one raise/recovery.
func (s *Store) AppendAlertHistory(ctx context.Context, e AlertHistoryEntry) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO alert_history (ts, event_type, subject, kind, message) VALUES (?, ?, ?, ?, ?)`,
		e.Ts, e.EventType, e.Subject, e.Kind, e.Message)
	if err != nil {
		return fmt.Errorf("storage: append alert history: %w", err)
	}
	return nil
}

// ListAlertHistory returns up to limit entries, newest first.
func (s *Store) ListAlertHistory(ctx context.Context, limit int) ([]AlertHistoryEntry, error) {
	if limit < 1 || limit > 1000 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, ts, event_type, subject, kind, message
		   FROM alert_history ORDER BY ts DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("storage: list alert history: %w", err)
	}
	defer rows.Close()
	out := []AlertHistoryEntry{}
	for rows.Next() {
		var e AlertHistoryEntry
		if err := rows.Scan(&e.ID, &e.Ts, &e.EventType, &e.Subject, &e.Kind, &e.Message); err != nil {
			return nil, fmt.Errorf("storage: scan alert history: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteAlertHistoryOlderThan prunes old log rows (the retention
// worker calls this with a fixed 90-day cutoff — long enough for
// "what happened this quarter," bounded forever).
func (s *Store) DeleteAlertHistoryOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM alert_history WHERE ts < ?`, cutoff.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("storage: delete old alert history: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("storage: rows affected (alert_history): %w", err)
	}
	return n, nil
}

// TargetWindowStats summarizes the destination hop's behavior for one
// (probe, target) over a time window — the loss/latency alert rules'
// input. The destination is approximated as the highest TTL probed in
// the window (the pinger only probes discovered hops, so the deepest
// row is the destination once discovery has settled).
type TargetWindowStats struct {
	Sent     int
	Received int
	AvgRTTUs int64 // over received samples; 0 when none
}

func (s *Store) TargetWindowStats(ctx context.Context, probeID, target string, since, until time.Time) (TargetWindowStats, error) {
	var st TargetWindowStats
	var avg sql.NullFloat64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*),
		        COALESCE(SUM(CASE WHEN ip IS NOT NULL THEN 1 ELSE 0 END), 0),
		        AVG(CASE WHEN ip IS NOT NULL THEN rtt_us END)
		   FROM samples
		  WHERE probe_id = ? AND target = ? AND ts >= ? AND ts <= ?
		    AND ttl = (SELECT MAX(ttl) FROM samples
		                WHERE probe_id = ? AND target = ? AND ts >= ? AND ts <= ?)`,
		probeID, target, since.UnixMilli(), until.UnixMilli(),
		probeID, target, since.UnixMilli(), until.UnixMilli()).
		Scan(&st.Sent, &st.Received, &avg)
	if err != nil {
		return st, fmt.Errorf("storage: target window stats: %w", err)
	}
	if avg.Valid {
		st.AvgRTTUs = int64(avg.Float64)
	}
	return st, nil
}
