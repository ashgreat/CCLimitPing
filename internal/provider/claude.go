package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/wavever/CCLimitPing/internal/activity"
	"github.com/wavever/CCLimitPing/internal/auth"
	"github.com/wavever/CCLimitPing/internal/config"
	"github.com/wavever/CCLimitPing/internal/usage"
)

const (
	claudeUsageURL       = "https://api.anthropic.com/api/oauth/usage"
	claudeCountTokensURL = "https://api.anthropic.com/v1/messages/count_tokens"
	claudeOAuthBeta      = "oauth-2025-04-20"
	claudeAPIVersion     = "2023-06-01"
	claudeFallbackVer    = "2.1.0"
	claudeFiveHourSec    = 5 * 60 * 60
	claudeWeeklySec      = 7 * 24 * 60 * 60

	// The usage endpoint collapses a disabled subscription into a generic 429.
	// Token counting is free, creates no Message, and has an independent rate
	// limit, so it is a safe authorization probe when that 429 leaves the account
	// state ambiguous.
	claudeAccessProbeBody = `{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"."}]}`
	claudeOAuthOrgDenied  = "OAuth authentication is currently not allowed for this organization"
	claudeOAuthOrgCode    = "oauth_org_not_allowed"
	claudeDisabledText    = "Your organization has disabled Claude subscription access for Claude Code"
	claudeProbeTimeout    = 10 * time.Second
)

var (
	claudeUserAgentOnce sync.Once
	claudeUserAgent     string
	claudeANSIEscapeRE  = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	claudeNonTextRE     = regexp.MustCompile(`[^a-z0-9_]+`)
)

// ClaudeSubscriptionAccessError means Anthropic accepted the OAuth identity
// but explicitly rejected Claude Code subscription authentication. It wraps the
// failure it was diagnosed from (if any), so status-aware callers — e.g. the
// scheduler honoring a usage 429's Retry-After — keep seeing it.
type ClaudeSubscriptionAccessError struct{ Err error }

func (*ClaudeSubscriptionAccessError) Error() string {
	return "Claude subscription access is unavailable (the plan may have expired, or an organization admin may have disabled Claude Code); renew or re-enable the subscription, or use an Anthropic API key in Claude Code"
}

func (e *ClaudeSubscriptionAccessError) Unwrap() error { return e.Err }

// Claude reads usage via the OAuth usage endpoint and triggers windows via
// Claude Code's non-interactive print mode. Unlike a synthetic PTY session,
// print mode has a clear exit status and works reliably from a LaunchAgent.
type Claude struct {
	cfg  config.ProviderConfig
	auth *auth.ClaudeAuth
}

func NewClaude(cfg config.ProviderConfig) *Claude {
	return &Claude{cfg: cfg, auth: auth.NewClaudeAuth(cfg.RefreshCredentials)}
}

func (c *Claude) Name() string { return "claude" }

func (c *Claude) ActiveTask(_ context.Context) (string, bool, error) {
	// Active-session detection relies entirely on the CLI hooks (see `limitping
	// hooks install`). Without them we don't guess from the process list — the
	// scheduler just pings.
	if !activity.Enabled("claude") {
		return "", false, nil
	}
	return activity.Active("claude")
}

type claudeWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

type claudeUsageResp struct {
	FiveHour claudeWindow `json:"five_hour"`
	SevenDay claudeWindow `json:"seven_day"`
}

