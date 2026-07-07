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
	DefaultTypingInterval  = 4 * time.Second
)

type Sink interface {
	SendMessage(ctx context.Context, text string) (int, error)
}

type TypingSink interface {
	SendTyping(ctx context.Context) error
}

type Renderer struct {
	sink           Sink
	maxRunes       int
	typingInterval time.Duration
	now            func() time.Time

	assistant  textStream
	lastTyping time.Time
}

type textStream struct {
	buffer     string
	messageKey string
}

func New(sink Sink) *Renderer {
	return &Renderer{
		sink:           sink,
		maxRunes:       DefaultMaxMessageRunes,
		typingInterval: DefaultTypingInterval,
		now:            time.Now,
	}
}

func (r *Renderer) SetLimits(maxRunes int, _ time.Duration) {
	if maxRunes > 0 {
		r.maxRunes = maxRunes
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
	case "agent_start":
		return r.Flush(ctx)
	case "message_update":
		var parsed messageUpdateEvent
		if err := json.Unmarshal(event.Raw, &parsed); err != nil {
			return err
		}
		assistantEvent := parsed.AssistantMessageEvent
		delta := assistantEvent.delta()
		messageKey := parsed.messageKey()
		if isThinkingEventType(assistantEvent.Type) && delta != "" {
			return r.SendTyping(ctx)
		}
		if isTextEventType(assistantEvent.Type) && delta != "" {
			if err := r.startMessage(ctx, &r.assistant, messageKey); err != nil {
				return err
			}
			if err := r.AppendText(ctx, delta); err != nil {
				return err
			}
		}
		if isMessageBoundaryEventType(assistantEvent.Type) {
			return r.Flush(ctx)
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
		if err := r.Flush(ctx); err != nil {
			return err
		}
		var parsed toolStartEvent
		if err := json.Unmarshal(event.Raw, &parsed); err != nil {
			return err
		}
		_, err := r.sink.SendMessage(ctx, formatToolStart(parsed))
		return err
	case "tool_execution_end":
		if err := r.Flush(ctx); err != nil {
			return err
		}
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

func (r *Renderer) startMessage(ctx context.Context, stream *textStream, messageKey string) error {
	if messageKey == "" {
		return nil
	}
	if stream.messageKey != "" && stream.messageKey != messageKey {
		if err := r.flushStream(ctx, stream); err != nil {
			return err
		}
	}
	stream.messageKey = messageKey
	return nil
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

func (r *Renderer) appendToStream(_ context.Context, stream *textStream, delta string) error {
	stream.buffer += delta
	return nil
}

func (r *Renderer) Flush(ctx context.Context) error {
	return r.flushStream(ctx, &r.assistant)
}

func (r *Renderer) flushStream(ctx context.Context, stream *textStream) error {
	if stream.buffer == "" {
		stream.messageKey = ""
		return nil
	}
	remaining := stream.render()
	for remaining != "" {
		part, rest := splitRunes(remaining, r.maxRunes)
		if part == "" && rest != "" {
			part, rest = rest, ""
		}
		if _, err := r.sink.SendMessage(ctx, part); err != nil {
			return err
		}
		remaining = rest
	}
	stream.buffer = ""
	stream.messageKey = ""
	return nil
}

func (s *textStream) render() string {
	return s.buffer
}

type messageUpdateEvent struct {
	MessageID             string             `json:"messageId"`
	MessageIDUpper        string             `json:"messageID"`
	MessageIDSnake        string             `json:"message_id"`
	Message               messageRef         `json:"message"`
	AssistantMessageEvent streamingTextEvent `json:"assistantMessageEvent"`
}

type streamingTextEvent struct {
	Type           string       `json:"type"`
	MessageID      string       `json:"messageId"`
	MessageIDUpper string       `json:"messageID"`
	MessageIDSnake string       `json:"message_id"`
	Message        messageRef   `json:"message"`
	Delta          textFragment `json:"delta"`
	Text           textFragment `json:"text"`
	Content        textFragment `json:"content"`
	Thinking       textFragment `json:"thinking"`
	Reasoning      textFragment `json:"reasoning"`
	Thought        textFragment `json:"thought"`
}

type messageRef struct {
	ID             string `json:"id"`
	MessageID      string `json:"messageId"`
	MessageIDUpper string `json:"messageID"`
	MessageIDSnake string `json:"message_id"`
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

func (e messageUpdateEvent) messageKey() string {
	for _, value := range []string{
		e.AssistantMessageEvent.MessageID,
		e.AssistantMessageEvent.MessageIDUpper,
		e.AssistantMessageEvent.MessageIDSnake,
		e.AssistantMessageEvent.Message.key(),
		e.MessageID,
		e.MessageIDUpper,
		e.MessageIDSnake,
		e.Message.key(),
	} {
		if value != "" {
			return value
		}
	}
	return ""
}

func (m messageRef) key() string {
	for _, value := range []string{m.MessageID, m.MessageIDUpper, m.MessageIDSnake, m.ID} {
		if value != "" {
			return value
		}
	}
	return ""
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

func isMessageBoundaryEventType(typ string) bool {
	switch strings.ToLower(typ) {
	case "text_done", "text_end", "text_complete", "text_stop",
		"message_done", "message_end", "message_complete", "message_stop",
		"assistant_message_done", "assistant_message_end", "assistant_message_complete", "assistant_message_stop":
		return true
	default:
		return false
	}
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
