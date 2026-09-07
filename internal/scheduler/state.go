package scheduler

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/wavever/CCLimitPing/internal/usage"
)

const schedulerStateVersion = 1

// providerScheduleState contains only non-secret timing metadata. Persisting it
// lets a restarted watcher distinguish a genuinely weekly-only account from a
// five-hour window that disappeared from the API immediately after resetting.
type providerScheduleState struct {
	WindowSeconds        int       `json:"window_seconds,omitempty"`
	ExpectedResetAt      time.Time `json:"expected_reset_at,omitempty"`
	LastPingAt           time.Time `json:"last_ping_at,omitempty"`
	AwaitingConfirmation bool      `json:"awaiting_confirmation,omitempty"`
}

type schedulerStateFile struct {
	Version   int                              `json:"version"`
	Providers map[string]providerScheduleState `json:"providers"`
}

type stateStore struct {
	mu        sync.Mutex
	path      string
	providers map[string]providerScheduleState
}

func newStateStore(path string) (*stateStore, error) {
	s := &stateStore{path: path, providers: make(map[string]providerScheduleState)}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	var f schedulerStateFile
	if err := json.Unmarshal(b, &f); err != nil {
		return s, err
	}
	if f.Providers != nil {
		s.providers = f.Providers
	}
	return s, nil
}

func (s *stateStore) get(name string) providerScheduleState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.providers[name]
}

func (s *stateStore) observeWindow(name string, w usage.Window) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.providers[name]
	if w.WindowSeconds > 0 {
		st.WindowSeconds = w.WindowSeconds
	}
	if !w.ResetsAt.IsZero() {
		st.ExpectedResetAt = w.ResetsAt
	}
	st.AwaitingConfirmation = false
	s.providers[name] = st
	return s.saveLocked()
}

func (s *stateStore) recordPing(name string, at time.Time, window time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.providers[name]
	st.LastPingAt = at
	st.WindowSeconds = int(window / time.Second)
	st.ExpectedResetAt = at.Add(window)
	st.AwaitingConfirmation = true
	s.providers[name] = st
	return s.saveLocked()
}

func (s *stateStore) disarmUnconfirmed(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.providers[name]
	st.AwaitingConfirmation = false
	st.ExpectedResetAt = time.Time{}
	s.providers[name] = st
	return s.saveLocked()
}

func (s *stateStore) saveLocked() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(schedulerStateFile{
		Version:   schedulerStateVersion,
		Providers: s.providers,
	}, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".scheduler-state-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	renameErr := os.Rename(tmpName, s.path)
	if renameErr == nil {
		return nil
	}
	// Windows does not replace an existing destination with os.Rename. Only
	// this watcher writes the state file, so a remove-and-retry is safe there.
	if runtime.GOOS != "windows" {
		return renameErr
	}
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(tmpName, s.path)
}
