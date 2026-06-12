// The heartbeat loop (§3.1): registers the agent on startup and keeps
// the registration fresh; each response carries central's
// authoritative target set, which is how UI-side target changes
// propagate to agents without config edits.

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

const (
	defaultHeartbeatInterval = 60 * time.Second

	// heartbeatRetryInterval is the cadence while heartbeats are
	// failing. Faster than the steady-state interval because until
	// the FIRST heartbeat succeeds the agent does no probing at all
	// (§7) — a 60s dead window on a transient startup blip would be
	// needless.
	heartbeatRetryInterval = 10 * time.Second
)

// HeartbeatConfig controls the loop.
type HeartbeatConfig struct {
	ProbeID   string
	Version   string
	StartedAt time.Time

	// Interval is the steady-state cadence (config
	// central.heartbeat_interval, default 60s).
	Interval time.Duration

	// Targets returns the agent's current local target set, announced
	// in each heartbeat (informational — central owns the set).
	Targets func() []string

	// OnTargetSet receives central's authoritative target set after
	// every successful heartbeat. The agent reconciles its pipelines
	// against it.
	OnTargetSet func([]string)
}

type heartbeatPayload struct {
	ProbeID   string   `json:"probe_id"`
	Version   string   `json:"version"`
	StartedAt int64    `json:"started_at"`
	Targets   []string `json:"targets"`
}

type heartbeatReply struct {
	RegisteredAt     int64    `json:"registered_at"`
	CentralTargetSet []string `json:"central_target_set"`
}

// RunHeartbeat sends an immediate heartbeat, then loops until ctx is
// done. Retryable failures (central down) retry at
// heartbeatRetryInterval; successes settle to cfg.Interval. Returns
// nil on ctx cancellation, or an error on 401/400 — both are
// config-shaped and unfixable by retry, so the caller exits non-zero
// (§12.1 + lesson #9: visible crash-loop beats silent dead agent).
func RunHeartbeat(ctx context.Context, client *Client, cfg HeartbeatConfig, log *slog.Logger) error {
	if cfg.Interval <= 0 {
		cfg.Interval = defaultHeartbeatInterval
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(nopWriter{}, nil))
	}

	timer := time.NewTimer(0) // immediate first beat
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}

		ok, err := beatOnce(ctx, client, cfg)
		switch {
		case err != nil:
			return err // 401 — fatal
		case ok:
			timer.Reset(cfg.Interval)
		default:
			log.Warn("probe: heartbeat failed; retrying", "retry_in", heartbeatRetryInterval)
			timer.Reset(heartbeatRetryInterval)
		}
	}
}

// beatOnce sends one heartbeat. ok=false means a retryable failure;
// a non-nil error means 401 (fatal).
func beatOnce(ctx context.Context, client *Client, cfg HeartbeatConfig) (ok bool, fatal error) {
	var targets []string
	if cfg.Targets != nil {
		targets = cfg.Targets()
	}
	if targets == nil {
		targets = []string{}
	}
	body, err := json.Marshal(heartbeatPayload{
		ProbeID:   cfg.ProbeID,
		Version:   cfg.Version,
		StartedAt: cfg.StartedAt.UnixMilli(),
		Targets:   targets,
	})
	if err != nil {
		return false, fmt.Errorf("probe: heartbeat marshal: %w", err)
	}

	outcome, respBody, postErr := client.PostJSON(ctx, "/api/ingest/heartbeat", body)
	switch outcome {
	case OutcomeOK:
		var reply heartbeatReply
		if err := json.Unmarshal(respBody, &reply); err != nil {
			// Central acked but the reply didn't parse — treat like a
			// retryable failure; the target set stays as-is until the
			// next beat.
			return false, nil
		}
		if cfg.OnTargetSet != nil {
			cfg.OnTargetSet(reply.CentralTargetSet)
		}
		return true, nil
	case OutcomeUnauthorized:
		return false, postErr
	case OutcomeDrop:
		// A 400 on heartbeat (bad probe_id) is config-shaped: no
		// number of retries fixes it. Fail loud (lesson #9) so the
		// operator sees a failed unit, not a silent never-registered
		// agent.
		return false, fmt.Errorf("probe: central rejected heartbeat: %w", postErr)
	default:
		return false, nil
	}
}
