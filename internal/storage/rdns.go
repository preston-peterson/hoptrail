// Storage methods for the rdns table.
//
// The rdns table caches reverse-DNS lookups. The rdns worker (in
// internal/rdns) populates it; the HTTP handlers read from it to
// surface hostnames alongside IPs in the path response.
//
// Schema (from migration v1):
//   CREATE TABLE rdns (
//       ip          TEXT NOT NULL PRIMARY KEY,
//       hostname    TEXT,
//       resolved_at INTEGER NOT NULL
//   );
//
// Three states for an IP:
//   1. No row in rdns        → never attempted a lookup
//   2. Row, hostname NULL    → lookup attempted, no PTR record (or error)
//   3. Row, hostname non-NULL → got a name
//
// The worker only attempts state-1 IPs. The handler treats states 1
// and 2 the same — both render as "no hostname available."

package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// UnresolvedIPs returns up to limit distinct IPs that appear in the
// samples table but have no corresponding row in the rdns table.
//
// This is the rdns worker's input queue: every poll cycle, the worker
// asks "which IPs have I never tried to resolve?", attempts each one,
// and writes the result back via UpsertRDNS — which means subsequent
// calls to this method skip those IPs whether or not the lookup
// succeeded.
//
// Returns IPs in no particular order. Callers should treat the slice
// as a set.
func (s *Store) UnresolvedIPs(ctx context.Context, limit int) ([]string, error) {
	// LEFT JOIN + WHERE r.ip IS NULL is the standard anti-join pattern.
	// SQLite optimizes this better than NOT IN (subquery) on large
	// samples tables, since NOT IN can't use the rdns primary-key index
	// as efficiently. The DISTINCT collapses the per-sample rows down
	// to one entry per IP before the LIMIT bites.
	const q = `
		SELECT DISTINCT s.ip
		FROM samples s
		LEFT JOIN rdns r ON r.ip = s.ip
		WHERE s.ip IS NOT NULL
		  AND r.ip IS NULL
		LIMIT ?`

	rows, err := s.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("storage: query unresolved IPs: %w", err)
	}
	defer rows.Close()

	var ips []string
	for rows.Next() {
		var ip string
		if err := rows.Scan(&ip); err != nil {
			return nil, fmt.Errorf("storage: scan unresolved IP: %w", err)
		}
		ips = append(ips, ip)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: iterate unresolved IPs: %w", err)
	}
	return ips, nil
}

// UpsertRDNS writes a lookup result to the rdns table. The hostname
// can be empty — that's how the worker records "lookup attempted, no
// result" so it doesn't keep re-querying the same dead-end IP.
//
// An empty hostname is stored as SQL NULL (not the empty string) so
// the schema's "hostname IS NULL" predicate distinguishes "attempted
// and got nothing" from "got a non-empty name."
//
// INSERT OR REPLACE rather than INSERT: an existing row gets its
// resolved_at refreshed and (if a TTL re-resolve happens in the
// future) its hostname updated. For step-14 the worker never
// re-resolves, so this is just defensive — but cheap and correct.
func (s *Store) UpsertRDNS(ctx context.Context, ip, hostname string) error {
	var hn any = hostname
	if hostname == "" {
		hn = nil
	}
	const q = `
		INSERT OR REPLACE INTO rdns (ip, hostname, resolved_at)
		VALUES (?, ?, ?)`
	_, err := s.db.ExecContext(ctx, q, ip, hn, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("storage: upsert rdns for %s: %w", ip, err)
	}
	return nil
}

// LookupHostnames returns hostnames for the given IPs as a map. IPs
// not present in the rdns table, or present with a NULL hostname,
// are absent from the returned map — the caller treats absence as
// "no hostname available." A nil or empty input slice returns an
// empty map without hitting the DB.
//
// This is the read path used by the /api/path handler: the handler
// has ~10 IPs (one per hop) and needs hostnames inline in the JSON
// response, so it calls this once and merges results into the hop
// list. The IN-clause is built dynamically; for the expected sizes
// (single-digit to low double-digit) this is fine.
func (s *Store) LookupHostnames(ctx context.Context, ips []string) (map[string]string, error) {
	result := make(map[string]string, len(ips))
	if len(ips) == 0 {
		return result, nil
	}

	// Build "?,?,?" placeholder list and the matching []any args slice.
	placeholders := strings.Repeat("?,", len(ips))
	placeholders = placeholders[:len(placeholders)-1] // trim trailing comma
	args := make([]any, len(ips))
	for i, ip := range ips {
		args[i] = ip
	}

	q := "SELECT ip, hostname FROM rdns WHERE hostname IS NOT NULL AND ip IN (" + placeholders + ")"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: query hostnames: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var ip string
		var hostname sql.NullString
		if err := rows.Scan(&ip, &hostname); err != nil {
			return nil, fmt.Errorf("storage: scan hostname row: %w", err)
		}
		if hostname.Valid && hostname.String != "" {
			result[ip] = hostname.String
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: iterate hostname rows: %w", err)
	}
	return result, nil
}
