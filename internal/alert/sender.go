// The delivery side: a goroutine draining alert_queue oldest-first
// into ntfy with exponential backoff. The queue is SQLite, so nothing
// is lost to a daemon restart or an ntfy outage — late delivery beats
// no delivery (the operator's explicit call).

package alert

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/preston-peterson/hoptrail/internal/storage"
)

// Poster delivers one notification. The production implementation
// POSTs to ntfy; tests inject. A *PosterPermanentError return means
// "this item can never deliver — drop it" (4xx); any other error
// retries with backoff.
type Poster func(ctx context.Context, cfg Config, item storage.AlertQueueItem) error

// PosterPermanentError marks an undeliverable item (poison).
type PosterPermanentError struct{ msg string }

func (e *PosterPermanentError) Error() string { return e.msg }

// NtfyPost is the production Poster.
func NtfyPost(ctx context.Context, cfg Config, item storage.AlertQueueItem) error {
	url := strings.TrimRight(cfg.ServerURL, "/") + "/" + cfg.Topic
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(item.Body))
	if err != nil {
		return &PosterPermanentError{msg: err.Error()}
	}
	req.Header.Set("Title", sanitizeLatin1(item.Title))
	req.Header.Set("Priority", item.Priority)
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return err // network: retry
	}
	defer res.Body.Close()
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
	msg := fmt.Sprintf("ntfy %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	if res.StatusCode >= 400 && res.StatusCode < 500 && res.StatusCode != 429 {
		return &PosterPermanentError{msg: msg}
	}
	return fmt.Errorf("%s", msg)
}

// Sender drains the queue.
type Sender struct {
	store *storage.Store
	post  Poster
	cfgFn func() Config
	log   *slog.Logger

	// Last delivery outcome, for /api/alerts/status.
	LastOK  func() (time.Time, string) // set internally; read via Status
	lastAt  time.Time
	lastErr string
}

func NewSender(store *storage.Store, cfgFn func() Config, post Poster, log *slog.Logger) *Sender {
	if post == nil {
		post = NtfyPost
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(nopWriter{}, nil))
	}
	return &Sender{store: store, post: post, cfgFn: cfgFn, log: log}
}

// Status returns the last delivery attempt's time and error ("" = ok).
func (s *Sender) Status() (time.Time, string) { return s.lastAt, s.lastErr }

// Run blocks until ctx ends. Poll cadence 5s when idle; backoff
// doubles from 5s to 5m across consecutive failures.
func (s *Sender) Run(ctx context.Context) {
	backoff := 5 * time.Second
	const maxBackoff = 5 * time.Minute
	for {
		wait := 5 * time.Second
		item, err := s.store.NextQueuedAlert(ctx)
		switch {
		case err != nil:
			s.log.Warn("alert: queue read failed", "err", err)
		case item == nil:
			// idle
		default:
			cfg := s.cfgFn()
			if cfg.ServerURL == "" || cfg.Topic == "" {
				// Transport unconfigured: keep the item, check later.
				wait = 30 * time.Second
				break
			}
			perr := s.post(ctx, cfg, *item)
			s.lastAt = time.Now()
			switch e := perr.(type) {
			case nil:
				s.lastErr = ""
				_ = s.store.DeleteQueuedAlert(ctx, item.ID)
				backoff = 5 * time.Second
				wait = 0 // drain immediately
			case *PosterPermanentError:
				s.lastErr = e.msg
				s.log.Error("alert: dropping undeliverable notification", "title", item.Title, "err", e.msg)
				_ = s.store.DeleteQueuedAlert(ctx, item.ID)
				wait = 0
			default:
				s.lastErr = perr.Error()
				_ = s.store.BumpAlertAttempts(ctx, item.ID)
				s.log.Warn("alert: delivery failed, will retry", "title", item.Title, "attempts", item.Attempts+1, "err", perr)
				wait = backoff
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
		}
		if wait == 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
