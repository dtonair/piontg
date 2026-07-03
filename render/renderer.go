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
	DefaultTypingInterval  = 4 * time.Second
)

type Sink interface {
	SendMessage(ctx context.Context, text string) (int, error)
	EditMessage(ctx context.Context, messageID int, text string) error
}

type TypingSink interface {
	SendTyping(ctx context.Context) error
}

type Renderer struct {
	sink           Sink
	maxRunes       int
	editInterval   time.Duration
	typingInterval time.Duration
	now            func() time.Time

	assistant  textStream
	lastTyping time.Time
}

type textStream struct {
	activeID int
	buffer   string
	lastEdit time.Time
	prefix   string
}

func New(sink Sink) *Renderer {
	return &Renderer{
		sink:           sink,
		maxRunes:       DefaultMaxMessageRunes,
		editInterval:   DefaultEditInterval,
		typingInterval: DefaultTypingInterval,
		now:            time.Now,
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

func (r *Renderer) SetTypingInterval(interval time.Duration) {
	if interval >= 0 {
		r.typingInterval = interval
	}
}

func (r *Renderer) HandleEvent(ctx context.Context, event pi.Event) error {
	switch event.Type {
	case "message_update":
		var parsed messageUpdateEvent
		if err := json.Unmarshal(event.Raw, &parsed); err != nil {
			return err
		}
		assistantEvent := parsed.AssistantMessageEvent
		delta := assistantEvent.delta()
		if isThinkingEventType(assistantEvent.Type) && delta != "" {
			return r.SendTyping(ctx)
		}
		if isTextEventType(assistantEvent.Type) && delta != "" {
			return r.AppendText(ctx, delta)
		}
	case "thinking_update", "thinking_delta", "reasoning_update", "reasoning_delta":
		var parsed streamingTextEvent
		if err := json.Unmarshal(event.Raw, &parsed); err != nil {
			return err
		}
		if delta := parsed.delta(); delta != "" {
			return r.SendTyping(ctx)
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
	return r.appendToStream(ctx, &r.assistant, delta)
}

func (r *Renderer) AppendThinking(ctx context.Context, _ string) error {
	return r.SendTyping(ctx)
}

func (r *Renderer) SendTyping(ctx context.Context) error {
	typingSink, ok := r.sink.(TypingSink)
	if !ok {
		return nil
	}
	now := r.now()
	if !r.lastTyping.IsZero() && now.Sub(r.lastTyping) < r.typingInterval {
		return nil
	}
	if err := typingSink.SendTyping(ctx); err != nil {
		return err
	}
	r.lastTyping = now
	return nil
}

func (r *Renderer) appendToStream(ctx context.Context, stream *textStream, delta string) error {
	remaining := delta
	for remaining != "" {
		space := r.streamCapacity(stream) - runeLen(stream.buffer)
		if space <= 0 {
			if err := r.finalizeChunk(ctx, stream); err != nil {
				return err
			}
			space = r.streamCapacity(stream)
		}
		part, rest := splitRunes(remaining, space)
		if part == "" && rest != "" {
			part, rest = rest, ""
		}
		stream.buffer += part
		if stream.activeID == 0 {
			id, err := r.sink.SendMessage(ctx, stream.render())
			if err != nil {
				return err
			}
			stream.activeID = id
			stream.lastEdit = r.now()
		} else if r.now().Sub(stream.lastEdit) >= r.editInterval {
			if err := r.sink.EditMessage(ctx, stream.activeID, stream.render()); err != nil {
				id, sendErr := r.sink.SendMessage(ctx, stream.render())
				if sendErr != nil {
					return fmt.Errorf("edit failed: %v; fallback send failed: %w", err, sendErr)
				}
				stream.activeID = id
			}
			stream.lastEdit = r.now()
		}
		remaining = rest
		if remaining != "" && runeLen(stream.buffer) >= r.streamCapacity(stream) {
			if err := r.finalizeChunk(ctx, stream); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Renderer) Flush(ctx context.Context) error {
	return r.flushStream(ctx, &r.assistant)
}

func (r *Renderer) flushStream(ctx context.Context, stream *textStream) error {
	if stream.activeID == 0 || stream.buffer == "" {
		stream.activeID = 0
		stream.buffer = ""
		return nil
	}
	if err := r.sink.EditMessage(ctx, stream.activeID, stream.render()); err != nil {
		_, sendErr := r.sink.SendMessage(ctx, stream.render())
		if sendErr != nil {
			return fmt.Errorf("flush edit failed: %v; fallback send failed: %w", err, sendErr)
		}
	}
	stream.activeID = 0
	stream.buffer = ""
	stream.lastEdit = time.Time{}
	return nil
}

func (r *Renderer) finalizeChunk(ctx context.Context, stream *textStream) error {
	if stream.activeID != 0 && stream.buffer != "" {
		if err := r.sink.EditMessage(ctx, stream.activeID, stream.render()); err != nil {
			if _, sendErr := r.sink.SendMessage(ctx, stream.render()); sendErr != nil {
				return fmt.Errorf("final edit failed: %v; fallback send failed: %w", err, sendErr)
			}
		}
	}
	stream.activeID = 0
	stream.buffer = ""
	stream.lastEdit = time.Time{}
	return nil
}

func (r *Renderer) streamCapacity(stream *textStream) int {
	capacity := r.maxRunes - runeLen(stream.prefix)
	if capacity <= 0 {
		return r.maxRunes
	}
	return capacity
}

func (s *textStream) render() string {
	if s.prefix == "" || s.buffer == "" {
		return s.buffer
	}
	return s.prefix + s.buffer
}

type messageUpdateEvent struct {
	AssistantMessageEvent streamingTextEvent `json:"assistantMessageEvent"`
}

type streamingTextEvent struct {
	Type      string       `json:"type"`
	Delta     textFragment `json:"delta"`
	Text      textFragment `json:"text"`
	Content   textFragment `json:"content"`
	Thinking  textFragment `json:"thinking"`
	Reasoning textFragment `json:"reasoning"`
	Thought   textFragment `json:"thought"`
}

type textFragment string

func (f *textFragment) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err == nil {
		*f = textFragment(value)
		return nil
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(data, &nested); err != nil {
		return nil
	}
	for _, key := range []string{"delta", "text", "content", "thinking", "reasoning", "thought"} {
		if raw, ok := nested[key]; ok {
			if err := json.Unmarshal(raw, &value); err == nil && value != "" {
				*f = textFragment(value)
				return nil
			}
		}
	}
	return nil
}

func (e streamingTextEvent) delta() string {
	for _, value := range []textFragment{e.Delta, e.Text, e.Content, e.Thinking, e.Reasoning, e.Thought} {
		if value != "" {
			return string(value)
		}
	}
	return ""
}

func isTextEventType(typ string) bool {
	return typ == "text_delta"
}

func isThinkingEventType(typ string) bool {
	typ = strings.ToLower(typ)
	return strings.Contains(typ, "thinking") || strings.Contains(typ, "reasoning") || strings.Contains(typ, "thought")
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
