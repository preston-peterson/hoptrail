// Package rdns runs a background worker that populates the rdns
// table with reverse-DNS lookups for IPs seen in probe samples.
//
// The worker has three jobs:
//  1. Poll the storage layer for IPs in samples that haven't been
//     looked up yet (storage.UnresolvedIPs).
//  2. For each unresolved IP, perform a reverse-DNS lookup with a
//     per-call timeout, with a small inter-lookup delay so the local
//     resolver and upstream DNS aren't hammered.
//  3. Write the result back via storage.UpsertRDNS — including empty
//     results, so failed lookups don't get retried indefinitely.
//
// The worker is deliberately conservative: small batches, serial
// lookups, sleeps between polls. Reverse-DNS is best-effort UX, not
// a critical path — it's better to be slow and quiet than to flood
// the resolver. Once an IP is recorded, it's never re-resolved (in
// the current implementation); the schema's resolved_at column is
// reserved for future re-resolution logic.

package rdns

import (
	"context"
	"log/slog"
	"net"
	"time"

	"github.com/preston-peterson/hoptrail/internal/storage"
)

// LookupFunc is the signature for a reverse-DNS lookup. The
// production implementation wraps net.DefaultResolver.LookupAddr.
// Tests substitute a fake to avoid real DNS traffic.
//
// Implementations should:
//   - Honor the context (cancellation, deadline)
//   - Return "", nil for "no PTR record" (NOT an error)
//   - Return "", err only for genuine errors (timeout, network)
//
// Either form ("", nil) or ("", err) results in a NULL row being
// recorded, so the worker stops re-querying. Production callers
// generally see (name, nil) on success and ("", err) on failure;
// the ("", nil) form arises mostly from the SystemResolver when a
// PTR query succeeds but returns no records.
type LookupFunc func(ctx context.Context, ip string) (string, error)

// SystemResolver is the production LookupFunc. It uses Go's default
// resolver, which honors /etc/resolv.conf and systemd-resolved.
// Returns the first name returned by the PTR query, with the trailing
// dot stripped — most PTRs return "host.example.com.", and consumers
// of this package expect the trailing-dot-free form.
func SystemResolver(ctx context.Context, ip string) (string, error) {
	names, err := net.DefaultResolver.LookupAddr(ctx, ip)
	if err != nil {
		// LookupAddr returns *net.DNSError with IsNotFound=true when
		// the PTR query succeeded but had no records. Treat as "no
		// name available," not an error — the worker records a NULL
		// row either way, but distinguishing keeps logs cleaner.
		if dnsErr, ok := err.(*net.DNSError); ok && dnsErr.IsNotFound {
			return "", nil
		}
		return "", err
	}
	if len(names) == 0 {
		return "", nil
	}
	name := names[0]
	if len(name) > 0 && name[len(name)-1] == '.' {
		name = name[:len(name)-1]
	}
	return name, nil
}

// Config governs the worker's polling and lookup behavior. The
// zero value is not usable — callers must set the durations and
// batch size, typically via DefaultConfig().
type Config struct {
	// PollInterval is how often the worker scans the store for
	// unresolved IPs. Shorter means newly-discovered hops get
	// hostnames faster; longer means less DB load. 60s is a
	// reasonable balance for a daemon that's also doing probes
	// every 1-5 seconds.
	PollInterval time.Duration

	// LookupTimeout caps each individual reverse-DNS query. The
	// system resolver may itself enforce a longer timeout if this
	// is too generous; 2s is enough for a well-functioning resolver
	// and short enough that a dead resolver doesn't stall the
	// worker for minutes.
	LookupTimeout time.Duration

	// InterLookupDelay is the sleep between consecutive lookups in
	// a batch. Spreading the queries reduces resolver pressure;
	// 200ms × 50 IPs = 10s total batch time, which is fine for a
	// 60s poll interval.
	InterLookupDelay time.Duration

	// BatchSize is the maximum number of IPs to resolve in one poll
	// cycle. Larger batches catch up faster on startup but extend
	// the cycle's tail; 50 is enough for any homelab-scale path
	// (typical hops 8-15, multiplied by route changes over time).
	BatchSize int
}

// DefaultConfig returns sensible defaults for production use.
func DefaultConfig() Config {
	return Config{
		PollInterval:     60 * time.Second,
		LookupTimeout:    2 * time.Second,
		InterLookupDelay: 200 * time.Millisecond,
		BatchSize:        50,
	}
}

