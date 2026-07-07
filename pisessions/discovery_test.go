package pisessions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestReadSummaryParsesHeaderNamePreviewAndLastTimestamp(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "session.jsonl")
	content := strings.Join([]string{
		`{"type":"session","version":3,"id":"abc123","timestamp":"2026-07-07T10:00:00Z","cwd":"` + slashPath(dir) + `"}`,
		`{"type":"message","id":"m1","parentId":null,"timestamp":"2026-07-07T10:01:00Z","message":{"role":"user","content":[{"type":"text","text":"Please fix the Telegram session resume flow"}]}}`,
		`{"type":"message","id":"m2","parentId":"m1","timestamp":"2026-07-07T10:02:00Z","message":{"role":"assistant","content":[{"type":"text","text":"OK"}]}}`,
		`{"type":"session_info","id":"i1","parentId":"m2","timestamp":"2026-07-07T10:03:00Z","name":"Resume work"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	summary, err := ReadSummary(file)
	if err != nil {
		t.Fatalf("ReadSummary() error = %v", err)
	}
	if summary.File != fileAbs(t, file) || summary.ID != "abc123" || summary.CWD != slashPath(dir) {
		t.Fatalf("summary identity = %#v", summary)
	}
	if summary.Name != "Resume work" {
		t.Fatalf("Name = %q", summary.Name)
	}
	if summary.Preview != "Please fix the Telegram session resume flow" {
		t.Fatalf("Preview = %q", summary.Preview)
	}
	if summary.MessageCount != 2 {
		t.Fatalf("MessageCount = %d", summary.MessageCount)
	}
	want := time.Date(2026, 7, 7, 10, 3, 0, 0, time.UTC)
	if !summary.LastAt.Equal(want) {
		t.Fatalf("LastAt = %s, want %s", summary.LastAt, want)
	}
}

func TestReadSummaryRejectsInvalidHeaders(t *testing.T) {
	dir := t.TempDir()
	tests := map[string]string{
		"not_session": `{"type":"message"}`,
		"missing_id":  `{"type":"session","cwd":"/tmp"}`,
		"missing_cwd": `{"type":"session","id":"abc"}`,
		"bad_json":    `{`,
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(dir, name+".jsonl")
			if err := os.WriteFile(file, []byte(content+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadSummary(file); err == nil {
				t.Fatalf("ReadSummary() error = nil")
			}
		})
	}
}

func TestDiscoverFiltersSortsAndLimits(t *testing.T) {
	dir := t.TempDir()
	allowed := filepath.Join(dir, "allowed")
	denied := filepath.Join(dir, "denied")
	if err := os.MkdirAll(filepath.Join(dir, "store", "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeSession(t, filepath.Join(dir, "store", "old.jsonl"), "old", allowed, "2026-07-07T09:00:00Z", "old work")
	writeSession(t, filepath.Join(dir, "store", "nested", "new.jsonl"), "new", allowed, "2026-07-07T11:00:00Z", "new work")
	writeSession(t, filepath.Join(dir, "store", "denied.jsonl"), "denied", denied, "2026-07-07T12:00:00Z", "secret")
	if err := os.WriteFile(filepath.Join(dir, "store", "bad.jsonl"), []byte("{\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "store", "ignore.txt"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}

	var skipped []string
	summaries, err := Discover(context.Background(), Options{
		Dir:        filepath.Join(dir, "store"),
		MaxResults: 1,
		OnSkip: func(path string, err error) {
			skipped = append(skipped, filepath.Base(path)+":"+err.Error())
		},
		ResolveFolder: func(path string) (string, error) {
			if sameClean(path, allowed) {
				return filepath.Clean(path), nil
			}
			return "", errors.New("outside allowed roots")
		},
	})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("len(summaries) = %d, want 1: %#v", len(summaries), summaries)
	}
	if summaries[0].ID != "new" {
		t.Fatalf("first summary ID = %q, want new", summaries[0].ID)
	}
	joined := strings.Join(skipped, "\n")
	if !strings.Contains(joined, "denied.jsonl") || !strings.Contains(joined, "bad.jsonl") {
		t.Fatalf("skipped = %#v, want denied and bad", skipped)
	}
}

func TestDiscoverMissingDirectoryReturnsEmpty(t *testing.T) {
	summaries, err := Discover(context.Background(), Options{Dir: filepath.Join(t.TempDir(), "missing")})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(summaries) != 0 {
		t.Fatalf("len(summaries) = %d", len(summaries))
	}
}

func TestDefaultDirHonorsEnvironment(t *testing.T) {
	t.Setenv(EnvSessionDir, "~/custom-pi-sessions")
	dir, err := DefaultDir()
	if err != nil {
		t.Fatalf("DefaultDir() error = %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "custom-pi-sessions")
	if dir != want {
		t.Fatalf("DefaultDir() = %q, want %q", dir, want)
	}
}

func writeSession(t *testing.T, file, id, cwd, ts, prompt string) {
	t.Helper()
	content := strings.Join([]string{
		`{"type":"session","version":3,"id":"` + id + `","timestamp":"` + ts + `","cwd":"` + slashPath(cwd) + `"}`,
		`{"type":"message","id":"m1","parentId":null,"timestamp":"` + ts + `","message":{"role":"user","content":"` + prompt + `"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func fileAbs(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func sameClean(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b) || slashPath(a) == slashPath(b)
}

func slashPath(path string) string {
	if runtime.GOOS == "windows" {
		return strings.ReplaceAll(path, `\`, `/`)
	}
	return path
}
