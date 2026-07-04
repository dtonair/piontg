package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAppliesDefaultsAndExpandsPaths(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "projects")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	writeConfig(t, cfgPath, `
telegram:
  token: test-token
  allowedUserId: 42
state:
  dir: ./state
pi: {}
folders:
  roots:
    - path: ./projects
`)

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldWD) }()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Pi.Binary != "pi" {
		t.Fatalf("Pi.Binary = %q", cfg.Pi.Binary)
	}
	if cfg.Pi.DefaultTrust != TrustNoApprove {
		t.Fatalf("DefaultTrust = %q", cfg.Pi.DefaultTrust)
	}
	if cfg.Pi.DefaultStreamingBehavior != StreamingFollowUp {
		t.Fatalf("DefaultStreamingBehavior = %q", cfg.Pi.DefaultStreamingBehavior)
	}
	if cfg.Folders.MaxDepth != 4 || cfg.Folders.MaxEntries != 200 {
		t.Fatalf("folder defaults = depth %d entries %d", cfg.Folders.MaxDepth, cfg.Folders.MaxEntries)
	}
	wantStateDir, err := filepath.EvalSymlinks(filepath.Join(dir, "state"))
	if err != nil {
		// The directory does not exist yet; canonicalize the parent for platforms like macOS /var -> /private/var.
		parent, parentErr := filepath.EvalSymlinks(dir)
		if parentErr != nil {
			t.Fatal(parentErr)
		}
		wantStateDir = filepath.Join(parent, "state")
	}
	if cfg.State.Dir != wantStateDir {
		t.Fatalf("State.Dir = %q, want %q", cfg.State.Dir, wantStateDir)
	}
	wantRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Folders.Roots[0].Path != wantRoot {
		t.Fatalf("root path = %q, want %q", cfg.Folders.Roots[0].Path, wantRoot)
	}
	if cfg.Folders.Roots[0].Trust != TrustNoApprove {
		t.Fatalf("root trust = %q", cfg.Folders.Roots[0].Trust)
	}
}

func TestLoadReadsTokenFromEnvironment(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeConfig(t, cfgPath, `
telegram:
  tokenEnv: PIONTG_TEST_TOKEN
  allowedUserId: 42
pi: {}
folders:
  roots:
    - path: ./
`)
	t.Setenv("PIONTG_TEST_TOKEN", "from-env")

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Telegram.Token != "from-env" {
		t.Fatalf("Token = %q", cfg.Telegram.Token)
	}
}

func TestValidateRejectsUnsafeOrIncompleteConfig(t *testing.T) {
	cfg := Config{
		Telegram: TelegramConfig{Token: "", AllowedUserID: 0},
		Pi:       PiConfig{Binary: "pi", DefaultTrust: "bad", DefaultStreamingBehavior: "bad"},
		State:    StateConfig{Dir: "/tmp/state"},
		Folders:  FoldersConfig{MaxDepth: 0, MaxEntries: 0, Roots: []FolderRoot{{Path: "relative", Trust: "bad"}}},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil")
	}
	text := err.Error()
	for _, want := range []string{"telegram token", "allowedUserId", "defaultTrust", "defaultStreamingBehavior", "maxDepth", "maxEntries", "absolute path", "trust"} {
		if !strings.Contains(text, want) {
			t.Fatalf("Validate() error %q missing %q", text, want)
		}
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeConfig(t, cfgPath, `
telegram:
  token: test-token
  allowedUserId: 42
unknown: true
folders:
  roots:
    - path: ./
`)

	_, err := Load(cfgPath)
	if err == nil {
		t.Fatal("Load() error = nil")
	}
	if !strings.Contains(err.Error(), "field unknown not found") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadRejectsRemovedSessionDirField(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeConfig(t, cfgPath, `
telegram:
  token: test-token
  allowedUserId: 42
pi:
  sessionDir: ./sessions
folders:
  roots:
    - path: ./
`)

	_, err := Load(cfgPath)
	if err == nil {
		t.Fatal("Load() error = nil")
	}
	if !strings.Contains(err.Error(), "field sessionDir not found") {
		t.Fatalf("Load() error = %v", err)
	}
}

func writeConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
