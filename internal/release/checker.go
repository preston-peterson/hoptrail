package release

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"
)

// ConfigStore is the slice of storage.Store the checker needs —
// primitives only, keeping this package free of storage imports.
type ConfigStore interface {
	GetConfig(ctx context.Context, key string) (value string, ok bool, err error)
	SetConfig(ctx context.Context, key, value string) error
}

// Checker periodically looks for a newer release and persists the
// result to the config KV, where the update panel and status page read
// it. It NEVER downloads or applies anything — surfacing "an update
// exists" is its whole job; the operator drives everything else.
//
// Cadence is the operator's update.check_interval setting (default
// monthly). The "is a check due" decision keys off the persisted
// last-check timestamp, so daemon restarts don't reset the clock and a
// box that was off when a check came due simply checks on its next
// hourly tick.
type Checker struct {
	Store ConfigStore
	// Fetch returns the latest release (production: Client.Latest).
	Fetch func(ctx context.Context) (*Release, error)
	Log   *slog.Logger

	// Tick is how often the due-ness rule is evaluated. Zero means
	// hourly — fine-grained enough for a daily shortest interval.
	Tick time.Duration

	// now is injectable for tests; nil means time.Now.
	now func() time.Time
}

func (c *Checker) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// Run loops until ctx is canceled. One due-ness pass runs immediately
// at startup (a fresh install gets its first check at first boot; an
// established one only checks if the interval has truly elapsed).
func (c *Checker) Run(ctx context.Context) {
	tick := c.Tick
	if tick <= 0 {
		tick = time.Hour
	}
	c.pass(ctx)
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.pass(ctx)
		}
	}
}

// pass checks once if a check is due. Errors are persisted (the UI
// shows "last check failed: …") and logged, never fatal.
func (c *Checker) pass(ctx context.Context) {
	setting, ok, err := c.Store.GetConfig(ctx, KeyCheckInterval)
	if err != nil {
		c.Log.Error("release: read check interval", "err", err)
		return
	}
	if !ok {
		setting = DefaultCheckInterval
	}
	dur, enabled := IntervalDuration(setting)
	if !enabled {
		return
	}
	last := c.ReadLastCheck(ctx)
	if last != nil && c.clock().Sub(time.UnixMilli(last.At)) < dur {
		return
	}
	c.CheckNow(ctx)
}

// CheckNow performs one check unconditionally and persists the result.
// The manual check endpoint shares it. Returns what was persisted.
func (c *Checker) CheckNow(ctx context.Context) LastCheck {
	result := LastCheck{At: c.clock().UnixMilli()}
	rel, err := c.Fetch(ctx)
	if err != nil {
		result.Err = err.Error()
		c.Log.Warn("release: check failed", "err", err)
	} else {
		result.LatestVersion = rel.Version()
		result.URL = rel.HTMLURL
		c.Log.Info("release: checked", "latest", result.LatestVersion)
	}
	raw, _ := json.Marshal(result)
	if err := c.Store.SetConfig(ctx, KeyLastCheck, string(raw)); err != nil {
		c.Log.Error("release: persist check result", "err", err)
	}
	return result
}

// ReadLastCheck returns the persisted last-check result, nil when no
// check has ever completed (or the row is unreadable).
func (c *Checker) ReadLastCheck(ctx context.Context) *LastCheck {
	raw, ok, err := c.Store.GetConfig(ctx, KeyLastCheck)
	if err != nil || !ok {
		return nil
	}
	var lc LastCheck
	if json.Unmarshal([]byte(raw), &lc) != nil || lc.At == 0 {
		return nil
	}
	return &lc
}
