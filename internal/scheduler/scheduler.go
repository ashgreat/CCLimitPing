// Package scheduler runs the watch loop: for each provider it sleeps until the
// 5h window resets, then triggers a minimal ping to start the next window,
// keeping windows back-to-back. It respects the weekly limit and never lets a
// transient error kill the loop. When requested on an interactive terminal, it
// also draws a live status line (heartbeat + per-provider countdowns) beneath
// the scrolling log.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/wavever/CCLimitPing/internal/auth"
	"github.com/wavever/CCLimitPing/internal/config"
	"github.com/wavever/CCLimitPing/internal/notify"
	"github.com/wavever/CCLimitPing/internal/provider"
	"github.com/wavever/CCLimitPing/internal/usage"
)

const (
	postPingGrace     = 15 * time.Second // wait after a ping before re-reading usage
	minBackoff        = 30 * time.Second
	maxBackoff        = 10 * time.Minute
	rateLimitPause    = 5 * time.Minute
	defaultWindow     = 5 * time.Hour // fallback when the API omits the window length
	readTimeout       = 30 * time.Second
	triggerTimeout    = 3 * time.Minute
	activeTaskPoll    = time.Minute
	weeklyOnlyRecheck = 15 * time.Minute
	// Usage reads cost no quota, so an exhausted weekly limit is rechecked
	// periodically to catch an early reset instead of sleeping until the
	// scheduled one.
	weeklyExhaustedRecheck = 15 * time.Minute
)

// Target pairs a provider with its scheduling options.
type Target struct {
	Provider   provider.Provider
	AlignStart time.Time // zero = ping as soon as the window is free
	AutoRedeem bool      // spend a reset credit that is about to lapse
}

// Scheduler drives the watch loops.
type Scheduler struct {
	cfg     config.Config
	targets []Target
	dryRun  bool
	log     *log.Logger
	live    *liveStatus
	state   *stateStore
}

// Option customizes a Scheduler.
type Option func(*Scheduler)

// WithStateFile persists non-secret per-provider timing metadata at path.
func WithStateFile(path string) Option {
	return func(s *Scheduler) {
		state, err := newStateStore(path)
		if err != nil {
			s.log.Printf("scheduler state unavailable at %s: %v; continuing in memory", path, err)
			return
		}
		s.state = state
	}
}

