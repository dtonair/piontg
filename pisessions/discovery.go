package pisessions

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	EnvSessionDir     = "PI_CODING_AGENT_SESSION_DIR"
	defaultMaxResults = 20
	maxPreviewRunes   = 120
)

// Summary is the Telegram-safe metadata piontg needs to display and resume a
// Pi JSONL session. File is the absolute session file path; CWD is canonicalized
// by Options.ResolveFolder when a resolver is provided.
type Summary struct {
	File         string
	ID           string
	CWD          string
	Name         string
	Preview      string
	LastAt       time.Time
	MessageCount int
}

// Options controls session discovery.
type Options struct {
	// Dir is the Pi session directory. When empty, DefaultDir is used.
	Dir string
	// MaxResults caps returned summaries after policy filtering and newest-first
	// sorting. Values <= 0 use a conservative default.
	MaxResults int
	// ResolveFolder validates a session header cwd and returns its canonical form.
	// Sessions for which this returns an error are skipped.
	ResolveFolder func(path string) (canonical string, err error)
	// OnSkip is called for unreadable, corrupt, or policy-rejected candidates.
	// It is optional and intended for diagnostics only.
	OnSkip func(path string, err error)
}

// DefaultDir returns Pi's effective default session directory for this process.
// It mirrors Pi's documented PI_CODING_AGENT_SESSION_DIR override and otherwise
// falls back to ~/.pi/agent/sessions.
func DefaultDir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv(EnvSessionDir)); dir != "" {
		return expandHome(filepath.Clean(dir))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".pi", "agent", "sessions"), nil
}

func expandHome(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}

// Discover finds Pi JSONL sessions, filters them through the configured folder
// resolver, and returns newest sessions first.
func Discover(ctx context.Context, opts Options) ([]Summary, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	dir := strings.TrimSpace(opts.Dir)
	if dir == "" {
		var err error
		dir, err = DefaultDir()
		if err != nil {
			return nil, err
		}
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve session dir: %w", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat session dir: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("session dir %q is not a directory", dir)
	}

	var summaries []Summary
	walkErr := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if opts.OnSkip != nil {
				opts.OnSkip(path, err)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if d.IsDir() || !strings.EqualFold(filepath.Ext(path), ".jsonl") {
			return nil
		}
		summary, err := ReadSummary(path)
		if err != nil {
			if opts.OnSkip != nil {
				opts.OnSkip(path, err)
			}
			return nil
		}
		if opts.ResolveFolder != nil {
			canonical, err := opts.ResolveFolder(summary.CWD)
			if err != nil {
				if opts.OnSkip != nil {
					opts.OnSkip(path, err)
				}
				return nil
			}
			summary.CWD = canonical
		}
		summaries = append(summaries, summary)
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}

	sort.SliceStable(summaries, func(i, j int) bool {
		if summaries[i].LastAt.Equal(summaries[j].LastAt) {
			return summaries[i].File < summaries[j].File
		}
		return summaries[i].LastAt.After(summaries[j].LastAt)
	})
	maxResults := opts.MaxResults
	if maxResults <= 0 {
		maxResults = defaultMaxResults
	}
	if len(summaries) > maxResults {
		summaries = summaries[:maxResults]
	}
	return summaries, nil
}

// ReadSummary parses one Pi session JSONL file into display/resume metadata.
func ReadSummary(path string) (Summary, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Summary{}, fmt.Errorf("resolve session file: %w", err)
	}
	file, err := os.Open(abs)
	if err != nil {
		return Summary{}, err
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return Summary{}, err
	}

	reader := bufio.NewReader(file)
	headerLine, err := readJSONLRecord(reader)
	if err != nil {
		return Summary{}, fmt.Errorf("read session header: %w", err)
	}
	var header sessionHeader
	if err := json.Unmarshal(headerLine, &header); err != nil {
		return Summary{}, fmt.Errorf("parse session header: %w", err)
	}
	if header.Type != "session" {
		return Summary{}, fmt.Errorf("not a Pi session file")
	}
	if strings.TrimSpace(header.ID) == "" {
		return Summary{}, fmt.Errorf("session header missing id")
	}
	if strings.TrimSpace(header.CWD) == "" {
		return Summary{}, fmt.Errorf("session header missing cwd")
	}

	summary := Summary{File: abs, ID: header.ID, CWD: header.CWD, LastAt: stat.ModTime()}
	if ts, ok := parseTimestamp(header.Timestamp); ok {
		summary.LastAt = ts
	}

	for {
		line, err := readJSONLRecord(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return Summary{}, err
		}
		var env entryEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			continue
		}
		if ts, ok := parseTimestamp(env.Timestamp); ok && ts.After(summary.LastAt) {
			summary.LastAt = ts
		}
		switch env.Type {
		case "session_info":
			var info sessionInfoEntry
			if err := json.Unmarshal(line, &info); err == nil && strings.TrimSpace(info.Name) != "" {
				summary.Name = strings.TrimSpace(info.Name)
			}
		case "message":
			summary.MessageCount++
			if summary.Preview != "" {
				continue
			}
			var msg messageEntry
			if err := json.Unmarshal(line, &msg); err == nil && msg.Message.Role == "user" {
				summary.Preview = truncateRunes(extractText(msg.Message.Content), maxPreviewRunes)
			}
		}
	}
	return summary, nil
}

func readJSONLRecord(reader *bufio.Reader) ([]byte, error) {
	line, err := reader.ReadBytes('\n')
	if len(line) > 0 {
		text := strings.TrimRight(string(line), "\r\n")
		if strings.TrimSpace(text) == "" {
			if err != nil {
				return nil, err
			}
			return readJSONLRecord(reader)
		}
		return []byte(text), nil
	}
	return nil, err
}

type sessionHeader struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	CWD       string `json:"cwd"`
}

type entryEnvelope struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
}

type sessionInfoEntry struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

type messageEntry struct {
	Type    string       `json:"type"`
	Message agentMessage `json:"message"`
}

type agentMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func parseTimestamp(value string) (time.Time, bool) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, false
	}
	if ts, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return ts, true
	}
	return time.Time{}, false
}

func extractText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			parts = append(parts, strings.TrimSpace(block.Text))
		}
	}
	return strings.Join(parts, " ")
}

func truncateRunes(s string, max int) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}
