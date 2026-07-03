package telegram

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"piontg/authz"
	"piontg/folders"
	"piontg/pi"
	"piontg/session"
)

type fakeMessenger struct {
	nextID      int
	sends       []sentMessage
	edits       []string
	chatActions []string
	callbacks   []string
}

type sentMessage struct {
	chatID   int64
	text     string
	keyboard InlineKeyboard
}

func (f *fakeMessenger) SendMessage(_ context.Context, chatID int64, text string, keyboard InlineKeyboard) (int, error) {
	f.nextID++
	f.sends = append(f.sends, sentMessage{chatID: chatID, text: text, keyboard: keyboard})
	return f.nextID, nil
}
func (f *fakeMessenger) EditMessage(_ context.Context, _ int64, _ int, text string) error {
	f.edits = append(f.edits, text)
	return nil
}
func (f *fakeMessenger) SendChatAction(_ context.Context, _ int64, action string) error {
	f.chatActions = append(f.chatActions, action)
	return nil
}
func (f *fakeMessenger) AnswerCallback(_ context.Context, _ string, text string) error {
	f.callbacks = append(f.callbacks, text)
	return nil
}

type fakeSession struct {
	models   []pi.ModelInfo
	commands []pi.CommandInfo
	status   session.Status
	events   chan pi.Event

	selectedFolder string
	selectedModel  string
	prompts        []string
	aborts         int
	newSessions    int
	stops          int
	availableErr   error
	promptErr      error
}

func newFakeSession() *fakeSession {
	return &fakeSession{
		events:   make(chan pi.Event, 10),
		models:   []pi.ModelInfo{{Provider: "anthropic", ID: "claude", Name: "Claude", ContextWindow: 100}},
		commands: []pi.CommandInfo{{Name: "skill:asana-cli", Description: "Use Asana", Source: "skill", Location: "user", Path: "/skills/asana/SKILL.md"}, {Name: "fix-tests", Description: "Fix tests", Source: "prompt"}},
	}
}
func (f *fakeSession) SelectFolder(_ context.Context, path string) error {
	f.selectedFolder = path
	f.status.SelectedFolder = path
	return nil
}
func (f *fakeSession) SelectModel(_ context.Context, provider, modelID string) error {
	f.selectedModel = provider + "/" + modelID
	f.status.SelectedModel = f.selectedModel
	return nil
}
func (f *fakeSession) AvailableModels(context.Context) ([]pi.ModelInfo, error) {
	return f.models, f.availableErr
}
func (f *fakeSession) AvailableCommands(context.Context) ([]pi.CommandInfo, error) {
	return f.commands, nil
}
func (f *fakeSession) Prompt(_ context.Context, message string) error {
	f.prompts = append(f.prompts, message)
	return f.promptErr
}
func (f *fakeSession) Abort(context.Context) error              { f.aborts++; return nil }
func (f *fakeSession) NewSession(context.Context) (bool, error) { f.newSessions++; return false, nil }
func (f *fakeSession) Stop(context.Context) error               { f.stops++; return nil }
func (f *fakeSession) Status() session.Status                   { return f.status }
func (f *fakeSession) Events() <-chan pi.Event                  { return f.events }

type fakeFolders struct {
	choices []folders.Choice
	err     error
}

func (f fakeFolders) Discover() ([]folders.Choice, error) { return f.choices, f.err }

func (f fakeFolders) ResolveToken(token string) (string, folders.EffectivePolicy, error) {
	for _, choice := range f.choices {
		if choice.Token == token {
			return choice.Path, folders.EffectivePolicy{Trust: "no-approve"}, nil
		}
	}
	return "", folders.EffectivePolicy{}, errors.New("not found")
}

func setupHandler() (*Handler, *fakeMessenger, *fakeSession, fakeFolders) {
	messenger := &fakeMessenger{}
	sess := newFakeSession()
	folderPolicy := fakeFolders{choices: []folders.Choice{{Token: "tok", Label: "root/app", Path: "/root/app"}}}
	h := NewHandler(messenger, sess, folderPolicy, authz.New(42), nil)
	return h, messenger, sess, folderPolicy
}

func TestUnauthorizedUserRejected(t *testing.T) {
	h, messenger, sess, _ := setupHandler()
	if err := h.HandleUpdate(context.Background(), Update{Message: &Message{ChatID: 1, UserID: 7, Text: "/start"}}); err != nil {
		t.Fatal(err)
	}
	if len(messenger.sends) != 1 || messenger.sends[0].text != "Not authorized." {
		t.Fatalf("sends = %#v", messenger.sends)
	}
	if len(sess.prompts) != 0 {
		t.Fatalf("unauthorized prompt routed: %#v", sess.prompts)
	}
}

