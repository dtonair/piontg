package render

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"piontg/pi"
)

type fakeSink struct {
	nextID  int
	sends   []string
	edits   []editCall
	typings int
	edErr   error
}

type editCall struct {
	id   int
	text string
}

func (f *fakeSink) SendMessage(_ context.Context, text string) (int, error) {
	f.nextID++
	f.sends = append(f.sends, text)
	return f.nextID, nil
}

func (f *fakeSink) EditMessage(_ context.Context, messageID int, text string) error {
	if f.edErr != nil {
		return f.edErr
	}
	f.edits = append(f.edits, editCall{id: messageID, text: text})
	return nil
}

func (f *fakeSink) SendTyping(context.Context) error {
	f.typings++
	return nil
}

func TestRendererBuffersTextUntilFlushWithoutEdits(t *testing.T) {
	sink := &fakeSink{}
	r := New(sink)
	r.SetLimits(100, time.Second)
	ctx := context.Background()

	if err := r.AppendText(ctx, "hel"); err != nil {
		t.Fatal(err)
	}
	if err := r.AppendText(ctx, "lo"); err != nil {
		t.Fatal(err)
	}
	if len(sink.sends) != 0 {
		t.Fatalf("sends before flush = %#v", sink.sends)
	}
	if len(sink.edits) != 0 {
		t.Fatalf("edits before flush = %#v", sink.edits)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sink.sends) != 1 || sink.sends[0] != "hello" {
		t.Fatalf("sends = %#v", sink.sends)
	}
	if len(sink.edits) != 0 {
		t.Fatalf("edits = %#v", sink.edits)
	}
}

func TestRendererChunksLongMessages(t *testing.T) {
	sink := &fakeSink{}
	r := New(sink)
	r.SetLimits(5, time.Hour)
	ctx := context.Background()

	if err := r.AppendText(ctx, "hello world"); err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sink.sends) != 3 {
		t.Fatalf("sends = %#v", sink.sends)
	}
	if sink.sends[0] != "hello" || sink.sends[1] != " worl" || sink.sends[2] != "d" {
		t.Fatalf("sends = %#v", sink.sends)
	}
	for _, send := range sink.sends {
		if len([]rune(send)) > 5 {
			t.Fatalf("send exceeds limit: %#v", send)
		}
	}
	if len(sink.edits) != 0 {
		t.Fatalf("edits = %#v", sink.edits)
	}
}

func TestRendererHandlesMessageUpdateAndToolEvents(t *testing.T) {
	sink := &fakeSink{}
	r := New(sink)
	r.SetLimits(100, 0)
	ctx := context.Background()

	events := []pi.Event{
		{Type: "message_update", Raw: []byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"Hi"}}`)},
		{Type: "tool_execution_start", Raw: []byte(`{"type":"tool_execution_start","toolName":"bash","args":{"command":"go test ./..."}}`)},
		{Type: "tool_execution_end", Raw: []byte(`{"type":"tool_execution_end","toolName":"bash","isError":false}`)},
		{Type: "agent_end", Raw: []byte(`{"type":"agent_end"}`)},
	}
	for _, event := range events {
		if err := r.HandleEvent(ctx, event); err != nil {
			t.Fatalf("HandleEvent(%s) error = %v", event.Type, err)
		}
	}
	if len(sink.sends) != 2 {
		t.Fatalf("sends = %#v", sink.sends)
	}
	if sink.sends[0] != "Hi" {
		t.Fatalf("assistant send = %#v", sink.sends)
	}
	if !strings.Contains(sink.sends[1], "🔧 bash") || !strings.Contains(sink.sends[1], "```bash\ngo test ./...\n```") {
		t.Fatalf("tool start = %q", sink.sends[1])
	}
}

func TestRendererFlushesWhenMessageIDChanges(t *testing.T) {
	sink := &fakeSink{}
	r := New(sink)
	r.SetLimits(100, 0)
	ctx := context.Background()

	events := []pi.Event{
		{Type: "message_update", Raw: []byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","messageId":"m1","delta":"First"}}`)},
		{Type: "message_update", Raw: []byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","messageId":"m2","delta":"Second"}}`)},
		{Type: "agent_end", Raw: []byte(`{"type":"agent_end"}`)},
	}
	for _, event := range events {
		if err := r.HandleEvent(ctx, event); err != nil {
			t.Fatalf("HandleEvent(%s) error = %v", event.Type, err)
		}
	}
	if len(sink.sends) != 2 || sink.sends[0] != "First" || sink.sends[1] != "Second" {
		t.Fatalf("sends = %#v", sink.sends)
	}
	if len(sink.edits) != 0 {
		t.Fatalf("edits = %#v", sink.edits)
	}
}

