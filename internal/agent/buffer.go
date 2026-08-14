// The partition-recovery spill buffer (§8): when central is
// unreachable, marshaled batches land here instead of being lost;
// the flush loop drains oldest-first when central comes back. A
// deliberately tiny single-table SQLite file — not internal/storage,
// which is the central's authoritative schema.

package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	// Registers the "sqlite3" driver; already a project dependency.
	_ "github.com/mattn/go-sqlite3"
)

// Buffer is the agent's local spill store. Batches are stored as
// their fully-marshaled wire payloads, so draining is a dumb
// read-POST-delete loop with no re-encoding — the batch_id inside
// the payload stays stable across retries, which is what makes
// central's dedup work.
type Buffer struct {
	db       *sql.DB
	maxBytes int64
}

// OpenBuffer opens (creating if needed) the spill buffer at path.
// maxSizeMB bounds the total payload bytes retained; when an Add
// would exceed it, oldest batches are dropped to make room (§8 —
// losing the oldest data beats wedging the agent).
func OpenBuffer(path string, maxSizeMB int) (*Buffer, error) {
	if maxSizeMB < 1 {
		return nil, fmt.Errorf("probe: buffer max_size_mb must be >= 1, got %d", maxSizeMB)
	}
	if dir := filepath.Dir(path); dir != "." && dir != "/" && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("probe: create buffer directory %q: %w", dir, err)
		}
	}
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL")
	if err != nil {
		return nil, fmt.Errorf("probe: open buffer %q: %w", path, err)
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS pending_batches (
			batch_id   TEXT NOT NULL PRIMARY KEY,
			created_at INTEGER NOT NULL,
			payload    BLOB NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_pending_created ON pending_batches(created_at);
	`); err != nil {
		db.Close()
		return nil, fmt.Errorf("probe: init buffer schema: %w", err)
	}
	return &Buffer{db: db, maxBytes: int64(maxSizeMB) << 20}, nil
}

// Close releases the buffer's database handle.
func (b *Buffer) Close() error {
	if b.db == nil {
		return nil
	}
	err := b.db.Close()
	b.db = nil
	return err
}

// Add stores a batch payload, evicting oldest batches if the size
// bound would be exceeded. Returns how many batches were evicted so
// the caller can WARN — silent data loss is the one thing this
// buffer must never do quietly.
func (b *Buffer) Add(ctx context.Context, batchID string, createdAt int64, payload []byte) (evicted int64, err error) {
	if int64(len(payload)) > b.maxBytes {
		return 0, fmt.Errorf("probe: batch %s (%d bytes) exceeds the whole buffer bound (%d)", batchID, len(payload), b.maxBytes)
	}
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("probe: buffer add: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	if _, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO pending_batches (batch_id, created_at, payload) VALUES (?, ?, ?)`,
		batchID, createdAt, payload); err != nil {
		return 0, fmt.Errorf("probe: buffer add: insert: %w", err)
	}

	// Evict oldest-first until under the bound. One row at a time is
	// fine: eviction is rare (sustained partition at full buffer) and
	// batches are sizeable, so the loop runs a handful of times at
	// most per add.
	for {
		var total sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			`SELECT SUM(LENGTH(payload)) FROM pending_batches`).Scan(&total); err != nil {
			return evicted, fmt.Errorf("probe: buffer add: size check: %w", err)
		}
		if !total.Valid || total.Int64 <= b.maxBytes {
			break
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM pending_batches WHERE batch_id =
			   (SELECT batch_id FROM pending_batches ORDER BY created_at ASC, batch_id ASC LIMIT 1)`); err != nil {
			return evicted, fmt.Errorf("probe: buffer add: evict: %w", err)
		}
		evicted++
	}

	if err := tx.Commit(); err != nil {
		return evicted, fmt.Errorf("probe: buffer add: commit: %w", err)
	}
	return evicted, nil
}

// Oldest returns the oldest pending batch, or ok=false when the
// buffer is empty.
func (b *Buffer) Oldest(ctx context.Context) (batchID string, payload []byte, ok bool, err error) {
	row := b.db.QueryRowContext(ctx,
		`SELECT batch_id, payload FROM pending_batches ORDER BY created_at ASC, batch_id ASC LIMIT 1`)
	if err := row.Scan(&batchID, &payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil, false, nil
		}
		return "", nil, false, fmt.Errorf("probe: buffer oldest: %w", err)
	}
	return batchID, payload, true, nil
}

// Delete removes a delivered (or poison) batch.
func (b *Buffer) Delete(ctx context.Context, batchID string) error {
	if _, err := b.db.ExecContext(ctx,
		`DELETE FROM pending_batches WHERE batch_id = ?`, batchID); err != nil {
		return fmt.Errorf("probe: buffer delete: %w", err)
	}
	return nil
}

// Count returns the number of pending batches. Used for logging and
// tests.
func (b *Buffer) Count(ctx context.Context) (int, error) {
	var n int
	if err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_batches`).Scan(&n); err != nil {
		return 0, fmt.Errorf("probe: buffer count: %w", err)
	}
	return n, nil
}
