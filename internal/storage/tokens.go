// Storage methods for the probe_tokens table (migration v14, step-120):
// UI-minted bearer tokens for the /api/ingest/* surface, replacing the
// yaml-edit-and-restart flow. The server's auth layer treats the union
// of yaml probes.tokens and this table as the accepted set, so adding
// or revoking a token here takes effect on the next request with no
// restart.
//
// See docs/design/v0.5-simplicity-design.md §5.1.

package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ProbeToken is one row of the probe_tokens table. Token is the full
// secret — callers decide how much of it to expose (the list API
// surfaces only a prefix; the full value leaves storage exactly once,
// in the creation response).
type ProbeToken struct {
	ID         int64
	Token      string
	Name       string
	CreatedAt  int64  // unix ms
	LastUsedAt *int64 // unix ms; nil until the first heartbeat auths with it
}

// InsertProbeToken persists a freshly generated token. Generation
// (crypto/rand) is the caller's job — storage only persists. The
// UNIQUE constraint on token makes an (astronomically unlikely)
// collision a loud error rather than a silent overwrite.
func (s *Store) InsertProbeToken(ctx context.Context, token, name string, createdAt time.Time) (int64, error) {
	if token == "" || name == "" {
		return 0, fmt.Errorf("storage: insert probe token: empty token or name")
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO probe_tokens (token, name, created_at) VALUES (?, ?, ?)`,
		token, name, createdAt.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("storage: insert probe token: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("storage: insert probe token id: %w", err)
	}
	return id, nil
}

// ListProbeTokens returns every token row, newest first.
func (s *Store) ListProbeTokens(ctx context.Context) ([]ProbeToken, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, token, name, created_at, last_used_at
		 FROM probe_tokens ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("storage: list probe tokens: %w", err)
	}
	defer rows.Close()

	out := []ProbeToken{}
	for rows.Next() {
		var t ProbeToken
		var lastUsed sql.NullInt64
		if err := rows.Scan(&t.ID, &t.Token, &t.Name, &t.CreatedAt, &lastUsed); err != nil {
			return nil, fmt.Errorf("storage: scan probe token: %w", err)
		}
		if lastUsed.Valid {
			t.LastUsedAt = &lastUsed.Int64
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: probe tokens rows: %w", err)
	}
	return out, nil
}

// DeleteProbeToken revokes (deletes) a token by id. Returns false
// when no such row existed — the caller maps that to 404.
func (s *Store) DeleteProbeToken(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM probe_tokens WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("storage: delete probe token: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("storage: rows affected (probe_tokens): %w", err)
	}
	return n > 0, nil
}

// ProbeTokenSecrets returns just the token strings — the auth layer's
// accepted set. Called per ingest request; at homelab scale (one
// ~5s batch per probe against a table of a handful of rows) a query
// per request is well under the noise floor.
func (s *Store) ProbeTokenSecrets(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT token FROM probe_tokens`)
	if err != nil {
		return nil, fmt.Errorf("storage: probe token secrets: %w", err)
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("storage: scan probe token secret: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: probe token secrets rows: %w", err)
	}
	return out, nil
}

// TokenBinding pairs a UI-minted token with the probe_id it is
// authorized for (the token's name, validated as a probe_id at mint).
type TokenBinding struct {
	Token string
	Name  string
}

// ProbeTokenBindings returns token→authorized-probe_id pairs for the
// auth layer (step-170, audit #10): a token may only push as the probe
// it was minted for. Called per ingest request; table is a handful of
// rows at homelab scale.
func (s *Store) ProbeTokenBindings(ctx context.Context) ([]TokenBinding, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT token, name FROM probe_tokens`)
	if err != nil {
		return nil, fmt.Errorf("storage: probe token bindings: %w", err)
	}
	defer rows.Close()
	out := []TokenBinding{}
	for rows.Next() {
		var b TokenBinding
		if err := rows.Scan(&b.Token, &b.Name); err != nil {
			return nil, fmt.Errorf("storage: scan token binding: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: token bindings rows: %w", err)
	}
	return out, nil
}

// TouchProbeToken stamps last_used_at for the row holding this exact
// token. A no-op (no error) for yaml-configured tokens, which have no
// row. Heartbeat-cadence only — see the migration comment.
func (s *Store) TouchProbeToken(ctx context.Context, token string, usedAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE probe_tokens SET last_used_at = ? WHERE token = ?`,
		usedAt.UnixMilli(), token)
	if err != nil {
		return fmt.Errorf("storage: touch probe token: %w", err)
	}
	return nil
}

// DeleteProbe forgets a registered probe: its probes row, its path
// snapshots, and any tabs pointed at it are reset to the local probe
// — all in one transaction. Its samples and route_changes are left to
// age out via retention (deleting them inline could be a multi-GB
// scan). Returns false when the probe wasn't registered.
//
// Deliberately does NOT touch probe_tokens: tokens aren't bound to a
// probe_id, and a still-valid token means the probe re-registers on
// its next heartbeat. The UI warns to revoke the token first.
func (s *Store) DeleteProbe(ctx context.Context, probeID string) (bool, error) {
	if probeID == "" || probeID == LocalProbeID {
		return false, fmt.Errorf("storage: delete probe: invalid probe_id %q", probeID)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("storage: delete probe: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	res, err := tx.ExecContext(ctx, `DELETE FROM probes WHERE probe_id = ?`, probeID)
	if err != nil {
		return false, fmt.Errorf("storage: delete probe row: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("storage: rows affected (probes): %w", err)
	}
	if n == 0 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM path_snapshots WHERE probe_id = ?`, probeID); err != nil {
		return false, fmt.Errorf("storage: delete probe snapshots: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE tabs SET probe_id = ? WHERE probe_id = ?`, LocalProbeID, probeID); err != nil {
		return false, fmt.Errorf("storage: reset tabs probe_id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("storage: delete probe: commit: %w", err)
	}
	return true, nil
}
