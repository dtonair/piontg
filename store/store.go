package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const stateFileName = "state.json"

// ModelRef is the Pi model selection persisted by the bot.
type ModelRef struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
}

// State is the durable bot session metadata. Pi conversation content remains in
// Pi's own session files; this stores only enough metadata to reconnect.
type State struct {
	SelectedFolder string    `json:"selectedFolder,omitempty"`
	SelectedModel  *ModelRef `json:"selectedModel,omitempty"`
	SessionFile    string    `json:"sessionFile,omitempty"`
	SessionID      string    `json:"sessionId,omitempty"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// Store persists State as JSON under an application state directory.
type Store struct {
	dir  string
	path string
}

func New(dir string) *Store {
	return &Store{dir: dir, path: filepath.Join(dir, stateFileName)}
}

func (s *Store) Path() string { return s.path }

// Load returns an empty state when no state file exists. If the state file is
// corrupt, it attempts to load the .bak file and returns that state with an
// explanatory error. If no backup is usable, it returns an empty state and the
// corruption error.
func (s *Store) Load() (*State, error) {
	state, err := readStateFile(s.path)
	if err == nil {
		return state, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return &State{}, nil
	}

	primaryErr := err
	backup, backupErr := readStateFile(s.path + ".bak")
	if backupErr == nil {
		return backup, fmt.Errorf("state file is corrupt; loaded backup: %w", primaryErr)
	}
	return &State{}, fmt.Errorf("state file is corrupt and no usable backup exists: %w", primaryErr)
}

// Save writes state atomically using a temporary file and rename. When a prior
// state exists, it is retained as state.json.bak before replacement.
func (s *Store) Save(state *State) error {
	if state == nil {
		state = &State{}
	}
	state.UpdatedAt = time.Now().UTC()
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(s.dir, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp state: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp state: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp state: %w", err)
	}

	if _, err := os.Stat(s.path); err == nil {
		if err := copyFile(s.path, s.path+".bak", 0o600); err != nil {
			return fmt.Errorf("backup state: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat state: %w", err)
	}

	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	return nil
}

func readStateFile(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func copyFile(src, dst string, perm os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, perm)
}
