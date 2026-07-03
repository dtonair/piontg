package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"piontg/config"
	"piontg/folders"
	"piontg/pi"
	"piontg/store"
)

type fakeFactory struct {
	clients []*fakeClient
	opts    []pi.Options
}

func (f *fakeFactory) Start(_ context.Context, opts pi.Options) (PiClient, error) {
	client := newFakeClient()
	f.clients = append(f.clients, client)
	f.opts = append(f.opts, opts)
	return client, nil
}

type fakeClient struct {
	events   chan pi.Event
	state    pi.SessionState
	models   []pi.ModelInfo
	commands []pi.CommandInfo

	stopped     bool
	closed      bool
	prompts     []string
	followUps   []string
	steers      []string
	aborts      int
	newSess     int
	setModels   []string
	uiResponses []string
	uiPayloads  []map[string]any
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		events:   make(chan pi.Event, 10),
		state:    pi.SessionState{SessionFile: "/tmp/session.jsonl", SessionID: "sid"},
		models:   []pi.ModelInfo{{Provider: "anthropic", ID: "claude", ContextWindow: 200000}},
		commands: []pi.CommandInfo{{Name: "skill:asana-cli", Description: "Use Asana", Source: "skill", Location: "user", Path: "/skills/asana/SKILL.md"}},
	}
}

func (f *fakeClient) Events() <-chan pi.Event { return f.events }
func (f *fakeClient) Stop(context.Context) error {
	f.stopped = true
	f.closed = true
	close(f.events)
	return nil
}
func (f *fakeClient) IsClosed() bool                                    { return f.closed }
func (f *fakeClient) GetState(context.Context) (pi.SessionState, error) { return f.state, nil }
func (f *fakeClient) GetAvailableModels(context.Context) ([]pi.ModelInfo, error) {
	return f.models, nil
}
func (f *fakeClient) GetCommands(context.Context) ([]pi.CommandInfo, error) {
	return f.commands, nil
}
func (f *fakeClient) SetModel(_ context.Context, provider, modelID string) (pi.ModelInfo, error) {
	f.setModels = append(f.setModels, provider+"/"+modelID)
	return pi.ModelInfo{Provider: provider, ID: modelID}, nil
}
func (f *fakeClient) Prompt(_ context.Context, message string) error {
	f.prompts = append(f.prompts, message)
	return nil
}
func (f *fakeClient) FollowUp(_ context.Context, message string) error {
	f.followUps = append(f.followUps, message)
	return nil
}
func (f *fakeClient) Steer(_ context.Context, message string) error {
	f.steers = append(f.steers, message)
	return nil
}
func (f *fakeClient) Abort(context.Context) error { f.aborts++; return nil }
func (f *fakeClient) RespondExtensionUI(_ context.Context, requestID string, payload map[string]any) error {
	f.uiResponses = append(f.uiResponses, requestID)
	f.uiPayloads = append(f.uiPayloads, payload)
	return nil
}
func (f *fakeClient) NewSession(context.Context) (bool, error) { f.newSess++; return false, nil }
func (f *fakeClient) StderrTail() string                       { return "stderr" }

func TestManagerStartsPiWithSelectedFolderPolicyAndPersistsState(t *testing.T) {
	ctx := context.Background()
	m, factory, root := setupManager(t, config.StreamingFollowUp)
	child := mkdir(t, root, "child")

	if err := m.SelectFolder(ctx, child); err != nil {
		t.Fatalf("SelectFolder() error = %v", err)
	}
	client, err := m.EnsureStarted(ctx)
	if err != nil {
		t.Fatalf("EnsureStarted() error = %v", err)
	}
	if client == nil || len(factory.opts) != 1 {
		t.Fatalf("client/factory = %#v %#v", client, factory.opts)
	}
	if factory.opts[0].CWD != canonical(t, child) {
		t.Fatalf("cwd = %q", factory.opts[0].CWD)
	}
	if factory.opts[0].Trust != config.TrustNoApprove {
		t.Fatalf("trust = %q", factory.opts[0].Trust)
	}
	status := m.Status()
	if status.SessionID != "sid" || status.SessionFile != "/tmp/session.jsonl" || !status.IsStarted {
		t.Fatalf("status = %#v", status)
	}
}

