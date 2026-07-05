package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	promptRequests []session.PromptRequest
	uiResponses    []string
	uiPayloads     []map[string]any
	uiErr          error
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
func (f *fakeSession) PromptRequest(_ context.Context, req session.PromptRequest) error {
	f.promptRequests = append(f.promptRequests, req)
	return f.promptErr
}
func (f *fakeSession) RespondExtensionUI(_ context.Context, requestID string, payload map[string]any) error {
	f.uiResponses = append(f.uiResponses, requestID)
	f.uiPayloads = append(f.uiPayloads, payload)
	return f.uiErr
}
func (f *fakeSession) Abort(context.Context) error              { f.aborts++; return nil }
func (f *fakeSession) NewSession(context.Context) (bool, error) { f.newSessions++; return false, nil }
func (f *fakeSession) Stop(context.Context) error               { f.stops++; return nil }
func (f *fakeSession) Status() session.Status                   { return f.status }
func (f *fakeSession) Events() <-chan pi.Event                  { return f.events }

type fakeImageFetcher struct {
	calls []ImageRef
	image pi.ImageContent
	err   error
}

func (f *fakeImageFetcher) FetchImage(_ context.Context, ref ImageRef) (pi.ImageContent, error) {
	f.calls = append(f.calls, ref)
	if f.err != nil {
		return pi.ImageContent{}, f.err
	}
	if f.image.Type == "" {
		f.image = pi.ImageContent{Type: pi.ImageContentTypeImage, Data: "aW1n", MimeType: "image/jpeg"}
	}
	return f.image, nil
}

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

func TestParseExtensionUIRequest(t *testing.T) {
	tests := []struct {
		name   string
		event  pi.Event
		method string
	}{
		{
			name:   "select",
			event:  pi.Event{Type: "extension_ui_request", Raw: []byte(`{"id":"ui-1","method":"select","title":"Allow?","options":["Allow","Deny"],"timeout":10000}`)},
			method: "select",
		},
		{
			name:   "confirm",
			event:  pi.Event{Type: "extension_ui_request", Raw: []byte(`{"id":"ui-2","method":"confirm","title":"Clear?","message":"All messages will be lost."}`)},
			method: "confirm",
		},
		{
			name:   "notify",
			event:  pi.Event{Type: "extension_ui_request", Raw: []byte(`{"id":"ui-3","method":"notify","message":"Blocked","notifyType":"warning"}`)},
			method: "notify",
		},
		{
			name:   "unsupported blocking method still parses",
			event:  pi.Event{Type: "extension_ui_request", Raw: []byte(`{"id":"ui-4","method":"editor","title":"Edit"}`)},
			method: "editor",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request, err := parseExtensionUIRequest(tt.event)
			if err != nil {
				t.Fatalf("parseExtensionUIRequest() error = %v", err)
			}
			if request.Method != tt.method || request.ID == "" {
				t.Fatalf("request = %#v", request)
			}
		})
	}
}

func TestParseExtensionUIRequestRejectsMalformed(t *testing.T) {
	for _, event := range []pi.Event{
		{Type: "message_update", Raw: []byte(`{"id":"ui-1","method":"select"}`)},
		{Type: "extension_ui_request", Raw: []byte(`{"id":"ui-1","method":`)},
		{Type: "extension_ui_request", Raw: []byte(`{"method":"select"}`)},
		{Type: "extension_ui_request", Raw: []byte(`{"id":"ui-1"}`)},
	} {
		if _, err := parseExtensionUIRequest(event); err == nil {
			t.Fatalf("parseExtensionUIRequest(%#v) error = nil", event)
		}
	}
}

func TestPendingExtensionUITokenDoesNotEmbedOptionsAndResolvesServerSide(t *testing.T) {
	h, _, _, _ := setupHandler()
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	request := extensionUIRequest{ID: "ui-1", Method: "select", Options: []string{"Allow dangerous command", "Deny"}}
	token, pending, err := h.storePendingExtensionUI(request, now)
	if err != nil {
		t.Fatalf("storePendingExtensionUI() error = %v", err)
	}
	if token == "" || strings.Contains(token, "Allow") || strings.Contains(token, "Deny") {
		t.Fatalf("unsafe token %q", token)
	}
	if value, ok := extensionUIOption(pending, 0); !ok || value != "Allow dangerous command" {
		t.Fatalf("option 0 = %q %v", value, ok)
	}
	resolved, ok, expired := h.resolvePendingExtensionUI(token, now.Add(time.Second))
	if !ok || expired || resolved.RequestID != "ui-1" {
		t.Fatalf("resolved=%#v ok=%v expired=%v", resolved, ok, expired)
	}
	if value, ok := extensionUIOption(resolved, 1); !ok || value != "Deny" {
		t.Fatalf("option 1 = %q %v", value, ok)
	}
}