func TestRendererFlushesOnMessageBoundaryEvent(t *testing.T) {
	sink := &fakeSink{}
	r := New(sink)
	r.SetLimits(100, 0)
	ctx := context.Background()

	events := []pi.Event{
		{Type: "message_update", Raw: []byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"Done"}}`)},
		{Type: "message_update", Raw: []byte(`{"type":"message_update","assistantMessageEvent":{"type":"message_end"}}`)},
	}
	for _, event := range events {
		if err := r.HandleEvent(ctx, event); err != nil {
			t.Fatalf("HandleEvent(%s) error = %v", event.Type, err)
		}
	}
	if len(sink.sends) != 1 || sink.sends[0] != "Done" {
		t.Fatalf("sends = %#v", sink.sends)
	}
	if len(sink.edits) != 0 {
		t.Fatalf("edits = %#v", sink.edits)
	}
}

func TestRendererSendsTypingForThinkingDeltas(t *testing.T) {
	sink := &fakeSink{}
	r := New(sink)
	r.SetLimits(100, 0)
	ctx := context.Background()

	events := []pi.Event{
		{Type: "message_update", Raw: []byte(`{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":"plan"}}`)},
		{Type: "message_update", Raw: []byte(`{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":"ning"}}`)},
		{Type: "message_update", Raw: []byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"Answer"}}`)},
		{Type: "agent_end", Raw: []byte(`{"type":"agent_end"}`)},
	}
	for _, event := range events {
		if err := r.HandleEvent(ctx, event); err != nil {
			t.Fatalf("HandleEvent(%s) error = %v", event.Type, err)
		}
	}

	if sink.typings != 1 {
		t.Fatalf("typings = %d", sink.typings)
	}
	if len(sink.sends) != 1 || sink.sends[0] != "Answer" {
		t.Fatalf("sends = %#v", sink.sends)
	}
}

func TestRendererHandlesTopLevelThinkingEvent(t *testing.T) {
	sink := &fakeSink{}
	r := New(sink)
	ctx := context.Background()

	if err := r.HandleEvent(ctx, pi.Event{Type: "reasoning_delta", Raw: []byte(`{"type":"reasoning_delta","text":"considering"}`)}); err != nil {
		t.Fatal(err)
	}
	if sink.typings != 1 {
		t.Fatalf("typings = %d", sink.typings)
	}
	if len(sink.sends) != 0 {
		t.Fatalf("sends = %#v", sink.sends)
	}
}

func TestRendererHandlesNestedThinkingDeltaObject(t *testing.T) {
	sink := &fakeSink{}
	r := New(sink)
	ctx := context.Background()

	event := pi.Event{Type: "message_update", Raw: []byte(`{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":{"type":"thinking_delta","thinking":"nested"}}}`)}
	if err := r.HandleEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	if sink.typings != 1 {
		t.Fatalf("typings = %d", sink.typings)
	}
	if len(sink.sends) != 0 {
		t.Fatalf("sends = %#v", sink.sends)
	}
}

func TestRendererOnlyShowsToolEndWhenFailed(t *testing.T) {
	sink := &fakeSink{}
	r := New(sink)
	ctx := context.Background()

	if err := r.HandleEvent(ctx, pi.Event{Type: "tool_execution_end", Raw: []byte(`{"type":"tool_execution_end","toolName":"read","isError":false}`)}); err != nil {
		t.Fatal(err)
	}
	if len(sink.sends) != 0 {
		t.Fatalf("successful tool end should be silent, sends = %#v", sink.sends)
	}

	if err := r.HandleEvent(ctx, pi.Event{Type: "tool_execution_end", Raw: []byte(`{"type":"tool_execution_end","toolName":"bash","isError":true}`)}); err != nil {
		t.Fatal(err)
	}
	if len(sink.sends) != 1 || sink.sends[0] != "❌ bash failed" {
		t.Fatalf("failed tool end sends = %#v", sink.sends)
	}
}

func TestRendererDoesNotEditWhenEditFails(t *testing.T) {
	sink := &fakeSink{edErr: errors.New("edit failed")}
	r := New(sink)
	r.SetLimits(100, 0)
	ctx := context.Background()
	if err := r.AppendText(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := r.AppendText(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sink.sends) != 1 || sink.sends[0] != "ab" {
		t.Fatalf("sends = %#v", sink.sends)
	}
	if len(sink.edits) != 0 {
		t.Fatalf("edits = %#v", sink.edits)
	}
}

func TestTruncateIsRuneSafe(t *testing.T) {
	got := truncate("😀😀😀", 2)
	if got != "😀…" {
		t.Fatalf("truncate = %q", got)
	}
}