func TestStartHelpStatusCommands(t *testing.T) {
	h, messenger, _, _ := setupHandler()
	for _, cmd := range []string{"/start", "/help", "/status"} {
		if err := h.HandleUpdate(context.Background(), Update{Message: &Message{ChatID: 1, UserID: 42, Text: cmd}}); err != nil {
			t.Fatalf("%s error = %v", cmd, err)
		}
	}
	if len(messenger.sends) != 3 {
		t.Fatalf("sends = %#v", messenger.sends)
	}
	if !strings.Contains(messenger.sends[0].text, "Pi Telegram bot ready") || !strings.Contains(messenger.sends[1].text, "Commands:") || !strings.Contains(messenger.sends[2].text, "Status:") {
		t.Fatalf("unexpected sends = %#v", messenger.sends)
	}
}

func TestFolderPickerAndCallback(t *testing.T) {
	h, messenger, sess, _ := setupHandler()
	ctx := context.Background()
	if err := h.HandleUpdate(ctx, Update{Message: &Message{ChatID: 1, UserID: 42, Text: "/folder"}}); err != nil {
		t.Fatal(err)
	}
	if len(messenger.sends) != 1 || len(messenger.sends[0].keyboard) != 1 {
		t.Fatalf("folder sends = %#v", messenger.sends)
	}
	data := messenger.sends[0].keyboard[0][0].Data
	if err := h.HandleUpdate(ctx, Update{Callback: &Callback{ID: "cb", ChatID: 1, UserID: 42, Data: data}}); err != nil {
		t.Fatal(err)
	}
	if sess.selectedFolder != "/root/app" {
		t.Fatalf("selected folder = %q", sess.selectedFolder)
	}
	if last := messenger.callbacks[len(messenger.callbacks)-1]; last != "Folder selected" {
		t.Fatalf("callbacks = %#v", messenger.callbacks)
	}
}

func TestModelPickerAndCallback(t *testing.T) {
	h, messenger, sess, _ := setupHandler()
	ctx := context.Background()
	if err := h.HandleUpdate(ctx, Update{Message: &Message{ChatID: 1, UserID: 42, Text: "/model"}}); err != nil {
		t.Fatal(err)
	}
	if len(messenger.sends) != 1 || len(messenger.sends[0].keyboard) != 1 {
		t.Fatalf("model sends = %#v", messenger.sends)
	}
	data := messenger.sends[0].keyboard[0][0].Data
	if err := h.HandleUpdate(ctx, Update{Callback: &Callback{ID: "cb", ChatID: 1, UserID: 42, Data: data}}); err != nil {
		t.Fatal(err)
	}
	if sess.selectedModel != "anthropic/claude" {
		t.Fatalf("selected model = %q", sess.selectedModel)
	}
}

func TestSkillPickerAndCallback(t *testing.T) {
	h, messenger, _, _ := setupHandler()
	ctx := context.Background()
	if err := h.HandleUpdate(ctx, Update{Message: &Message{ChatID: 1, UserID: 42, Text: "/skills"}}); err != nil {
		t.Fatal(err)
	}
	if len(messenger.sends) != 1 || len(messenger.sends[0].keyboard) != 1 {
		t.Fatalf("skill sends = %#v", messenger.sends)
	}
	button := messenger.sends[0].keyboard[0][0]
	if button.Text != "asana-cli" || !strings.HasPrefix(button.Data, callbackSkillPrefix) {
		t.Fatalf("skill button = %#v", button)
	}
	if err := h.HandleUpdate(ctx, Update{Callback: &Callback{ID: "cb", ChatID: 1, UserID: 42, Data: button.Data}}); err != nil {
		t.Fatal(err)
	}
	last := messenger.sends[len(messenger.sends)-1].text
	if !strings.Contains(last, "Skill: asana-cli") || !strings.Contains(last, "/skill:asana-cli <request>") || !strings.Contains(last, "Use Asana") {
		t.Fatalf("skill details = %q", last)
	}
}

func TestSkillCommandIsForwardedToPi(t *testing.T) {
	h, _, sess, _ := setupHandler()
	ctx := context.Background()
	if err := h.HandleUpdate(ctx, Update{Message: &Message{ChatID: 1, UserID: 42, Text: "/skill:asana-cli inspect task"}}); err != nil {
		t.Fatal(err)
	}
	if len(sess.prompts) != 1 || sess.prompts[0] != "/skill:asana-cli inspect task" {
		t.Fatalf("prompts = %#v", sess.prompts)
	}
}