func TestPendingExtensionUIExpiry(t *testing.T) {
	h, _, _, _ := setupHandler()
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	token, pending, err := h.storePendingExtensionUI(extensionUIRequest{ID: "ui-1", Method: "confirm", TimeoutMS: 1000}, now)
	if err != nil {
		t.Fatalf("storePendingExtensionUI() error = %v", err)
	}
	wantExpiry := now.Add(time.Second + extensionUITimeoutGrace)
	if !pending.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("expires at %v, want %v", pending.ExpiresAt, wantExpiry)
	}
	if _, ok, expired := h.resolvePendingExtensionUI(token, wantExpiry.Add(time.Millisecond)); ok || !expired {
		t.Fatalf("resolve expired ok=%v expired=%v", ok, expired)
	}

	defaultToken, defaultPending, err := h.storePendingExtensionUI(extensionUIRequest{ID: "ui-2", Method: "confirm"}, now)
	if err != nil {
		t.Fatalf("store default pending error = %v", err)
	}
	if !defaultPending.ExpiresAt.Equal(now.Add(extensionUIDefaultTTL)) {
		t.Fatalf("default expiry = %v", defaultPending.ExpiresAt)
	}
	h.cleanupExpiredExtensionUI(now.Add(extensionUIDefaultTTL + time.Millisecond))
	if _, ok, expired := h.resolvePendingExtensionUI(defaultToken, now.Add(extensionUIDefaultTTL+2*time.Millisecond)); ok || expired {
		t.Fatalf("cleaned resolve ok=%v expired=%v", ok, expired)
	}
}

func TestUnauthorizedPhotoDoesNotFetchOrPrompt(t *testing.T) {
	h, messenger, sess, _ := setupHandler()
	fetcher := &fakeImageFetcher{}
	h.imageFetcher = fetcher
	msg := Message{ChatID: 1, UserID: 7, Images: []ImageRef{{FileID: "file"}}, Caption: "look"}
	if err := h.HandleUpdate(context.Background(), Update{Message: &msg}); err != nil {
		t.Fatal(err)
	}
	if len(messenger.sends) != 1 || messenger.sends[0].text != "Not authorized." {
		t.Fatalf("sends = %#v", messenger.sends)
	}
	if len(fetcher.calls) != 0 || len(sess.promptRequests) != 0 {
		t.Fatalf("fetches=%#v promptRequests=%#v", fetcher.calls, sess.promptRequests)
	}
}

func TestPhotoWithCaptionSendsImagePrompt(t *testing.T) {
	h, _, sess, _ := setupHandler()
	fetcher := &fakeImageFetcher{image: pi.ImageContent{Type: pi.ImageContentTypeImage, Data: "aW1n", MimeType: "image/jpeg"}}
	h.imageFetcher = fetcher
	msg := Message{ChatID: 1, UserID: 42, Images: []ImageRef{{FileID: "file", Size: 100, Source: "photo"}}, Caption: "what is this?"}
	if err := h.HandleUpdate(context.Background(), Update{Message: &msg}); err != nil {
		t.Fatal(err)
	}
	if len(fetcher.calls) != 1 || fetcher.calls[0].FileID != "file" {
		t.Fatalf("fetcher calls = %#v", fetcher.calls)
	}
	if len(sess.promptRequests) != 1 || sess.promptRequests[0].Message != "what is this?" || len(sess.promptRequests[0].Images) != 1 || sess.promptRequests[0].Images[0].Data != "aW1n" {
		t.Fatalf("promptRequests = %#v", sess.promptRequests)
	}
}

func TestPhotoWithoutCaptionUsesDefaultPrompt(t *testing.T) {
	h, _, sess, _ := setupHandler()
	h.imageFetcher = &fakeImageFetcher{}
	msg := Message{ChatID: 1, UserID: 42, Images: []ImageRef{{FileID: "file"}}}
	if err := h.HandleUpdate(context.Background(), Update{Message: &msg}); err != nil {
		t.Fatal(err)
	}
	if len(sess.promptRequests) != 1 || sess.promptRequests[0].Message != "Please analyze the attached image." {
		t.Fatalf("promptRequests = %#v", sess.promptRequests)
	}
}

