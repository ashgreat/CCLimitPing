package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type staticTokenSource struct {
	token string
}

func (s staticTokenSource) Token(context.Context) (string, error)   { return s.token, nil }
func (s staticTokenSource) Reload(context.Context) (string, error)  { return s.token, nil }
func (s staticTokenSource) Refresh(context.Context) (string, error) { return s.token, nil }

// useTransport points usageHTTPClient at fn for the duration of the test.
func useTransport(t *testing.T, fn roundTripFunc) {
	t.Helper()
	oldClient := usageHTTPClient
	usageHTTPClient = &http.Client{Transport: fn}
	t.Cleanup(func() { usageHTTPClient = oldClient })
}

func TestDoGetRetriesTransientNetworkError(t *testing.T) {
	oldClient := usageHTTPClient
	defer func() { usageHTTPClient = oldClient }()

	attempts := 0
	usageHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return nil, syscall.ECONNRESET
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			Request:    req,
		}, nil
	})}

	body, status, _, err := doGet(context.Background(), "token", func(token string) (*http.Request, error) {
		return http.NewRequest(http.MethodGet, "https://example.test/usage", nil)
	})
	if err != nil {
		t.Fatalf("doGet: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if string(body) != `{"ok":true}` {
		t.Fatalf("body = %q", body)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestDoGetDoesNotRetryUnauthorized(t *testing.T) {
	oldClient := usageHTTPClient
	defer func() { usageHTTPClient = oldClient }()

	attempts := 0
	usageHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Body:       io.NopCloser(strings.NewReader(`{"error":"expired"}`)),
			Request:    req,
		}, nil
	})}

	body, status, _, err := doGet(context.Background(), "token", func(token string) (*http.Request, error) {
		return http.NewRequest(http.MethodGet, "https://example.test/usage", nil)
	})
	if err != nil {
		t.Fatalf("doGet: %v", err)
	}
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", status, http.StatusUnauthorized)
	}
	if string(body) != `{"error":"expired"}` {
		t.Fatalf("body = %q", body)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestDoGetDoesNotRetryTooManyRequests(t *testing.T) {
	oldClient := usageHTTPClient
	defer func() { usageHTTPClient = oldClient }()

	attempts := 0
	usageHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Body:       io.NopCloser(strings.NewReader(`{"error":"slow down"}`)),
			Request:    req,
		}, nil
	})}

	body, status, _, err := doGet(context.Background(), "token", func(token string) (*http.Request, error) {
		return http.NewRequest(http.MethodGet, "https://example.test/usage", nil)
	})
	if err != nil {
		t.Fatalf("doGet: %v", err)
	}
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", status, http.StatusTooManyRequests)
	}
	if string(body) != `{"error":"slow down"}` {
		t.Fatalf("body = %q", body)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestFetchWithAuthPreservesRetryAfter(t *testing.T) {
	oldClient := usageHTTPClient
	defer func() { usageHTTPClient = oldClient }()

	usageHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Retry-After": []string{"42"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":"slow down"}`)),
			Request:    req,
		}, nil
	})}

	start := time.Now()
	_, err := fetchWithAuth(context.Background(), staticTokenSource{token: "token"}, func(token string) (*http.Request, error) {
		return http.NewRequest(http.MethodGet, "https://example.test/usage", nil)
	})
	var httpErr *UsageHTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("err = %T %v, want UsageHTTPError", err, err)
	}
	if httpErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", httpErr.StatusCode, http.StatusTooManyRequests)
	}
	if httpErr.RetryAfter.Before(start.Add(42*time.Second)) || httpErr.RetryAfter.After(time.Now().Add(43*time.Second)) {
		t.Fatalf("retry_after = %s, want about 42s from now", httpErr.RetryAfter)
	}
}

func TestUsageHTTPClientDisablesHTTP2(t *testing.T) {
	client := newUsageHTTPClient()
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", client.Transport)
	}
	if tr.ForceAttemptHTTP2 {
		t.Fatal("ForceAttemptHTTP2 = true, want false")
	}
	if tr.TLSNextProto == nil {
		t.Fatal("TLSNextProto = nil, want empty map to disable HTTP/2")
	}
	if len(tr.TLSNextProto) != 0 {
		t.Fatalf("TLSNextProto len = %d, want 0", len(tr.TLSNextProto))
	}
	if tr.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig = nil")
	}
	if len(tr.TLSClientConfig.NextProtos) != 1 || tr.TLSClientConfig.NextProtos[0] != "http/1.1" {
		t.Fatalf("NextProtos = %#v, want [http/1.1]", tr.TLSClientConfig.NextProtos)
	}
}

// rotatingTokenSource simulates the auth holders: Reload/Refresh hand out new
// tokens and count how often each path was taken.
type rotatingTokenSource struct {
	token      string
	reloadTo   string
	refreshTo  string
	refreshErr error
	reloads    int
	refreshes  int
}

func (s *rotatingTokenSource) Token(context.Context) (string, error) { return s.token, nil }

func (s *rotatingTokenSource) Reload(context.Context) (string, error) {
	s.reloads++
	if s.reloadTo != "" {
		s.token = s.reloadTo
	}
	return s.token, nil
}

func (s *rotatingTokenSource) Refresh(context.Context) (string, error) {
	s.refreshes++
	if s.refreshErr != nil {
		return "", s.refreshErr
	}
	s.token = s.refreshTo
	return s.token, nil
}

// authProbe serves 401 to every token except accept, recording the bearer
// tokens seen.
func authProbe(t *testing.T, accept string) *[]string {
	t.Helper()
	var seen []string
	useTransport(t, func(req *http.Request) (*http.Response, error) {
		token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		seen = append(seen, token)
		if token != accept {
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Body:       io.NopCloser(strings.NewReader(`{"error":"unauthorized"}`)),
				Request:    req,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			Request:    req,
		}, nil
	})
	return &seen
}

func bearerReq(token string) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodGet, "https://usage.test/usage", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return req, nil
}

func TestFetchWithAuthReloadRecoversFrom401(t *testing.T) {
	// The official CLI already rotated the token on disk: a reload is enough,
	// no OAuth refresh should happen.
	seen := authProbe(t, "tok-reloaded")
	src := &rotatingTokenSource{token: "tok-stale", reloadTo: "tok-reloaded"}

	body, err := fetchWithAuth(context.Background(), src, bearerReq)
	if err != nil {
		t.Fatalf("fetchWithAuth: %v", err)
	}
	if string(body) != `{"ok":true}` {
		t.Fatalf("body = %s", body)
	}
	if src.reloads != 1 || src.refreshes != 0 {
		t.Fatalf("reloads=%d refreshes=%d, want 1/0", src.reloads, src.refreshes)
	}
	if want := []string{"tok-stale", "tok-reloaded"}; strings.Join(*seen, ",") != strings.Join(want, ",") {
		t.Fatalf("tokens seen = %v, want %v", *seen, want)
	}
}

func TestFetchWithAuthRefreshesWhenReloadDoesNotHelp(t *testing.T) {
	// Reload returns the same stale token, so fetchWithAuth must fall through
	// to the OAuth refresh and retry with its result.
	seen := authProbe(t, "tok-refreshed")
	src := &rotatingTokenSource{token: "tok-stale", refreshTo: "tok-refreshed"}

	body, err := fetchWithAuth(context.Background(), src, bearerReq)
	if err != nil {
		t.Fatalf("fetchWithAuth: %v", err)
	}
	if string(body) != `{"ok":true}` {
		t.Fatalf("body = %s", body)
	}
	if src.reloads != 1 || src.refreshes != 1 {
		t.Fatalf("reloads=%d refreshes=%d, want 1/1", src.reloads, src.refreshes)
	}
	if want := []string{"tok-stale", "tok-refreshed"}; strings.Join(*seen, ",") != strings.Join(want, ",") {
		t.Fatalf("tokens seen = %v, want %v", *seen, want)
	}
}

func TestFetchWithAuthReportsRefreshFailure(t *testing.T) {
	authProbe(t, "never-issued")
	src := &rotatingTokenSource{token: "tok-stale", refreshErr: errors.New("grant revoked")}

	_, err := fetchWithAuth(context.Background(), src, bearerReq)
	if err == nil || !strings.Contains(err.Error(), "refresh failed") || !strings.Contains(err.Error(), "grant revoked") {
		t.Fatalf("error = %v, want unauthorized+refresh failure", err)
	}
	// The endpoint's 401 body says why the token was rejected (expired vs
	// revoked); keep it in the message.
	if !strings.Contains(err.Error(), `{"error":"unauthorized"}`) {
		t.Fatalf("error = %v, want the 401 body included", err)
	}
}

func TestUsageHTTPErrorMessage(t *testing.T) {
	withBody := &UsageHTTPError{StatusCode: 429, Body: "slow down"}
	if got := withBody.Error(); got != "usage endpoint returned HTTP 429: slow down" {
		t.Fatalf("Error() = %q", got)
	}
	bare := &UsageHTTPError{StatusCode: 503}
	if got := bare.Error(); got != "usage endpoint returned HTTP 503" {
		t.Fatalf("Error() = %q", got)
	}
}
