package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wavever/CCLimitPing/internal/config"
)

func TestClaudePingArgsDropsConflictingFlags(t *testing.T) {
	got := claudePingArgs([]string{
		"--max-turns", "1",
		"--output-format=json",
		"--tools", "Read",
		"--bare",
		"--permission-mode", "plan",
		"--json-schema", "{}",
	})
	want := []string{"--tools", "Read", "--permission-mode", "plan"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("interactive args = %#v, want %#v", got, want)
	}
}

func TestClaudeTriggerDryRunUsesPrintCommand(t *testing.T) {
	c := NewClaude(config.ProviderConfig{
		Prompt: ".",
		Model:  "haiku",
		ExtraArgs: []string{
			"--max-turns", "1",
			"--output-format", "json",
		},
	})

	res, err := c.Trigger(context.Background(), true)
	if err != nil {
		t.Fatalf("dry-run trigger: %v", err)
	}
	if res.Command != "claude -p --model haiku ." {
		t.Fatalf("command = %q, want %q", res.Command, "claude -p --model haiku .")
	}
	if !strings.Contains(res.Command, " -p") {
		t.Fatalf("command does not use print mode: %q", res.Command)
	}
}

func TestNormalizedClaudeVersion(t *testing.T) {
	if got := normalizedClaudeVersion("2.1.168 (Claude Code)\n"); got != "2.1.168" {
		t.Fatalf("version = %q, want %q", got, "2.1.168")
	}
	if got := normalizedClaudeVersion(" \n\t"); got != "" {
		t.Fatalf("empty version = %q, want empty", got)
	}
}

func claudeDeniedResponse(req *http.Request) *http.Response {
	return &http.Response{
		StatusCode: http.StatusForbidden,
		Body: io.NopCloser(strings.NewReader(`{
			"type":"error",
			"error":{
				"type":"permission_error",
				"message":"OAuth authentication is currently not allowed for this organization."
			}
		}`)),
		Request: req,
	}
}

func TestDiagnoseClaudeUsageErrorReportsSubscriptionAccess(t *testing.T) {
	requests := 0
	useTransport(t, func(req *http.Request) (*http.Response, error) {
		requests++
		if req.Method != http.MethodPost || req.URL.String() != claudeCountTokensURL {
			t.Fatalf("probe request = %s %s", req.Method, req.URL)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer oauth-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := req.Header.Get("anthropic-version"); got != claudeAPIVersion {
			t.Fatalf("anthropic-version = %q", got)
		}
		if got := req.Header.Get("anthropic-beta"); got != claudeOAuthBeta {
			t.Fatalf("anthropic-beta = %q", got)
		}
		if got := req.Header.Get("User-Agent"); !strings.HasPrefix(got, "claude-code/") {
			t.Fatalf("User-Agent = %q", got)
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != claudeAccessProbeBody {
			t.Fatalf("probe body = %s", body)
		}
		return claudeDeniedResponse(req), nil
	})

	original := &UsageHTTPError{
		StatusCode: http.StatusTooManyRequests,
		Body:       `{"error":{"type":"rate_limit_error"}}`,
		RetryAfter: time.Now().Add(time.Hour),
	}
	got := diagnoseClaudeUsageError(context.Background(), staticTokenSource{token: "oauth-token"}, original)

	var accessErr *ClaudeSubscriptionAccessError
	if !errors.As(got, &accessErr) {
		t.Fatalf("error = %T %v, want ClaudeSubscriptionAccessError", got, got)
	}
	// The 429 must stay reachable: the scheduler pauses reads on its
	// Retry-After instead of falling into generic backoff.
	var httpErr *UsageHTTPError
	if !errors.As(got, &httpErr) || httpErr != original {
		t.Fatalf("subscription error does not unwrap to the original 429: %v", got)
	}
	if requests != 1 {
		t.Fatalf("probe requests = %d, want 1", requests)
	}
}

func TestDiagnoseClaudeUsageErrorPreservesRealRateLimit(t *testing.T) {
	useTransport(t, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"input_tokens":1}`)),
			Request:    req,
		}, nil
	})

	original := &UsageHTTPError{
		StatusCode: http.StatusTooManyRequests,
		Body:       `{"error":"slow down"}`,
		RetryAfter: time.Now().Add(time.Hour),
	}
	got := diagnoseClaudeUsageError(context.Background(), staticTokenSource{token: "oauth-token"}, original)
	if got != original {
		t.Fatalf("error = %T %v, want original 429", got, got)
	}
}

func TestDiagnoseClaudeUsageErrorPreserves429WhenProbeIsInconclusive(t *testing.T) {
	cases := []struct {
		name      string
		response  *http.Response
		transport error
	}{
		{
			name: "unrelated 403",
			response: &http.Response{
				StatusCode: http.StatusForbidden,
				Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"permission_error","message":"model is restricted"}}`)),
			},
		},
		{
			name: "stale token",
			response: &http.Response{
				StatusCode: http.StatusUnauthorized,
				Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"authentication_error","message":"token expired"}}`)),
			},
		},
		{
			name: "probe rate limited",
			response: &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error"}}`)),
			},
		},
		{
			name:      "network failure",
			transport: errors.New("connection reset"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useTransport(t, func(req *http.Request) (*http.Response, error) {
				if tc.transport != nil {
					return nil, tc.transport
				}
				tc.response.Request = req
				return tc.response, nil
			})
			original := &UsageHTTPError{StatusCode: http.StatusTooManyRequests, Body: "original"}
			got := diagnoseClaudeUsageError(context.Background(), staticTokenSource{token: "oauth-token"}, original)
			if got != original {
				t.Fatalf("error = %T %v, want original 429", got, got)
			}
		})
	}
}

