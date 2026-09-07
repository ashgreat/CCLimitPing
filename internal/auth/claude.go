// Package auth loads the OAuth credentials that Claude Code and Codex already
// store on disk / in the Keychain. Optional refresh is explicit; the default is
// read-only credential access.
package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// claudeOAuthClientID is Claude Code's public OAuth client id, used for the
// refresh-token grant. Refresh rotates the token in the same store the official
// CLI reads, keeping both in sync. If Anthropic changes this, update here.
const claudeOAuthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

const (
	claudeKeychainService = "Claude Code-credentials"
	claudeTokenEndpoint   = "https://console.anthropic.com/v1/oauth/token"
	macSecurityTool       = "/usr/bin/security"
)

// authHTTPClient performs the OAuth refresh requests; swapped in tests (the
// same seam the provider package uses for its usage client).
var authHTTPClient = http.DefaultClient

// claudeKeychainEnabled gates the macOS Keychain path. Tests disable it so
// they never read from — or write fake credentials into — the real Keychain.
var claudeKeychainEnabled = runtime.GOOS == "darwin"

// ClaudeAuth provides a current Claude access token, reloading from the store
// (Keychain on macOS, ~/.claude/.credentials.json elsewhere). It refreshes via
// the refresh token only when explicitly enabled.
type ClaudeAuth struct {
	mu           sync.Mutex
	allowRefresh bool
	access       string
	refresh      string
	account      string         // Keychain account, needed for write-back (macOS)
	root         map[string]any // full credential object, including unrelated top-level fields
	wrapper      map[string]any // full "claudeAiOauth" object, preserved on write-back
	wrapped      bool           // whether wrapper lives under root["claudeAiOauth"]
}

// NewClaudeAuth returns an empty holder; the token is loaded lazily. Credential
// refresh and write-back are disabled unless allowRefresh is explicitly true.
func NewClaudeAuth(allowRefresh ...bool) *ClaudeAuth {
	a := &ClaudeAuth{}
	if len(allowRefresh) > 0 {
		a.allowRefresh = allowRefresh[0]
	}
	return a
}

// Token returns a cached access token, loading from the store on first use.
func (a *ClaudeAuth) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.access != "" {
		return a.access, nil
	}
	if err := a.loadLocked(); err != nil {
		return "", err
	}
	return a.access, nil
}

// Reload forces a re-read from the store (e.g. the official CLI may have
// refreshed the token since we last read it) and returns the fresh token.
func (a *ClaudeAuth) Reload(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.loadLocked(); err != nil {
		return "", err
	}
	return a.access, nil
}

// Refresh exchanges the refresh token for a new access token and writes the
// rotated credentials back to the store so the official CLI stays in sync.
func (a *ClaudeAuth) Refresh(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.allowRefresh {
		return "", fmt.Errorf("claude credential refresh is disabled; log in with Claude Code again or set claude.refresh_credentials = true")
	}
	if a.refresh == "" {
		if err := a.loadLocked(); err != nil {
			return "", err
		}
	}
	if a.refresh == "" {
		return "", fmt.Errorf("claude: no refresh token available")
	}

	body, _ := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": a.refresh,
		"client_id":     claudeOAuthClientID,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, claudeTokenEndpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := authHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("claude token refresh: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("claude token refresh: HTTP %d", resp.StatusCode)
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", err
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("claude token refresh: empty access_token")
	}
	a.access = tok.AccessToken
	if tok.RefreshToken != "" {
		a.refresh = tok.RefreshToken
	}
	a.persistLocked(tok.ExpiresIn)
	return a.access, nil
}

func (a *ClaudeAuth) loadLocked() error {
	raw, account, err := readClaudeBlob()
	if err != nil {
		return err
	}
	a.account = account
	var outer map[string]any
	if err := json.Unmarshal(raw, &outer); err != nil {
		return fmt.Errorf("claude credentials: invalid JSON: %w", err)
	}
	// Credentials are stored either as {"claudeAiOauth": {...}} or flat.
	wrapper := outer
	if inner, ok := outer["claudeAiOauth"].(map[string]any); ok {
		wrapper = inner
		a.wrapped = true
	} else {
		a.wrapped = false
	}
	a.root = outer
	a.wrapper = wrapper
	a.access, _ = wrapper["accessToken"].(string)
	a.refresh, _ = wrapper["refreshToken"].(string)
	if a.access == "" {
		return fmt.Errorf("claude credentials: no accessToken found")
	}
	return nil
}