func TestManagerRevalidatesSelectedFolderBeforeStart(t *testing.T) {
	ctx := context.Background()
	m, factory, root := setupManager(t, config.StreamingFollowUp)
	child := mkdir(t, root, "child")
	outside := mkdir(t, filepath.Dir(root), "outside")

	if err := m.SelectFolder(ctx, child); err != nil {
		t.Fatalf("SelectFolder() error = %v", err)
	}
	if err := os.Remove(child); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, child); err != nil {
		t.Fatal(err)
	}

	if _, err := m.EnsureStarted(ctx); err == nil {
		t.Fatal("EnsureStarted() error = nil")
	}
	if len(factory.opts) != 0 {
		t.Fatalf("Pi was started with opts %#v", factory.opts)
	}
}

func TestManagerSelectModelSetsActiveClientAndPersists(t *testing.T) {
	ctx := context.Background()
	m, factory, root := setupManager(t, config.StreamingFollowUp)
	if err := m.SelectFolder(ctx, root); err != nil {
		t.Fatal(err)
	}
	if err := m.SelectModel(ctx, "anthropic", "claude"); err != nil {
		t.Fatalf("SelectModel() error = %v", err)
	}
	if got := factory.clients[0].setModels; len(got) != 1 || got[0] != "anthropic/claude" {
		t.Fatalf("setModels = %#v", got)
	}
	if status := m.Status(); status.SelectedModel != "anthropic/claude" {
		t.Fatalf("status = %#v", status)
	}
}

func TestManagerAvailableCommands(t *testing.T) {
	ctx := context.Background()
	m, _, root := setupManager(t, config.StreamingFollowUp)
	if err := m.SelectFolder(ctx, root); err != nil {
		t.Fatal(err)
	}
	commands, err := m.AvailableCommands(ctx)
	if err != nil {
		t.Fatalf("AvailableCommands() error = %v", err)
	}
	if len(commands) != 1 || commands[0].Name != "skill:asana-cli" || commands[0].Source != "skill" {
		t.Fatalf("commands = %#v", commands)
	}
}

func TestManagerEnsureStartedReplacesClosedClient(t *testing.T) {
	ctx := context.Background()
	m, factory, root := setupManager(t, config.StreamingFollowUp)
	if err := m.SelectFolder(ctx, root); err != nil {
		t.Fatal(err)
	}
	firstClient, err := m.EnsureStarted(ctx)
	if err != nil {
		t.Fatalf("EnsureStarted() error = %v", err)
	}
	factory.clients[0].closed = true
	secondClient, err := m.EnsureStarted(ctx)
	if err != nil {
		t.Fatalf("EnsureStarted() after closed client error = %v", err)
	}
	if firstClient == secondClient || len(factory.clients) != 2 {
		t.Fatalf("client was not replaced: first=%p second=%p clients=%d", firstClient, secondClient, len(factory.clients))
	}
}

func TestManagerPromptRoutesIdleAndStreamingMessages(t *testing.T) {
	ctx := context.Background()
	m, factory, root := setupManager(t, config.StreamingFollowUp)
	if err := m.SelectFolder(ctx, root); err != nil {
		t.Fatal(err)
	}
	if err := m.Prompt(ctx, "hello"); err != nil {
		t.Fatalf("Prompt(idle) error = %v", err)
	}
	client := factory.clients[0]
	if len(client.prompts) != 1 || client.prompts[0] != "hello" {
		t.Fatalf("prompts = %#v", client.prompts)
	}
	m.mu.Lock()
	m.isStreaming = true
	m.mu.Unlock()
	if err := m.Prompt(ctx, "later"); err != nil {
		t.Fatalf("Prompt(streaming) error = %v", err)
	}
	if len(client.followUps) != 1 || client.followUps[0] != "later" {
		t.Fatalf("followUps = %#v", client.followUps)
	}
}

func TestManagerPromptUsesSteerWhenConfigured(t *testing.T) {
	ctx := context.Background()
	m, factory, root := setupManager(t, config.StreamingSteer)
	if err := m.SelectFolder(ctx, root); err != nil {
		t.Fatal(err)
	}
	_, err := m.EnsureStarted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.isStreaming = true
	m.mu.Unlock()
	if err := m.Prompt(ctx, "stop"); err != nil {
		t.Fatal(err)
	}
	if got := factory.clients[0].steers; len(got) != 1 || got[0] != "stop" {
		t.Fatalf("steers = %#v", got)
	}
}

func TestManagerFolderChangeStopsExistingClient(t *testing.T) {
	ctx := context.Background()
	m, factory, root := setupManager(t, config.StreamingFollowUp)
	child := mkdir(t, root, "child")
	if err := m.SelectFolder(ctx, root); err != nil {
		t.Fatal(err)
	}
	if _, err := m.EnsureStarted(ctx); err != nil {
		t.Fatal(err)
	}
	first := factory.clients[0]
	if err := m.SelectFolder(ctx, child); err != nil {
		t.Fatal(err)
	}
	if !first.stopped {
		t.Fatal("first client was not stopped")
	}
	if status := m.Status(); status.IsStarted || status.SessionID != "" {
		t.Fatalf("status after folder change = %#v", status)
	}
}