func TestMessageAndControlCommands(t *testing.T) {
	h, messenger, sess, _ := setupHandler()
	ctx := context.Background()
	for _, input := range []string{"hello", "/abort", "/new", "/stop"} {
		if err := h.HandleUpdate(ctx, Update{Message: &Message{ChatID: 1, UserID: 42, Text: input}}); err != nil {
			t.Fatalf("%s error = %v", input, err)
		}
	}
	if len(sess.prompts) != 1 || sess.prompts[0] != "hello" {
		t.Fatalf("prompts = %#v", sess.prompts)
	}
	if sess.aborts != 1 || sess.newSessions != 1 || sess.stops != 1 {
		t.Fatalf("aborts=%d new=%d stops=%d", sess.aborts, sess.newSessions, sess.stops)
	}
	if len(messenger.sends) < 3 {
		t.Fatalf("control messages not sent: %#v", messenger.sends)
	}
}

func TestPromptErrorIsReported(t *testing.T) {
	h, messenger, sess, _ := setupHandler()
	sess.promptErr = errors.New("no folder selected")
	if err := h.HandleUpdate(context.Background(), Update{Message: &Message{ChatID: 1, UserID: 42, Text: "hello"}}); err == nil {
		t.Fatal("HandleUpdate() error = nil")
	}
	if len(messenger.sends) != 1 || !strings.Contains(messenger.sends[0].text, "no folder selected") {
		t.Fatalf("sends = %#v", messenger.sends)
	}
}

func TestEventRendering(t *testing.T) {
	h, messenger, sess, _ := setupHandler()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.StartEventRendering(ctx, 1)
	sess.events <- pi.Event{Type: "message_update", Raw: []byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"Hi"}}`)}
	sess.events <- pi.Event{Type: "agent_end", Raw: []byte(`{"type":"agent_end"}`)}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(messenger.sends) >= 1 && len(messenger.edits) >= 1 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("render did not send/edit: sends=%#v edits=%#v", messenger.sends, messenger.edits)
}

func TestEventRenderingKeepsTypingUntilAgentEnd(t *testing.T) {
	h, messenger, sess, _ := setupHandler()
	h.typingInterval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess.status.IsStreaming = true
	h.StartEventRendering(ctx, 1)

	sess.events <- pi.Event{Type: "agent_start", Raw: []byte(`{"type":"agent_start"}`)}
	waitForRepeatedTyping(t, messenger)

	sess.status.IsStreaming = false
	sess.events <- pi.Event{Type: "message_update", Raw: []byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"Done"}}`)}
	sess.events <- pi.Event{Type: "agent_end", Raw: []byte(`{"type":"agent_end"}`)}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(messenger.edits) >= 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if len(messenger.edits) < 1 {
		t.Fatalf("agent_end was not rendered: sends=%#v edits=%#v", messenger.sends, messenger.edits)
	}
	afterEnd := len(messenger.chatActions)
	time.Sleep(4 * h.typingInterval)
	if len(messenger.chatActions) != afterEnd {
		t.Fatalf("typing continued after agent_end: before=%d after=%d actions=%#v", afterEnd, len(messenger.chatActions), messenger.chatActions)
	}
}

func TestEventRenderingStopsTypingWhenSessionStopsWithoutAgentEnd(t *testing.T) {
	h, messenger, sess, _ := setupHandler()
	h.typingInterval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess.status.IsStreaming = true
	h.StartEventRendering(ctx, 1)

	sess.events <- pi.Event{Type: "agent_start", Raw: []byte(`{"type":"agent_start"}`)}
	waitForRepeatedTyping(t, messenger)

	sess.status.IsStreaming = false
	time.Sleep(4 * h.typingInterval)
	afterStop := len(messenger.chatActions)
	time.Sleep(4 * h.typingInterval)
	if len(messenger.chatActions) != afterStop {
		t.Fatalf("typing continued after session stopped: before=%d after=%d actions=%#v", afterStop, len(messenger.chatActions), messenger.chatActions)
	}
}

func waitForRepeatedTyping(t *testing.T, messenger *fakeMessenger) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(messenger.chatActions) >= 2 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("typing heartbeat did not repeat: chatActions=%#v", messenger.chatActions)
}
