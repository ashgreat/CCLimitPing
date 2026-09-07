package scheduler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wavever/CCLimitPing/internal/usage"
)

func TestStateStorePersistsNonSecretTimingMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scheduler-state.json")
	s, err := newStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	reset := time.Now().Add(5 * time.Hour).Round(time.Second)
	if err := s.observeWindow("codex", usage.Window{ResetsAt: reset, WindowSeconds: 18000}); err != nil {
		t.Fatal(err)
	}

	reloaded, err := newStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got := reloaded.get("codex")
	if got.WindowSeconds != 18000 || !got.ExpectedResetAt.Equal(reset) {
		t.Fatalf("reloaded state = %#v", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %o, want 600", info.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"access_token", "refresh_token", "Authorization", "Bearer "} {
		if strings.Contains(string(b), needle) {
			t.Fatalf("state contains credential-like material %q", needle)
		}
	}
}