func (c *Claude) ReadUsage(ctx context.Context) (*usage.Usage, error) {
	body, err := fetchWithAuth(ctx, c.auth, func(token string) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, claudeUsageURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("anthropic-beta", claudeOAuthBeta)
		req.Header.Set("User-Agent", claudeCodeUserAgent())
		return req, nil
	})
	if err != nil {
		return nil, diagnoseClaudeUsageError(ctx, c.auth, err)
	}

	var r claudeUsageResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("claude usage: parsing response: %w", err)
	}

	u := &usage.Usage{
		Provider:  "claude",
		FetchedAt: time.Now(),
		Raw:       body,
		FiveHour: usage.Window{
			UsedPercent:   r.FiveHour.Utilization,
			ResetsAt:      parseTime(r.FiveHour.ResetsAt),
			WindowSeconds: claudeFiveHourSec,
		},
		Weekly: usage.Window{
			UsedPercent:   r.SevenDay.Utilization,
			ResetsAt:      parseTime(r.SevenDay.ResetsAt),
			WindowSeconds: claudeWeeklySec,
		},
	}
	u.LimitReached = u.FiveHour.UsedPercent >= 100 || u.Weekly.UsedPercent >= 100
	return u, nil
}

// diagnoseClaudeUsageError resolves the ambiguity unique to Claude's OAuth
// usage endpoint: a disabled subscription can be returned as the same generic
// 429 used for a real endpoint throttle. Any inconclusive probe deliberately
// preserves the original error, preventing false subscription warnings.
func diagnoseClaudeUsageError(ctx context.Context, src tokenSource, usageErr error) error {
	var httpErr *UsageHTTPError
	if !errors.As(usageErr, &httpErr) || httpErr.StatusCode != http.StatusTooManyRequests {
		return usageErr
	}
	if claudeSubscriptionAccessUnavailable(ctx, src) {
		return &ClaudeSubscriptionAccessError{Err: usageErr}
	}
	return usageErr
}

// claudeSubscriptionAccessUnavailable checks the inference authorization gate
// through Anthropic's zero-cost token-counting endpoint. The token was just
// accepted by the usage endpoint (a 429 is not an auth failure), so no
// reload/refresh ladder is needed here: anything but an explicit denial is
// inconclusive and leaves the original error untouched.
func claudeSubscriptionAccessUnavailable(ctx context.Context, src tokenSource) bool {
	probeCtx, cancel := context.WithTimeout(ctx, claudeProbeTimeout)
	defer cancel()

	token, err := src.Token(probeCtx)
	if err != nil || token == "" {
		return false
	}
	req, err := http.NewRequestWithContext(probeCtx, http.MethodPost, claudeCountTokensURL,
		strings.NewReader(claudeAccessProbeBody))
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", claudeAPIVersion)
	req.Header.Set("anthropic-beta", claudeOAuthBeta)
	req.Header.Set("User-Agent", claudeCodeUserAgent())

	resp, err := usageHTTPClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return false
	}
	return claudeSubscriptionDeniedResponse(resp.StatusCode, body)
}

func claudeSubscriptionDeniedResponse(status int, body []byte) bool {
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		return false
	}
	return claudeAccessDenied(string(body))
}

// claudeAccessDenied matches Anthropic's subscription-denial wording in both an
// API error body and Claude Code's own rendered output. Everything that is not
// alphanumeric is collapsed first — ANSI escapes, JSON punctuation, and the
// line breaks and box borders the TUI injects when it wraps these sentences —
// so one matcher serves both and neither casing nor wrapping defeats it.
func claudeAccessDenied(text string) bool {
	plain := claudeNormalizedText(text)
	for _, denial := range []string{claudeDisabledText, claudeOAuthOrgDenied, claudeOAuthOrgCode} {
		if strings.Contains(plain, claudeNormalizedText(denial)) {
			return true
		}
	}
	return false
}

func claudeNormalizedText(text string) string {
	stripped := claudeANSIEscapeRE.ReplaceAllString(text, " ")
	return claudeNonTextRE.ReplaceAllString(strings.ToLower(stripped), " ")
}

func claudeCodeUserAgent() string {
	claudeUserAgentOnce.Do(func() {
		claudeUserAgent = "claude-code/" + claudeFallbackVer

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		out, err := exec.CommandContext(ctx, "claude", "--version").Output()
		if err != nil {
			return
		}
		if version := normalizedClaudeVersion(string(out)); version != "" {
			claudeUserAgent = "claude-code/" + version
		}
	})
	return claudeUserAgent
}

