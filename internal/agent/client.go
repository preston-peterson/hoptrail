// Package agent implements the remote-probe role (user-facing name:
// "probe" — this package keeps "agent" because internal/probe is the
// ICMP engine; operators never see the word). It is hoptrail's v0.3
// distributed mode (docs/v0.3-protocol-design.md §7-§8). It reuses
// internal/probe verbatim for the actual probing; what lives here is
// the coordination machinery: an HTTP client speaking the ingest
// protocol, an HTTPSink that replaces the local BatchedSink, a SQLite
// spill buffer for partition recovery, and the heartbeat loop that
// keeps the agent registered and its target set synced to central.
package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Outcome classifies an ingest POST per the §3.2 response-code
// contract. It drives control flow; the accompanying error carries
// detail for logging only.
type Outcome int

const (
	// OutcomeOK — 2xx. The batch is central's problem now.
	OutcomeOK Outcome = iota
	// OutcomeRetry — 5xx, connection failure, or timeout. Retry with
	// backoff; the data is still ours to deliver.
	OutcomeRetry
	// OutcomeDrop — 4xx other than 401. Central understood us and
	// said no (malformed, skewed clock). Drop the batch and log; a
	// retry would fail identically forever.
	OutcomeDrop
	// OutcomeUnauthorized — 401. The token was removed or rotated.
	// Fatal: the agent stops probing and exits non-zero so the
	// failure is visible in systemctl status (design §12.1).
	OutcomeUnauthorized
)

// Client speaks the ingest protocol to one central daemon.
type Client struct {
	baseURL string
	token   string
	httpc   *http.Client
}

// NewClient builds a Client for the given central URL (scheme +
// host[:port], no trailing path) and bearer token.
func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		// The timeout covers the whole exchange. Batches are small
		// (one ingest_interval of samples) so 15s of headroom means
		// "the link or central is in real trouble" — which is what
		// the retry/spill path is for.
		httpc: &http.Client{Timeout: 15 * time.Second},
	}
}

// PostJSON sends body to path (e.g. "/api/ingest/samples") and
// classifies the result. The response body is returned on OutcomeOK
// for callers that parse it (heartbeat); nil otherwise.
func (c *Client) PostJSON(ctx context.Context, path string, body []byte) (Outcome, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		// Malformed URL — config-shaped problem, but surfaced per-call.
		return OutcomeRetry, nil, fmt.Errorf("probe: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)

	res, err := c.httpc.Do(req)
	if err != nil {
		return OutcomeRetry, nil, fmt.Errorf("probe: POST %s: %w", path, err)
	}
	defer res.Body.Close()

	switch {
	case res.StatusCode >= 200 && res.StatusCode < 300:
		respBody, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		if err != nil {
			// The POST landed (central processed it); only the response
			// read failed. Treating this as OK without a body is right
			// for ingest; heartbeat callers see the error and skip
			// parsing.
			return OutcomeOK, nil, fmt.Errorf("probe: read response: %w", err)
		}
		return OutcomeOK, respBody, nil
	case res.StatusCode == http.StatusUnauthorized:
		return OutcomeUnauthorized, nil, fmt.Errorf("probe: POST %s: 401 unauthorized — token removed or rotated on central", path)
	case res.StatusCode >= 400 && res.StatusCode < 500:
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return OutcomeDrop, nil, fmt.Errorf("probe: POST %s: %d: %s", path, res.StatusCode, strings.TrimSpace(string(msg)))
	default:
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return OutcomeRetry, nil, fmt.Errorf("probe: POST %s: %d: %s", path, res.StatusCode, strings.TrimSpace(string(msg)))
	}
}

// DownloadFile streams an authenticated GET of path into w, returning
// the byte count. Used by the self-updater to fetch the new binary
// from the central (#22); the caller verifies the sha256 — this just
// moves bytes. A longer timeout than the ingest client's 15s because
// a ~10 MB binary over a slow site link is a legitimate slow request.
func (c *Client) DownloadFile(ctx context.Context, path string, w io.Writer) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return 0, fmt.Errorf("probe: build download request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	// A dedicated transport call without the ingest client's 15s
	// ceiling — the ctx timeout above governs.
	res, err := (&http.Client{}).Do(req)
	if err != nil {
		return 0, fmt.Errorf("probe: GET %s: %w", path, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return 0, fmt.Errorf("probe: GET %s: %d: %s", path, res.StatusCode, strings.TrimSpace(string(msg)))
	}
	return io.Copy(w, res.Body)
}

// NewBatchID returns a unique, time-sortable, opaque batch identifier
// (§3.2): 12 hex chars of unix-ms timestamp + 16 hex chars of
// crypto/rand. Stable across retries because the sink generates it
// once per batch, before the first delivery attempt.
func NewBatchID() (string, error) {
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return "", fmt.Errorf("probe: batch id entropy: %w", err)
	}
	return fmt.Sprintf("%012x%s", time.Now().UnixMilli(), hex.EncodeToString(suffix)), nil
}