// persistLocked writes the updated tokens back to the same store, preserving
// other fields. Best-effort: a failure here doesn't break the running session,
// it just means we may have to refresh again next time.
func (a *ClaudeAuth) persistLocked(expiresIn int64) {
	if a.wrapper == nil {
		a.wrapper = map[string]any{}
	}
	a.wrapper["accessToken"] = a.access
	a.wrapper["refreshToken"] = a.refresh
	if expiresIn > 0 {
		a.wrapper["expiresAt"] = time.Now().Add(time.Duration(expiresIn) * time.Second).UnixMilli()
	}
	out := a.wrapper
	if a.wrapped {
		if a.root == nil {
			a.root = map[string]any{}
		}
		a.root["claudeAiOauth"] = a.wrapper
		out = a.root
	}
	blob, err := json.Marshal(out)
	if err != nil {
		return
	}
	_ = writeClaudeBlob(blob, a.account)
}

// readClaudeBlob returns the raw credentials JSON and (on macOS) the Keychain
// account name for write-back.
func readClaudeBlob() (raw []byte, account string, err error) {
	var keychainErr error
	if claudeKeychainEnabled {
		out, err := exec.Command(macSecurityTool, "find-generic-password",
			"-s", claudeKeychainService, "-w").CombinedOutput()
		if err == nil && len(bytes.TrimSpace(out)) > 0 {
			return bytes.TrimSpace(out), keychainAccount(), nil
		}
		keychainErr = claudeKeychainReadError(err, string(out))
		// fall through to file fallback
	}
	home, herr := os.UserHomeDir()
	if herr != nil {
		return nil, "", herr
	}
	path := filepath.Join(home, ".claude", ".credentials.json")
	b, ferr := os.ReadFile(path)
	if ferr != nil {
		if claudeKeychainEnabled {
			return nil, "", fmt.Errorf("%v; fallback credentials file %s is unavailable", keychainErr, path)
		}
		return nil, "", fmt.Errorf("claude credentials not found at %s: %w", path, ferr)
	}
	return b, "", nil
}

func claudeKeychainReadError(cmdErr error, output string) error {
	detail := strings.TrimSpace(output)
	lower := strings.ToLower(detail)
	switch {
	case strings.Contains(lower, "could not be found") || strings.Contains(lower, "item not found"):
		return fmt.Errorf("Claude Code credentials are missing from macOS Keychain; run `claude auth login`")
	case strings.Contains(lower, "interaction is not allowed") ||
		strings.Contains(lower, "user interaction is not allowed") ||
		strings.Contains(lower, "authorization was denied") ||
		strings.Contains(lower, "user canceled"):
		return fmt.Errorf("macOS Keychain denied background access to Claude Code credentials; authorize `%s find-generic-password -s %q -w` once in Terminal", macSecurityTool, claudeKeychainService)
	case cmdErr == nil:
		return fmt.Errorf("macOS Keychain returned an empty Claude Code credential")
	case detail != "":
		return fmt.Errorf("reading Claude Code credentials from macOS Keychain failed: %v (%s)", cmdErr, detail)
	default:
		return fmt.Errorf("reading Claude Code credentials from macOS Keychain failed: %v", cmdErr)
	}
}

var acctRe = regexp.MustCompile(`"acct"<blob>="([^"]*)"`)

// keychainAccount reads the account field of the Claude Code credentials item
// so we can update (not duplicate) it on write-back.
func keychainAccount() string {
	out, err := exec.Command(macSecurityTool, "find-generic-password",
		"-s", claudeKeychainService).CombinedOutput()
	if err != nil {
		return ""
	}
	if m := acctRe.FindSubmatch(out); m != nil {
		return string(m[1])
	}
	return ""
}

func writeClaudeBlob(blob []byte, account string) error {
	if claudeKeychainEnabled {
		// Put -w at the end so security reads the secret from stdin instead of a
		// process argument. This avoids exposing OAuth credentials through ps(1).
		args := []string{"add-generic-password", "-U", "-s", claudeKeychainService}
		if account != "" {
			args = append(args, "-a", account)
		}
		args = append(args, "-w")
		cmd := exec.Command(macSecurityTool, args...)
		cmd.Stdin = bytes.NewReader(append(blob, '\n'))
		return cmd.Run()
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path := filepath.Join(home, ".claude", ".credentials.json")
	return os.WriteFile(path, blob, 0o600)
}