func normalizedClaudeVersion(raw string) string {
	fields := strings.Fields(strings.TrimSpace(raw))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func (c *Claude) Trigger(ctx context.Context, dryRun bool) (*TriggerResult, error) {
	prompt := c.cfg.Prompt
	if prompt == "" {
		prompt = "."
	}
	args := []string{"-p"}
	if c.cfg.Model != "" {
		args = append(args, "--model", c.cfg.Model)
	}
	args = append(args, claudePingArgs(c.cfg.ExtraArgs)...)
	args = append(args, prompt)

	res := &TriggerResult{Command: "claude " + shellJoin(args)}
	if dryRun {
		return res, nil
	}

	output, err := exec.CommandContext(ctx, "claude", args...).CombinedOutput()
	if accessErr := claudeSubscriptionErrorFromOutput(output); accessErr != nil {
		return res, accessErr
	}
	if err == nil {
		return res, nil
	}
	detail := claudeDiagnosticTail(output, 500)
	if detail == "" {
		return res, fmt.Errorf("claude print failed: %w", err)
	}
	return res, fmt.Errorf("claude print failed: %w: %s", err, detail)
}

// claudeSubscriptionErrorFromOutput reports the denial Claude Code printed
// itself: it exits cleanly after showing this error, so without it a ping that
// started no window would be reported as a success.
func claudeSubscriptionErrorFromOutput(raw []byte) error {
	if claudeAccessDenied(string(raw)) {
		return &ClaudeSubscriptionAccessError{}
	}
	return nil
}

func claudePingArgs(extra []string) []string {
	out := make([]string, 0, len(extra))
	for i := 0; i < len(extra); i++ {
		arg := extra[i]
		flag, inlineValue := splitFlagValue(arg)
		if claudePingUnsupportedValueArg(flag) {
			if !inlineValue && i+1 < len(extra) {
				i++
			}
			continue
		}
		if claudePingUnsupportedArg(flag) {
			continue
		}
		out = append(out, arg)
	}
	return out
}

func splitFlagValue(arg string) (flag string, inlineValue bool) {
	if strings.HasPrefix(arg, "--") {
		if i := strings.Index(arg, "="); i > 0 {
			return arg[:i], true
		}
	}
	return arg, false
}

func claudePingUnsupportedArg(flag string) bool {
	switch flag {
	case "-p", "--print", "--bare", "--init", "--maintenance", "--include-hook-events",
		"--include-partial-messages", "--replay-user-messages", "--prompt-suggestions",
		"--no-session-persistence":
		return true
	default:
		return false
	}
}

func claudePingUnsupportedValueArg(flag string) bool {
	switch flag {
	case "--output-format", "--input-format", "--json-schema", "--max-turns",
		"--max-budget-usd", "--permission-prompt-tool", "--fallback-model":
		return true
	default:
		return false
	}
}

func claudeDiagnosticTail(raw []byte, limit int) string {
	plain := claudeANSIEscapeRE.ReplaceAllString(string(raw), " ")
	plain = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, plain)
	plain = strings.Join(strings.Fields(plain), " ")
	if len(plain) <= limit {
		return plain
	}
	return "…" + plain[len(plain)-limit:]
}

type limitedBuffer struct {
	mu      sync.Mutex
	limit   int
	buf     []byte
	changed time.Time // time of the last write, used to detect when output goes quiet
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if b.limit > 0 && len(b.buf) > b.limit {
		b.buf = b.buf[len(b.buf)-b.limit:]
	}
	b.changed = time.Now()
	return len(p), nil
}

func (b *limitedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf...)
}

// changedAt reports when output last arrived; the zero value means no output yet.
func (b *limitedBuffer) changedAt() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.changed
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}