func TestPhotoCaptionSlashIsPromptText(t *testing.T) {
	h, _, sess, _ := setupHandler()
	h.imageFetcher = &fakeImageFetcher{}
	msg := Message{ChatID: 1, UserID: 42, Images: []ImageRef{{FileID: "file"}}, Caption: "/status"}
	if err := h.HandleUpdate(context.Background(), Update{Message: &msg}); err != nil {
		t.Fatal(err)
	}
	if len(sess.promptRequests) != 1 || sess.promptRequests[0].Message != "/status" || len(sess.prompts) != 0 {
		t.Fatalf("prompts=%#v promptRequests=%#v", sess.prompts, sess.promptRequests)
	}
}

func TestImageFetchErrorIsReported(t *testing.T) {
	h, messenger, sess, _ := setupHandler()
	h.imageFetcher = &fakeImageFetcher{err: errors.New("download failed")}
	msg := Message{ChatID: 1, UserID: 42, Images: []ImageRef{{FileID: "file"}}, Caption: "look"}
	if err := h.HandleUpdate(context.Background(), Update{Message: &msg}); err == nil {
		t.Fatal("HandleUpdate() error = nil")
	}
	if len(messenger.sends) != 1 || !strings.Contains(messenger.sends[0].text, "download failed") {
		t.Fatalf("sends = %#v", messenger.sends)
	}
	if len(sess.promptRequests) != 0 {
		t.Fatalf("promptRequests = %#v", sess.promptRequests)
	}
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

func TestEventRenderingShowsExtensionUISelect(t *testing.T) {
	h, messenger, sess, _ := setupHandler()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.StartEventRendering(ctx, 1)
	sess.events <- pi.Event{Type: "extension_ui_request", Raw: []byte(`{"type":"extension_ui_request","id":"ui-1","method":"select","title":"Allow command?","options":["Allow","Deny"],"timeout":10000}`)}

	waitForSends(t, messenger, 1)
	sent := messenger.sends[0]
	if !strings.Contains(sent.text, "Allow command?") {
		t.Fatalf("select text = %q", sent.text)
	}
	if len(sent.keyboard) != 3 {
		t.Fatalf("select keyboard = %#v", sent.keyboard)
	}
	allow := sent.keyboard[0][0]
	if allow.Text != "Allow" || !strings.HasPrefix(allow.Data, callbackUIPrefix) || strings.Contains(allow.Data, "Allow") || len(allow.Data) > extensionUICallbackDataMax {
		t.Fatalf("allow button = %#v", allow)
	}
	if sent.keyboard[2][0].Text != "Cancel" {
		t.Fatalf("cancel button = %#v", sent.keyboard[2][0])
	}
	h.mu.Lock()
	pendingCount := len(h.uiTokens)
	h.mu.Unlock()
	if pendingCount != 1 {
		t.Fatalf("pending count = %d", pendingCount)
	}
}

func TestEventRenderingCancelsExtensionUISelectWithTooManyOptions(t *testing.T) {
	h, messenger, sess, _ := setupHandler()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.StartEventRendering(ctx, 1)
	options := make([]string, extensionUIMaxOptions+1)
	for i := range options {
		options[i] = fmt.Sprintf("Option %d", i)
	}
	raw, err := json.Marshal(map[string]any{"type": "extension_ui_request", "id": "ui-1", "method": "select", "title": "Pick", "options": options})
	if err != nil {
		t.Fatal(err)
	}
	sess.events <- pi.Event{Type: "extension_ui_request", Raw: raw}

	waitForSends(t, messenger, 1)
	if len(sess.uiResponses) != 1 || sess.uiResponses[0] != "ui-1" || sess.uiPayloads[0]["cancelled"] != true {
		t.Fatalf("ui responses=%#v payloads=%#v", sess.uiResponses, sess.uiPayloads)
	}
	if messenger.sends[0].keyboard != nil || !strings.Contains(messenger.sends[0].text, "cancelled") {
		t.Fatalf("send = %#v", messenger.sends[0])
	}
}

func TestEventRenderingTruncatesExtensionUIText(t *testing.T) {
	h, messenger, sess, _ := setupHandler()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.StartEventRendering(ctx, 1)
	sess.events <- pi.Event{Type: "extension_ui_request", Raw: []byte(`{"type":"extension_ui_request","id":"ui-1","method":"confirm","title":"Dangerous?","message":"` + strings.Repeat("x", extensionUIMessageTextMax+100) + `"}`)}

	waitForSends(t, messenger, 1)
	if len([]rune(messenger.sends[0].text)) > extensionUIMessageTextMax {
		t.Fatalf("message length = %d", len([]rune(messenger.sends[0].text)))
	}
}

func TestEventRenderingShowsExtensionUIConfirm(t *testing.T) {
	h, messenger, sess, _ := setupHandler()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.StartEventRendering(ctx, 1)
	sess.events <- pi.Event{Type: "extension_ui_request", Raw: []byte(`{"type":"extension_ui_request","id":"ui-1","method":"confirm","title":"Dangerous?","message":"Run rm?"}`)}

	waitForSends(t, messenger, 1)
	sent := messenger.sends[0]
	if !strings.Contains(sent.text, "Dangerous?") || !strings.Contains(sent.text, "Run rm?") {
		t.Fatalf("confirm text = %q", sent.text)
	}
	if len(sent.keyboard) != 1 || len(sent.keyboard[0]) != 2 {
		t.Fatalf("confirm keyboard = %#v", sent.keyboard)
	}
	if sent.keyboard[0][0].Text != "Approve" || sent.keyboard[0][1].Text != "Deny" {
		t.Fatalf("confirm buttons = %#v", sent.keyboard[0])
	}
	if !strings.HasPrefix(sent.keyboard[0][0].Data, callbackUIPrefix) || !strings.HasPrefix(sent.keyboard[0][1].Data, callbackUIPrefix) {
		t.Fatalf("confirm callback data = %#v", sent.keyboard[0])
	}
}

func TestEventRenderingShowsExtensionUINotify(t *testing.T) {
	h, messenger, sess, _ := setupHandler()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.StartEventRendering(ctx, 1)
	sess.events <- pi.Event{Type: "extension_ui_request", Raw: []byte(`{"type":"extension_ui_request","id":"ui-1","method":"notify","message":"Blocked by policy","notifyType":"warning"}`)}

	waitForSends(t, messenger, 1)
	if !strings.Contains(messenger.sends[0].text, "warning") || !strings.Contains(messenger.sends[0].text, "Blocked by policy") {
		t.Fatalf("notify send = %#v", messenger.sends[0])
	}
	if messenger.sends[0].keyboard != nil {
		t.Fatalf("notify keyboard = %#v", messenger.sends[0].keyboard)
	}
}

func TestEventRenderingShowsUnsupportedExtensionUINotice(t *testing.T) {
	h, messenger, sess, _ := setupHandler()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.StartEventRendering(ctx, 1)
	sess.events <- pi.Event{Type: "extension_ui_request", Raw: []byte(`{"type":"extension_ui_request","id":"ui-1","method":"editor","title":"Edit"}`)}

	waitForSends(t, messenger, 1)
	if !strings.Contains(messenger.sends[0].text, "unsupported") || !strings.Contains(messenger.sends[0].text, "cancelled") {
		t.Fatalf("unsupported send = %#v", messenger.sends[0])
	}
}

func TestExtensionUISelectOptionCallbackRespondsToPi(t *testing.T) {
	h, messenger, sess, _ := setupHandler()
	token, _, err := h.storePendingExtensionUI(extensionUIRequest{ID: "ui-1", Method: "select", Options: []string{"Allow", "Deny"}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cb := Callback{ID: "cb", ChatID: 1, UserID: 42, Data: extensionUICallbackData(token, "opt", 0)}
	if err := h.HandleUpdate(context.Background(), Update{Callback: &cb}); err != nil {
		t.Fatal(err)
	}
	if len(sess.uiResponses) != 1 || sess.uiResponses[0] != "ui-1" || sess.uiPayloads[0]["value"] != "Allow" {
		t.Fatalf("ui responses=%#v payloads=%#v", sess.uiResponses, sess.uiPayloads)
	}
	if last := messenger.callbacks[len(messenger.callbacks)-1]; last != "Selected" {
		t.Fatalf("callbacks = %#v", messenger.callbacks)
	}
}

func TestExtensionUISelectCancelCallbackRespondsToPi(t *testing.T) {
	h, _, sess, _ := setupHandler()
	token, _, err := h.storePendingExtensionUI(extensionUIRequest{ID: "ui-1", Method: "select", Options: []string{"Allow", "Deny"}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cb := Callback{ID: "cb", ChatID: 1, UserID: 42, Data: extensionUICallbackData(token, "cancel", -1)}
	if err := h.HandleUpdate(context.Background(), Update{Callback: &cb}); err != nil {
		t.Fatal(err)
	}
	if len(sess.uiResponses) != 1 || sess.uiPayloads[0]["cancelled"] != true {
		t.Fatalf("ui responses=%#v payloads=%#v", sess.uiResponses, sess.uiPayloads)
	}
}

func TestExtensionUIConfirmCallbacksRespondToPi(t *testing.T) {
	for _, tt := range []struct {
		name       string
		action     string
		wantValue  bool
		wantAnswer string
	}{
		{name: "approve", action: "yes", wantValue: true, wantAnswer: "Approved"},
		{name: "deny", action: "no", wantValue: false, wantAnswer: "Denied"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h, messenger, sess, _ := setupHandler()
			token, _, err := h.storePendingExtensionUI(extensionUIRequest{ID: "ui-1", Method: "confirm"}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			cb := Callback{ID: "cb", ChatID: 1, UserID: 42, Data: extensionUICallbackData(token, tt.action, -1)}
			if err := h.HandleUpdate(context.Background(), Update{Callback: &cb}); err != nil {
				t.Fatal(err)
			}
			if len(sess.uiResponses) != 1 || sess.uiPayloads[0]["confirmed"] != tt.wantValue {
				t.Fatalf("ui responses=%#v payloads=%#v", sess.uiResponses, sess.uiPayloads)
			}
			if last := messenger.callbacks[len(messenger.callbacks)-1]; last != tt.wantAnswer {
				t.Fatalf("callbacks = %#v", messenger.callbacks)
			}
		})
	}
}

func TestExtensionUICallbackExpiredSendsNoPiResponse(t *testing.T) {
	h, messenger, sess, _ := setupHandler()
	old := time.Now().Add(-time.Minute)
	token, _, err := h.storePendingExtensionUI(extensionUIRequest{ID: "ui-1", Method: "confirm", TimeoutMS: 1}, old)
	if err != nil {
		t.Fatal(err)
	}
	cb := Callback{ID: "cb", ChatID: 1, UserID: 42, Data: extensionUICallbackData(token, "yes", -1)}
	if err := h.HandleUpdate(context.Background(), Update{Callback: &cb}); err != nil {
		t.Fatal(err)
	}
	if len(sess.uiResponses) != 0 {
		t.Fatalf("expired callback responded to Pi: %#v %#v", sess.uiResponses, sess.uiPayloads)
	}
	if last := messenger.callbacks[len(messenger.callbacks)-1]; !strings.Contains(last, "expired") {
		t.Fatalf("callbacks = %#v", messenger.callbacks)
	}
}

func TestExtensionUICallbackDuplicateIsAlreadyHandled(t *testing.T) {
	h, messenger, sess, _ := setupHandler()
	token, _, err := h.storePendingExtensionUI(extensionUIRequest{ID: "ui-1", Method: "confirm"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cb := Callback{ID: "cb", ChatID: 1, UserID: 42, Data: extensionUICallbackData(token, "yes", -1)}
	if err := h.HandleUpdate(context.Background(), Update{Callback: &cb}); err != nil {
		t.Fatal(err)
	}
	if err := h.HandleUpdate(context.Background(), Update{Callback: &cb}); err != nil {
		t.Fatal(err)
	}
	if len(sess.uiResponses) != 1 {
		t.Fatalf("duplicate responses = %#v", sess.uiResponses)
	}
	if last := messenger.callbacks[len(messenger.callbacks)-1]; !strings.Contains(last, "Already handled") {
		t.Fatalf("callbacks = %#v", messenger.callbacks)
	}
}

func TestExtensionUICallbackUnauthorizedSendsNoPiResponse(t *testing.T) {
	h, messenger, sess, _ := setupHandler()
	token, _, err := h.storePendingExtensionUI(extensionUIRequest{ID: "ui-1", Method: "confirm"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cb := Callback{ID: "cb", ChatID: 1, UserID: 7, Data: extensionUICallbackData(token, "yes", -1)}
	if err := h.HandleUpdate(context.Background(), Update{Callback: &cb}); err != nil {
		t.Fatal(err)
	}
	if len(sess.uiResponses) != 0 {
		t.Fatalf("unauthorized response sent to Pi: %#v", sess.uiResponses)
	}
	if last := messenger.callbacks[len(messenger.callbacks)-1]; last != "Not authorized" {
		t.Fatalf("callbacks = %#v", messenger.callbacks)
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

func waitForSends(t *testing.T, messenger *fakeMessenger, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(messenger.sends) >= count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("message sends did not reach %d: sends=%#v", count, messenger.sends)
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