func TestManagerEventsUpdateStreamingState(t *testing.T) {
	ctx := context.Background()
	m, factory, root := setupManager(t, config.StreamingFollowUp)
	if err := m.SelectFolder(ctx, root); err != nil {
		t.Fatal(err)
	}
	if _, err := m.EnsureStarted(ctx); err != nil {
		t.Fatal(err)
	}
	factory.clients[0].events <- pi.Event{Type: "agent_start"}
	waitFor(t, func() bool { return m.Status().IsStreaming })
	factory.clients[0].events <- pi.Event{Type: "agent_end"}
	waitFor(t, func() bool { return !m.Status().IsStreaming })
}

func TestManagerDoesNotDropCriticalEventsWhenRenderQueueFull(t *testing.T) {
	ctx := context.Background()
	m, factory, root := setupManager(t, config.StreamingFollowUp)
	if err := m.SelectFolder(ctx, root); err != nil {
		t.Fatal(err)
	}
	if _, err := m.EnsureStarted(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < cap(m.events); i++ {
		m.events <- pi.Event{Type: "queued"}
	}
	factory.clients[0].events <- pi.Event{Type: "agent_start"}
	waitFor(t, func() bool { return m.Status().IsStreaming })

	deadline := time.After(time.Second)
	for {
		select {
		case event := <-m.Events():
			if event.Type == "agent_start" {
				return
			}
		case <-deadline:
			t.Fatal("critical agent_start was dropped while render queue was full")
		}
	}
}

func TestManagerCancelsDialogExtensionUIRequests(t *testing.T) {
	ctx := context.Background()
	m, factory, root := setupManager(t, config.StreamingFollowUp)
	if err := m.SelectFolder(ctx, root); err != nil {
		t.Fatal(err)
	}
	if _, err := m.EnsureStarted(ctx); err != nil {
		t.Fatal(err)
	}
	client := factory.clients[0]
	client.events <- pi.Event{Type: "extension_ui_request", Raw: []byte(`{"type":"extension_ui_request","id":"ui-1","method":"confirm"}`)}
	waitFor(t, func() bool { return len(client.uiResponses) == 1 })
	if client.uiResponses[0] != "ui-1" || client.uiPayloads[0]["cancelled"] != true {
		t.Fatalf("ui response = %#v %#v", client.uiResponses, client.uiPayloads)
	}
}

func TestManagerAbortNewSessionAndStop(t *testing.T) {
	ctx := context.Background()
	m, factory, root := setupManager(t, config.StreamingFollowUp)
	if err := m.SelectFolder(ctx, root); err != nil {
		t.Fatal(err)
	}
	if err := m.Abort(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.EnsureStarted(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Abort(ctx); err != nil {
		t.Fatal(err)
	}
	if cancelled, err := m.NewSession(ctx); err != nil || cancelled {
		t.Fatalf("NewSession() cancelled=%v err=%v", cancelled, err)
	}
	client := factory.clients[0]
	if client.aborts != 1 || client.newSess != 1 {
		t.Fatalf("aborts=%d newSess=%d", client.aborts, client.newSess)
	}
	if err := m.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if !client.stopped || m.Status().IsStarted {
		t.Fatalf("stopped=%v status=%#v", client.stopped, m.Status())
	}
}

func setupManager(t *testing.T, streamingBehavior string) (*Manager, *fakeFactory, string) {
	t.Helper()
	dir := t.TempDir()
	root := mkdir(t, dir, "root")
	cfg := config.Config{
		Pi: config.PiConfig{
			Binary:                   "pi",
			SessionDir:               filepath.Join(dir, "sessions"),
			DefaultTrust:             config.TrustNoApprove,
			DefaultStreamingBehavior: streamingBehavior,
		},
		Folders: config.FoldersConfig{MaxDepth: 4, MaxEntries: 100, Roots: []config.FolderRoot{{Name: "root", Path: root, Trust: config.TrustNoApprove}}},
		State:   config.StateConfig{Dir: filepath.Join(dir, "state")},
	}
	policy, err := folders.NewPolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	factory := &fakeFactory{}
	manager, err := NewManager(cfg, policy, store.New(cfg.State.Dir), factory)
	if err != nil {
		t.Fatal(err)
	}
	return manager, factory, root
}

func mkdir(t *testing.T, parent, name string) string {
	t.Helper()
	path := filepath.Join(parent, name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func canonical(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}
