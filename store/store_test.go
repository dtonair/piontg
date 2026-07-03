package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreSaveAndLoad(t *testing.T) {
	s := New(t.TempDir())
	input := &State{
		SelectedFolder: "/tmp/project",
		SelectedModel:  &ModelRef{Provider: "anthropic", ID: "claude-sonnet"},
		SessionFile:    "/tmp/session.jsonl",
		SessionID:      "abc123",
	}

	if err := s.Save(input); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if input.UpdatedAt.IsZero() {
		t.Fatal("Save() did not set UpdatedAt")
	}

	loaded, err := s.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.SelectedFolder != input.SelectedFolder {
		t.Fatalf("SelectedFolder = %q", loaded.SelectedFolder)
	}
	if loaded.SelectedModel == nil || loaded.SelectedModel.Provider != "anthropic" || loaded.SelectedModel.ID != "claude-sonnet" {
		t.Fatalf("SelectedModel = %#v", loaded.SelectedModel)
	}
	if loaded.SessionFile != input.SessionFile || loaded.SessionID != input.SessionID {
		t.Fatalf("loaded session = %#v", loaded)
	}
}

func TestStoreLoadMissingReturnsEmptyState(t *testing.T) {
	s := New(t.TempDir())
	loaded, err := s.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded == nil || loaded.SelectedFolder != "" || loaded.SelectedModel != nil {
		t.Fatalf("Load() = %#v", loaded)
	}
}

func TestStoreCreatesBackupAndLoadsBackupWhenPrimaryCorrupt(t *testing.T) {
	s := New(t.TempDir())
	first := &State{SelectedFolder: "/tmp/first"}
	second := &State{SelectedFolder: "/tmp/second"}
	if err := s.Save(first); err != nil {
		t.Fatalf("Save(first) error = %v", err)
	}
	if err := s.Save(second); err != nil {
		t.Fatalf("Save(second) error = %v", err)
	}
	if _, err := os.Stat(s.Path() + ".bak"); err != nil {
		t.Fatalf("backup missing: %v", err)
	}

	if err := os.WriteFile(s.Path(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Load()
	if err == nil {
		t.Fatal("Load() error = nil, want corruption warning")
	}
	if !strings.Contains(err.Error(), "loaded backup") {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.SelectedFolder != first.SelectedFolder {
		t.Fatalf("loaded backup folder = %q, want %q", loaded.SelectedFolder, first.SelectedFolder)
	}
}

func TestStoreCorruptWithoutBackupReturnsEmptyStateAndError(t *testing.T) {
	s := New(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(s.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Path(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := s.Load()
	if err == nil {
		t.Fatal("Load() error = nil")
	}
	if loaded == nil || loaded.SelectedFolder != "" {
		t.Fatalf("Load() state = %#v", loaded)
	}
}