func TestDiagnoseClaudeUsageErrorDoesNotProbeOtherFailures(t *testing.T) {
	requests := 0
	useTransport(t, func(req *http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("must not be called")
	})
	original := &UsageHTTPError{StatusCode: http.StatusServiceUnavailable, Body: "maintenance"}
	if got := diagnoseClaudeUsageError(context.Background(), staticTokenSource{token: "oauth-token"}, original); got != original {
		t.Fatalf("error = %v, want original", got)
	}
	if requests != 0 {
		t.Fatalf("probe requests = %d, want 0", requests)
	}
}

func TestClaudeSubscriptionDeniedResponse(t *testing.T) {
	canonical := `{"error":{"type":"permission_error","message":"OAuth authentication is currently not allowed for this organization."}}`
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{name: "oauth denied 403", status: 403, body: canonical, want: true},
		{name: "oauth denied 401", status: 401, body: canonical, want: true},
		{name: "error code", status: 403, body: `{"error":{"code":"oauth_org_not_allowed"}}`, want: true},
		{name: "subscription disabled", status: 403,
			body: `{"error":{"message":"` + claudeDisabledText + `"}}`, want: true},
		{name: "unrelated denial", status: 403, body: `{"error":{"type":"permission_error","message":"model denied"}}`},
		{name: "wrong status", status: 429, body: canonical},
		{name: "empty body", status: 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeSubscriptionDeniedResponse(tc.status, []byte(tc.body)); got != tc.want {
				t.Fatalf("denied = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestClaudeOutputRecognizesSubscriptionDenial(t *testing.T) {
	// Claude Code renders this error inside a bordered box, so the sentence
	// reaches us coloured and word-wrapped.
	cases := []struct {
		name   string
		output string
		want   bool
	}{
		{
			name:   "single line",
			output: "\x1b[31mYour organization has disabled Claude subscription access for Claude Code\x1b[0m",
			want:   true,
		},
		{
			name: "wrapped in a TUI box",
			output: "╭────────────────────────────────────╮\r\n" +
				"│ \x1b[31mYour organization has disabled Claude\x1b[0m │\r\n" +
				"│ \x1b[31msubscription access for Claude Code\x1b[0m   │\r\n" +
				"╰────────────────────────────────────╯\r\n",
			want: true,
		},
		{name: "ordinary output", output: "Rate limited. Please try again later."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := claudeSubscriptionErrorFromOutput([]byte(tc.output))
			var accessErr *ClaudeSubscriptionAccessError
			if errors.As(err, &accessErr) != tc.want {
				t.Fatalf("error = %T %v, want denial = %t", err, err, tc.want)
			}
		})
	}
}

func TestClaudeDiagnosticTailStripsTerminalNoiseAndKeepsEnd(t *testing.T) {
	got := claudeDiagnosticTail([]byte("\x1b[31mstart\x1b[0m\r\n"+strings.Repeat("x", 20)+" useful error"), 18)
	if got != "…xxxxx useful error" {
		t.Fatalf("diagnostic tail = %q", got)
	}
}