// Resolver is the worker. Construct with New, then call Run in a
// goroutine to start it. Run blocks until ctx is canceled.
type Resolver struct {
	cfg    Config
	store  *storage.Store
	lookup LookupFunc
	log    *slog.Logger

	// watermark is the samples rowid this worker has scanned up to.
	// In-memory: a fresh process starts at 0, doing one full scan to
	// catch strays, after which every cycle is O(rows since last
	// look) instead of a full anti-join over the whole samples table
	// (see storage.UnresolvedIPs for why that mattered). Only the
	// Run goroutine touches it.
	watermark int64
}

// New constructs a Resolver. The lookup function is injectable so
// tests can substitute a fake; production callers pass SystemResolver.
// The logger may be nil; a discard logger is used in that case.
func New(cfg Config, store *storage.Store, lookup LookupFunc, log *slog.Logger) *Resolver {
	if log == nil {
		log = slog.New(slog.NewTextHandler(nopWriter{}, nil))
	}
	if lookup == nil {
		lookup = SystemResolver
	}
	return &Resolver{cfg: cfg, store: store, lookup: lookup, log: log}
}

// Run starts the worker loop and blocks until ctx is canceled. On
// ctx cancellation it returns nil; any storage or lookup errors
// during a poll are logged but do not terminate the loop, since
// transient resolver failures are normal and the worker should
// recover on its own.
//
// The first poll fires after PollInterval — not immediately on
// start. On a freshly-installed daemon there are no samples yet,
// so an immediate scan would always find nothing; waiting one
// interval avoids that wasted query and gives the probe loop time
// to populate samples first.
func (r *Resolver) Run(ctx context.Context) {
	r.log.Info("rdns: worker started",
		"poll_interval", r.cfg.PollInterval,
		"lookup_timeout", r.cfg.LookupTimeout,
		"batch_size", r.cfg.BatchSize)

	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			r.log.Info("rdns: worker stopped")
			return
		case <-ticker.C:
			r.runOnce(ctx)
		}
	}
}

// runOnce performs a single poll cycle: fetch unresolved IPs,
// resolve each, write results back. Errors are logged and the
// cycle continues — a transient resolver failure shouldn't stop
// the worker from making progress on other IPs in the batch.
func (r *Resolver) runOnce(ctx context.Context) {
	ips, scanMax, err := r.store.UnresolvedIPs(ctx, r.watermark, r.cfg.BatchSize)
	if err != nil {
		r.log.Error("rdns: query unresolved IPs", "err", err)
		return
	}
	if len(ips) == 0 {
		r.watermark = scanMax
		return // nothing to do
	}

	r.log.Debug("rdns: resolving batch", "count", len(ips))
	var resolved, failed, upsertFailed int
	for i, ip := range ips {
		if ctx.Err() != nil {
			return
		}

		// Inter-lookup delay between (but not before) queries —
		// the first lookup runs immediately, subsequent ones sleep.
		if i > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(r.cfg.InterLookupDelay):
			}
		}

		lookupCtx, cancel := context.WithTimeout(ctx, r.cfg.LookupTimeout)
		name, lookupErr := r.lookup(lookupCtx, ip)
		cancel()

		// Whether the lookup succeeded, failed, or returned empty,
		// we record a row so this IP is not queried again. The
		// hostname-is-NULL case (state 2 in the schema doc) is how
		// the table represents "we tried; no name."
		if upsertErr := r.store.UpsertRDNS(ctx, ip, name); upsertErr != nil {
			r.log.Error("rdns: upsert", "ip", ip, "err", upsertErr)
			upsertFailed++
			continue
		}

		if lookupErr != nil {
			r.log.Debug("rdns: lookup failed", "ip", ip, "err", lookupErr)
			failed++
		} else if name == "" {
			failed++ // succeeded but no PTR — counts toward "did not get a name"
		} else {
			r.log.Debug("rdns: resolved", "ip", ip, "name", name)
			resolved++
		}
	}

	// Advance the watermark only when this cycle fully covered the
	// scanned range: the batch wasn't clipped by the limit (a clipped
	// batch leaves unresolved IPs inside the range — the resolved
	// ones drop out of the anti-join, so re-scanning converges), and
	// every attempted IP got its rdns row written (an upsert failure
	// would otherwise be skipped past forever, until a process
	// restart's watermark-0 full scan). Early ctx-cancel returns
	// above deliberately never advance.
	if len(ips) < r.cfg.BatchSize && upsertFailed == 0 {
		r.watermark = scanMax
	}

	if resolved > 0 || failed > 0 {
		r.log.Info("rdns: batch complete", "resolved", resolved, "no_name", failed)
	}
}

// nopWriter is an io.Writer that discards everything. Used when the
// caller passes a nil logger to New.
type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