// New builds a scheduler that logs to out. When live is true and out is an
// interactive terminal, a live status line is drawn beneath the scrolling log;
// otherwise log output passes straight through.
func New(cfg config.Config, targets []Target, dryRun, live bool, out io.Writer, opts ...Option) *Scheduler {
	names := make([]string, len(targets))
	for i, t := range targets {
		names[i] = t.Provider.Name()
	}
	status := newLiveStatus(out, names, live)
	s := &Scheduler{
		cfg:     cfg,
		targets: targets,
		dryRun:  dryRun,
		log:     log.New(status, "", log.LstdFlags),
		live:    status,
		state:   mustMemoryStateStore(),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func mustMemoryStateStore() *stateStore {
	s, _ := newStateStore("")
	return s
}

// Run starts one loop per target and blocks until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	names := make([]string, len(s.targets))
	for i, t := range s.targets {
		names[i] = t.Provider.Name()
	}
	s.log.Printf("watching %v (weekly_threshold=%.2f, reset_buffer=%s, notify=%t, dry_run=%t)",
		names, s.cfg.WeeklyThreshold, s.cfg.ResetBuffer.Duration, s.cfg.Notify, s.dryRun)

	var liveWG sync.WaitGroup
	if s.live.enabled {
		liveWG.Add(1)
		go func() {
			defer liveWG.Done()
			s.live.run(ctx)
		}()
	}

	done := make(chan struct{}, len(s.targets))
	for _, t := range s.targets {
		go func(t Target) {
			s.runTarget(ctx, t)
			done <- struct{}{}
		}(t)
	}
	for range s.targets {
		<-done
	}
	liveWG.Wait() // let the render loop clear its line before the final log
	s.log.Printf("shutting down")
}

func (s *Scheduler) runTarget(ctx context.Context, t Target) {
	name := t.Provider.Name()
	readBackoff := minBackoff
	triggerBackoff := minBackoff
	aligned := t.AlignStart.IsZero() // whether the align gate has been passed
	weeklyReported := false          // log/notify an exhausted weekly limit once, not every recheck
	lastPingAt := s.state.get(name).LastPingAt

	for {
		if ctx.Err() != nil {
			return
		}

		s.live.set(name, "checking usage…", time.Time{})
		rctx, cancel := context.WithTimeout(ctx, readTimeout)
		u, err := t.Provider.ReadUsage(rctx)
		cancel()
		if err != nil && errors.Is(err, auth.ErrRefreshDisabled) {
			// The stored token expired and limitping may not rotate it; only the
			// official CLI can, and the ping is what runs that CLI. Retrying the
			// read would wait forever, so continue with an empty snapshot: the
			// loop below pings once the last ping is a window old, and the read
			// after the ping picks up the refreshed token.
			// ponytail: empty snapshot = "no window info"; it schedules by the
			// default 5h window until the read recovers.
			s.log.Printf("[%s] usage unreadable: %v; pinging on schedule so the official CLI refreshes the token", name, err)
			u, err = &usage.Usage{Provider: name, FetchedAt: time.Now()}, nil
		}
		if err != nil {
			var httpErr *provider.UsageHTTPError
			if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusTooManyRequests {
				wait := usageRateLimitWait(httpErr.RetryAfter, time.Now())
				s.log.Printf("[%s] usage endpoint rate limited; pausing reads for %s", name, wait.Round(time.Second))
				s.live.set(name, "usage rate limited", time.Now().Add(wait))
				if !sleepCtx(ctx, wait) {
					return
				}
				readBackoff = minBackoff
				continue
			}
			s.log.Printf("[%s] read usage failed: %v (retry in %s)", name, err, readBackoff)
			s.live.set(name, "read failed — retrying", time.Now().Add(readBackoff))
			if !sleepCtx(ctx, readBackoff) {
				return
			}
			readBackoff = nextBackoff(readBackoff)
			continue
		}
		readBackoff = minBackoff
		if !u.FiveHour.Missing() {
			if err := s.state.observeWindow(name, u.FiveHour); err != nil {
				s.log.Printf("[%s] saving scheduler state failed: %v", name, err)
			}
		}

		// A spent credit resets the windows, so this snapshot is stale; re-read
		// before deciding anything from it.
		if s.redeemExpiringCredit(ctx, t, u) {
			continue
		}

		// Respect the weekly limit: if exhausted (and no usable credits), wait
		// for the weekly window to reset instead of pinging.
		if s.weeklyExhausted(u) {
			wait := weeklyExhaustedWait(u.Weekly, s.cfg.ResetBuffer.Duration)
			if !weeklyReported {
				s.log.Printf("[%s] weekly limit exhausted (%.0f%%); weekly reset in %s, rechecking every %s in case it resets early",
					name, u.Weekly.UsedPercent, u.Weekly.Remaining().Round(time.Second), weeklyExhaustedRecheck)
				s.notify(name+": weekly limit reached", "Skipping pings until weekly reset")
				weeklyReported = true
			}
			s.live.set(name, fmt.Sprintf("weekly limit reached (%.0f%%) — rechecking", u.Weekly.UsedPercent), time.Now().Add(wait))
			if !sleepCtx(ctx, wait) {
				return
			}
			continue
		}
		weeklyReported = false

		// A missing 5h window is ambiguous: the provider may have removed that
		// limit, or it may omit a freshly reset window until the first request
		// anchors it. If we observed the reset beforehand, trigger once. With no
		// such evidence, recheck periodically without spending model quota.
		if u.FiveHour.Missing() && u.Weekly.Active() {
			st := s.state.get(name)
			now := time.Now()
			if st.AwaitingConfirmation {
				if err := s.state.disarmUnconfirmed(name); err != nil {
					s.log.Printf("[%s] saving scheduler state failed: %v", name, err)
				}
				s.log.Printf("[%s] ping was not followed by a visible 5h window; treating limits as weekly-only and rechecking in %s",
					name, weeklyOnlyRecheck)
				s.live.set(name, "5h window absent — rechecking", now.Add(weeklyOnlyRecheck))
				if !sleepCtx(ctx, weeklyOnlyRecheck) {
					return
				}
				continue
			}

			dueAt := st.ExpectedResetAt.Add(s.cfg.ResetBuffer.Duration)
			observedWindow := time.Duration(st.WindowSeconds) * time.Second
			observedResetDue := st.WindowSeconds > 0 && !st.ExpectedResetAt.IsZero() &&
				!now.Before(dueAt) && now.Sub(dueAt) <= observedWindow
			if observedResetDue {
				u.FiveHour.WindowSeconds = st.WindowSeconds
				s.log.Printf("[%s] previously observed 5h window reset at %s and is now absent; triggering once to anchor it",
					name, st.ExpectedResetAt.Local().Format("15:04:05"))
			} else {
				wait := missingWindowRecheck(st, u.Weekly, now, s.cfg.ResetBuffer.Duration)
				s.log.Printf("[%s] no 5h window (weekly-only or inactive, %.0f%%); rechecking in %s without pinging",
					name, u.Weekly.UsedPercent, wait.Round(time.Second))
				s.live.set(name, "5h window absent — rechecking", now.Add(wait))
				if !sleepCtx(ctx, wait) {
					return
				}
				continue
			}
		}

		// If the 5h window is still running, wait until it resets, then ping.
		if u.FiveHour.Active() {
			triggerBackoff = minBackoff
			wait := u.FiveHour.Remaining() + s.cfg.ResetBuffer.Duration
			s.log.Printf("[%s] 5h window active (%.0f%%), next ping at %s (in %s)",
				name, u.FiveHour.UsedPercent,
				u.FiveHour.ResetsAt.Local().Format("15:04:05"), wait.Round(time.Second))
			s.live.set(name, fmt.Sprintf("5h window %.0f%% — next ping", u.FiveHour.UsedPercent), time.Now().Add(wait))
			if !sleepCtx(ctx, wait) {
				return
			}
			continue
		}

		// Window is free. Guard against double-pinging if our last ping isn't
		// reflected by the endpoint yet.
		if !lastPingAt.IsZero() {
			est := lastPingAt.Add(windowLen(u.FiveHour))
			if time.Now().Before(est) {
				wait := time.Until(est) + s.cfg.ResetBuffer.Duration
				s.log.Printf("[%s] recent ping not yet visible; waiting %s", name, wait.Round(time.Second))
				s.live.set(name, "awaiting window", time.Now().Add(wait))
				if !sleepCtx(ctx, wait) {
					return
				}
				continue
			}
		}

		// Honor the first-window alignment anchor, once.
		if !aligned {
			if d := time.Until(t.AlignStart); d > 0 {
				s.log.Printf("[%s] waiting for align_start %s (in %s)",
					name, t.AlignStart.Local().Format("15:04:05"), d.Round(time.Second))
				s.live.set(name, "waiting for align_start", t.AlignStart)
				if !sleepCtx(ctx, d) {
					return
				}
			}
			aligned = true
		}

		if desc, active, err := activeProviderTask(ctx, t.Provider); err != nil {
			s.log.Printf("[%s] active task check failed: %v; pinging anyway", name, err)
		} else if active {
			s.log.Printf("[%s] window reset but %s is running; waiting %s for it to start the next window",
				name, desc, activeTaskPoll.Round(time.Second))
			s.live.set(name, desc+" active — deferring ping", time.Now().Add(activeTaskPoll))
			if !sleepCtx(ctx, activeTaskPoll) {
				return
			}
			continue
		}

		// Trigger the window.
		if !s.dryRun {
			s.log.Printf("[%s] window reset — triggering ping now…", name)
		}
		s.live.set(name, "window reset — triggering ping…", time.Time{})
		tctx, tcancel := context.WithTimeout(ctx, triggerTimeout)
		res, err := t.Provider.Trigger(tctx, s.dryRun)
		tcancel()
		if s.dryRun {
			if err != nil {
				s.log.Printf("[%s] dry-run ping failed: %v (retry in %s)", name, err, triggerBackoff)
				if !sleepCtx(ctx, triggerBackoff) {
					return
				}
				triggerBackoff = nextBackoff(triggerBackoff)
				continue
			}
			s.log.Printf("[%s] DRY-RUN would ping now: %s", name, res.Command)
			// In dry-run we can't actually start a window, so estimate the next
			// cycle from the configured window length to keep the loop sane.
			// Sleep immediately instead of doing an extra usage read that cannot
			// observe a real newly-started window.
			lastPingAt = time.Now()
			wait := windowLen(u.FiveHour) + s.cfg.ResetBuffer.Duration
			s.live.set(name, "dry-run — next estimated ping", lastPingAt.Add(wait))
			if !sleepCtx(ctx, wait) {
				return
			}
			continue
		}
		if err != nil {
			s.log.Printf("[%s] ping failed: %v (retry in %s)", name, err, triggerBackoff)
			s.live.set(name, "ping failed — retrying", time.Now().Add(triggerBackoff))
			s.notify(name+": ping failed", err.Error())
			if !sleepCtx(ctx, triggerBackoff) {
				return
			}
			triggerBackoff = nextBackoff(triggerBackoff)
			continue
		}
		triggerBackoff = minBackoff
		lastPingAt = time.Now()
		if err := s.state.recordPing(name, lastPingAt, windowLen(u.FiveHour)); err != nil {
			s.log.Printf("[%s] saving scheduler state failed: %v", name, err)
		}
		s.log.Printf("[%s] ping sent, new window started%s", name, triggerCost(res))
		s.live.set(name, "ping sent — checking window soon", lastPingAt.Add(postPingGrace))
		s.notify(name+": window started", "New 5h window"+triggerCost(res))

		if !sleepCtx(ctx, postPingGrace) {
			return
		}
	}
}

func (s *Scheduler) weeklyExhausted(u *usage.Usage) bool {
	return u.WeeklyExhausted(s.cfg.WeeklyThreshold)
}

// redeemExpiringCredit spends a banked reset credit that is about to lapse,
// when the target opted in. It reports whether the windows were actually reset;
// a failure is logged and never breaks the loop.
func (s *Scheduler) redeemExpiringCredit(ctx context.Context, t Target, u *usage.Usage) bool {
	redeemer, ok := t.Provider.(provider.ResetCreditRedeemer)
	if !t.AutoRedeem || !ok || s.dryRun {
		return false
	}
	name := t.Provider.Name()
	rctx, cancel := context.WithTimeout(ctx, readTimeout)
	outcome, err := redeemer.AutoRedeemResetCredit(rctx, u)
	cancel()
	switch {
	case err != nil:
		s.log.Printf("[%s] reset credit redeem failed: %v", name, err)
	case outcome == provider.RedeemReset:
		s.log.Printf("[%s] redeemed an expiring reset credit; rate-limit windows reset", name)
		s.notify(name+": reset credit redeemed", "An expiring reset credit was spent; the windows are reset")
		return true
	case outcome != "":
		s.log.Printf("[%s] reset credit not spent: %s", name, outcome)
	}
	return false
}

func activeProviderTask(ctx context.Context, p provider.Provider) (string, bool, error) {
	detector, ok := p.(provider.ActiveTaskDetector)
	if !ok {
		return "", false, nil
	}
	return detector.ActiveTask(ctx)
}

func (s *Scheduler) notify(title, msg string) {
	if s.cfg.Notify {
		notify.Notify(title, msg)
	}
}

// triggerCost renders the token/cost tail for logs, e.g.
// " — 32934 tok (in 32792 / out 142), $0.0110".
func triggerCost(res *provider.TriggerResult) string {
	if res == nil || !res.HasUsage {
		return ""
	}
	s := fmt.Sprintf(" — %d tok (in %d / out %d)", res.TotalTokens, res.InputTokens, res.OutputTokens)
	if res.CostUSD > 0 {
		s += fmt.Sprintf(", $%.4f", res.CostUSD)
	}
	return s
}

func windowLen(w usage.Window) time.Duration {
	if w.WindowSeconds > 0 {
		return time.Duration(w.WindowSeconds) * time.Second
	}
	return defaultWindow
}

// weeklyExhaustedWait sleeps until the weekly reset, capped at
// weeklyExhaustedRecheck so an early reset is noticed.
func weeklyExhaustedWait(weekly usage.Window, resetBuffer time.Duration) time.Duration {
	wait := weekly.Remaining() + resetBuffer
	if wait <= 0 {
		return time.Minute
	}
	return min(wait, weeklyExhaustedRecheck)
}

func missingWindowRecheck(st providerScheduleState, weekly usage.Window, now time.Time, resetBuffer time.Duration) time.Duration {
	wait := weeklyOnlyRecheck
	if !st.ExpectedResetAt.IsZero() {
		untilExpected := st.ExpectedResetAt.Add(resetBuffer).Sub(now)
		if untilExpected > 0 && untilExpected < wait {
			wait = untilExpected
		}
	}
	if weeklyRemaining := weekly.Remaining(); weeklyRemaining > 0 && weeklyRemaining < wait {
		wait = weeklyRemaining + resetBuffer
	}
	if wait <= 0 {
		return time.Minute
	}
	return wait
}

func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > maxBackoff {
		return maxBackoff
	}
	return d
}

func usageRateLimitWait(retryAfter time.Time, now time.Time) time.Duration {
	if !retryAfter.IsZero() {
		wait := retryAfter.Sub(now)
		if wait > 0 {
			return wait
		}
	}
	return rateLimitPause
}

// sleepCtx sleeps for d and reports false if the context was cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
