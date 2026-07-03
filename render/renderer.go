package render

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"piontg/pi"
)

const (
	DefaultMaxMessageRunes = 4096
	DefaultEditInterval    = 1500 * time.Millisecond
)

type Sink interface {
	SendMessage(ctx context.Context, text string) (int, error)
	EditMessage(ctx context.Context, messageID int, text string) error
}

type Renderer struct {
	sink         Sink
	maxRunes     int
	editInterval time.Duration
	now          func() time.Time

	activeID int
	buffer   string
	lastEdit time.Time
}

func New(sink Sink) *Renderer {
	return &Renderer{
		sink:         sink,
		maxRunes:     DefaultMaxMessageRunes,
		editInterval: DefaultEditInterval,
		now:          time.Now,
	}
}

func (r *Renderer) SetLimits(maxRunes int, editInterval time.Duration) {
	if maxRunes > 0 {
		r.maxRunes = maxRunes
	}
	if editInterval >= 0 {
		r.editInterval = editInterval
	}
}

func (r *Renderer) SetClock(now func() time.Time) {
	if now != nil {
		r.now = now
	}
}

func (r *Renderer) HandleEvent(ctx context.Context, event pi.Event) error {
	switch event.Type {
	case "message_update":
		var parsed messageUpdateEvent
		if err := json.Unmarshal(event.Raw, &parsed); err != nil {
			return err
		}
		if parsed.AssistantMessageEvent.Type == "text_delta" && parsed.AssistantMessageEvent.Delta != "" {
			return r.AppendText(ctx, parsed.AssistantMessageEvent.Delta)
		}
	case "tool_execution_start":
		var parsed toolStartEvent
		if err := json.Unmarshal(event.Raw, &parsed); err != nil {
			return err
		}
		_, err := r.sink.SendMessage(ctx, formatToolStart(parsed))
		return err
	case "tool_execution_end":
		var parsed toolEndEvent
		if err := json.Unmarshal(event.Raw, &parsed); err != nil {
			return err
		}
		if !parsed.IsError {
			return nil
		}
		_, err := r.sink.SendMessage(ctx, formatToolEnd(parsed))
		return err
	case "agent_end":
		return r.Flush(ctx)
	}
	return nil
}

func (r *Renderer) AppendText(ctx context.Context, delta string) error {
	remaining := delta
	for remaining != "" {
		space := r.maxRunes - runeLen(r.buffer)
		if space <= 0 {
			if err := r.finalizeChunk(ctx); err != nil {
				return err
			}
			space = r.maxRunes
		}
		part, rest := splitRunes(remaining, space)
		if part == "" && rest != "" {
			part, rest = rest, ""
		}
		r.buffer += part
		if r.activeID == 0 {
			id, err := r.sink.SendMessage(ctx, r.buffer)
			if err != nil {
				return err
			}
			r.activeID = id
			r.lastEdit = r.now()
		} else if r.now().Sub(r.lastEdit) >= r.editInterval {
			if err := r.sink.EditMessage(ctx, r.activeID, r.buffer); err != nil {
				id, sendErr := r.sink.SendMessage(ctx, r.buffer)
				if sendErr != nil {
					return fmt.Errorf("edit failed: %v; fallback send failed: %w", err, sendErr)
				}
				r.activeID = id
			}
			r.lastEdit = r.now()
		}
		remaining = rest
		if remaining != "" && runeLen(r.buffer) >= r.maxRunes {
			if err := r.finalizeChunk(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Renderer) Flush(ctx context.Context) error {
	if r.activeID == 0 || r.buffer == "" {
		r.activeID = 0
		r.buffer = ""
		return nil
	}
	if err := r.sink.EditMessage(ctx, r.activeID, r.buffer); err != nil {
		_, sendErr := r.sink.SendMessage(ctx, r.buffer)
		if sendErr != nil {
			return fmt.Errorf("flush edit failed: %v; fallback send failed: %w", err, sendErr)
		}
	}
	r.activeID = 0
	r.buffer = ""
	r.lastEdit = time.Time{}
	return nil
}

func (r *Renderer) finalizeChunk(ctx context.Context) error {
	if r.activeID != 0 && r.buffer != "" {
		if err := r.sink.EditMessage(ctx, r.activeID, r.buffer); err != nil {
			if _, sendErr := r.sink.SendMessage(ctx, r.buffer); sendErr != nil {
				return fmt.Errorf("final edit failed: %v; fallback send failed: %w", err, sendErr)
			}
		}
	}
	r.activeID = 0
	r.buffer = ""
	r.lastEdit = time.Time{}
	return nil
}

type messageUpdateEvent struct {
	AssistantMessageEvent struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
	} `json:"assistantMessageEvent"`
}

type toolStartEvent struct {
	ToolName string         `json:"toolName"`
	Args     map[string]any `json:"args"`
}

type toolEndEvent struct {
	ToolName string `json:"toolName"`
	IsError  bool   `json:"isError"`
}

func formatToolStart(event toolStartEvent) string {
	summary, language := formatToolArgs(event)
	if summary == "" {
		return fmt.Sprintf("🔧 %s started", event.ToolName)
	}
	return fmt.Sprintf("🔧 %s\n```%s\n%s\n```", event.ToolName, language, summary)
}

func formatToolArgs(event toolStartEvent) (summary, language string) {
	if event.ToolName == "bash" {
		if command, ok := event.Args["command"].(string); ok && strings.TrimSpace(command) != "" {
			return truncate(command, 500), "bash"
		}
	}
	args, _ := json.MarshalIndent(event.Args, "", "  ")
	summary = truncate(string(args), 500)
	if summary == "null" || summary == "{}" {
		return "", ""
	}
	return summary, "json"
}

func formatToolEnd(event toolEndEvent) string {
	if event.IsError {
		return fmt.Sprintf("❌ %s failed", event.ToolName)
	}
	return fmt.Sprintf("✅ %s done", event.ToolName)
}

func runeLen(s string) int { return len([]rune(s)) }

func splitRunes(s string, n int) (string, string) {
	if n <= 0 {
		return "", s
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s, ""
	}
	return string(runes[:n]), string(runes[n:])
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if runeLen(s) <= max {
		return s
	}
	part, _ := splitRunes(s, max-1)
	return part + "…"
}
