package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"piontg/config"
	"piontg/folders"
	"piontg/pi"
	"piontg/pisessions"
	"piontg/store"
)

type Manager struct {
	cfg     config.Config
	policy  *folders.Policy
	store   *store.Store
	factory ClientFactory

	mu                sync.Mutex
	state             store.State
	client            PiClient
	policyForSelected folders.EffectivePolicy
	isStreaming       bool
	events            chan pi.Event
}

func NewManager(cfg config.Config, policy *folders.Policy, stateStore *store.Store, factory ClientFactory) (*Manager, error) {
	if factory == nil {
		factory = RealClientFactory{}
	}
	loaded, err := stateStore.Load()
	if err != nil {
		// Corrupt primary with usable backup is non-fatal; caller can observe logs in later phases.
		// If no backup exists, Load returns an empty state plus error, which is also safe here.
		loaded = &store.State{}
	}
	m := &Manager{
		cfg:     cfg,
		policy:  policy,
		store:   stateStore,
		factory: factory,
		state:   *loaded,
		events:  make(chan pi.Event, 256),
	}
	needsSave := clearNonDefaultPiSessionState(&m.state)
	if loaded.SelectedFolder != "" {
		canonical, effective, err := policy.Resolve(loaded.SelectedFolder)
		if err == nil {
			if m.state.SelectedFolder != canonical {
				needsSave = true
			}
			m.state.SelectedFolder = canonical
			m.policyForSelected = effective
		} else {
			m.state.SelectedFolder = ""
			m.state.SessionFile = ""
			m.state.SessionID = ""
			needsSave = true
		}
	}
	if needsSave {
		_ = m.store.Save(&m.state)
	}
	return m, nil
}

func clearNonDefaultPiSessionState(state *store.State) bool {
	if state == nil || state.SessionFile == "" {
		return false
	}
	// Only resume sessions from Pi's default persistent session store. Older
	// piontg/Pi versions may have stored paths under <state.dir>/pi-sessions or
	// ~/.pi/agent/<project>; clearing those lets Pi create/use its current
	// default ~/.pi/agent/sessions/<project> location instead.
	defaultSessionsDir, err := defaultPiSessionsDir()
	if err != nil || defaultSessionsDir == "" {
		return false
	}
	if pathWithinDir(defaultSessionsDir, state.SessionFile) {
		return false
	}
	state.SessionFile = ""
	state.SessionID = ""
	return true
}

func defaultPiSessionsDir() (string, error) {
	return pisessions.DefaultDir()
}

func pathWithinDir(dir, path string) bool {
	dirAbs, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(dirAbs, pathAbs)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

func (m *Manager) Events() <-chan pi.Event { return m.events }

func (m *Manager) SelectFolder(ctx context.Context, path string) error {
	canonical, effective, err := m.policy.Resolve(path)
	if err != nil {
		return err
	}
	m.mu.Lock()
	changed := m.state.SelectedFolder != canonical
	client := m.client
	if changed {
		m.client = nil
		m.isStreaming = false
		m.state.SelectedFolder = canonical
		m.state.SessionFile = ""
		m.state.SessionID = ""
		m.policyForSelected = effective
	}
	state := m.state
	m.mu.Unlock()

	if changed && client != nil {
		_ = client.Stop(ctx)
	}
	return m.store.Save(&state)
}

func (m *Manager) SelectModel(ctx context.Context, provider, modelID string) error {
	if provider == "" || modelID == "" {
		return fmt.Errorf("provider and model ID are required")
	}
	client, err := m.EnsureStarted(ctx)
	if err != nil {
		return err
	}
	if _, err := client.SetModel(ctx, provider, modelID); err != nil {
		return err
	}
	m.mu.Lock()
	m.state.SelectedModel = &store.ModelRef{Provider: provider, ID: modelID}
	state := m.state
	m.mu.Unlock()
	return m.store.Save(&state)
}

func (m *Manager) AvailableModels(ctx context.Context) ([]pi.ModelInfo, error) {
	client, err := m.EnsureStarted(ctx)
	if err != nil {
		return nil, err
	}
	return client.GetAvailableModels(ctx)
}

func (m *Manager) AvailableCommands(ctx context.Context) ([]pi.CommandInfo, error) {
	client, err := m.EnsureStarted(ctx)
	if err != nil {
		return nil, err
	}
	return client.GetCommands(ctx)
}

func (m *Manager) ListSessions(ctx context.Context) ([]SessionSummary, error) {
	return pisessions.Discover(ctx, pisessions.Options{
		ResolveFolder: func(path string) (string, error) {
			canonical, _, err := m.policy.Resolve(path)
			return canonical, err
		},
	})
}

func (m *Manager) ResumeSession(ctx context.Context, file string) error {
	summary, effective, err := m.validateSessionForResume(file)
	if err != nil {
		return err
	}

	m.mu.Lock()
	if m.isStreaming {
		m.mu.Unlock()
		return fmt.Errorf("pi is currently streaming; wait or abort before resuming another session")
	}
	previous := m.state
	client := m.client
	m.client = nil
	m.isStreaming = false
	m.state.SelectedFolder = summary.CWD
	m.state.SessionFile = summary.File
	m.state.SessionID = summary.ID
	m.policyForSelected = effective
	resumed := m.state
	m.mu.Unlock()

	if client != nil {
		_ = client.Stop(ctx)
	}
	if err := m.store.Save(&resumed); err != nil {
		m.restoreState(previous)
		return err
	}
	if _, err := m.EnsureStarted(ctx); err != nil {
		m.restoreState(previous)
		_ = m.store.Save(&previous)
		return err
	}
	return nil
}

func (m *Manager) validateSessionForResume(file string) (SessionSummary, folders.EffectivePolicy, error) {
	sessionDir, err := defaultPiSessionsDir()
	if err != nil {
		return SessionSummary{}, folders.EffectivePolicy{}, err
	}
	canonicalFile, err := canonicalSessionFileWithinDir(sessionDir, file)
	if err != nil {
		return SessionSummary{}, folders.EffectivePolicy{}, err
	}
	summary, err := pisessions.ReadSummary(canonicalFile)
	if err != nil {
		return SessionSummary{}, folders.EffectivePolicy{}, err
	}
	canonicalFolder, effective, err := m.policy.Resolve(summary.CWD)
	if err != nil {
		return SessionSummary{}, folders.EffectivePolicy{}, err
	}
	summary.CWD = canonicalFolder
	return summary, effective, nil
}

func canonicalSessionFileWithinDir(dir, file string) (string, error) {
	if strings.TrimSpace(file) == "" {
		return "", fmt.Errorf("session file is required")
	}
	dirAbs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve session dir: %w", err)
	}
	dirResolved, err := filepath.EvalSymlinks(dirAbs)
	if err != nil {
		return "", fmt.Errorf("resolve session dir symlinks: %w", err)
	}
	fileAbs, err := filepath.Abs(file)
	if err != nil {
		return "", fmt.Errorf("resolve session file: %w", err)
	}
	fileResolved, err := filepath.EvalSymlinks(fileAbs)
	if err != nil {
		return "", fmt.Errorf("resolve session file symlinks: %w", err)
	}
	info, err := os.Stat(fileResolved)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("session file %q is a directory", file)
	}
	if !pathWithinDir(dirResolved, fileResolved) {
		return "", fmt.Errorf("session file %q is outside Pi session directory", file)
	}
	return fileResolved, nil
}

func (m *Manager) restoreState(state store.State) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state = state
	m.client = nil
	m.isStreaming = false
	if state.SelectedFolder != "" {
		if _, effective, err := m.policy.Resolve(state.SelectedFolder); err == nil {
			m.policyForSelected = effective
		} else {
			m.policyForSelected = folders.EffectivePolicy{}
		}
	} else {
		m.policyForSelected = folders.EffectivePolicy{}
	}
}

func (m *Manager) Prompt(ctx context.Context, message string) error {
	return m.PromptRequest(ctx, PromptRequest{Message: message})
}

func (m *Manager) PromptRequest(ctx context.Context, req PromptRequest) error {
	client, err := m.EnsureStarted(ctx)
	if err != nil {
		return err
	}
	images := append([]pi.ImageContent(nil), req.Images...)
	if len(images) > 0 {
		state, err := client.GetState(ctx)
		if err != nil {
			return err
		}
		if state.Model != nil && len(state.Model.Input) > 0 && !modelSupportsImageInput(state.Model) {
			return fmt.Errorf("selected model does not support image input; use /model to choose an image-capable model")
		}
	}
	m.mu.Lock()
	streaming := m.isStreaming
	behavior := m.cfg.Pi.DefaultStreamingBehavior
	m.mu.Unlock()
	if !streaming {
		return client.Prompt(ctx, req.Message, images...)
	}
	if behavior == config.StreamingSteer {
		return client.Steer(ctx, req.Message, images...)
	}
	return client.FollowUp(ctx, req.Message, images...)
}

func modelSupportsImageInput(model *pi.ModelInfo) bool {
	if model == nil {
		return true
	}
	for _, input := range model.Input {
		if strings.EqualFold(input, "image") {
			return true
		}
	}
	return false
}

func (m *Manager) Abort(ctx context.Context) error {
	m.mu.Lock()
	client := m.client
	m.mu.Unlock()
	if client == nil {
		return nil
	}
	return client.Abort(ctx)
}

func (m *Manager) RespondExtensionUI(ctx context.Context, requestID string, payload map[string]any) error {
	if requestID == "" {
		return fmt.Errorf("extension UI request ID is required")
	}
	m.mu.Lock()
	client := m.client
	m.mu.Unlock()
	if client == nil || clientClosed(client) {
		return fmt.Errorf("pi is not started")
	}
	return client.RespondExtensionUI(ctx, requestID, payload)
}

func (m *Manager) NewSession(ctx context.Context) (bool, error) {
	client, err := m.EnsureStarted(ctx)
	if err != nil {
		return false, err
	}
	cancelled, err := client.NewSession(ctx)
	if err != nil || cancelled {
		return cancelled, err
	}
	return false, m.refreshState(ctx, client)
}

func (m *Manager) Stop(ctx context.Context) error {
	m.mu.Lock()
	client := m.client
	m.client = nil
	m.isStreaming = false
	m.mu.Unlock()
	if client == nil {
		return nil
	}
	return client.Stop(ctx)
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	status := Status{
		SelectedFolder: m.state.SelectedFolder,
		SessionFile:    m.state.SessionFile,
		SessionID:      m.state.SessionID,
		IsStarted:      m.client != nil && !clientClosed(m.client),
		IsStreaming:    m.isStreaming,
	}
	if m.state.SelectedModel != nil {
		status.SelectedModel = m.state.SelectedModel.Provider + "/" + m.state.SelectedModel.ID
	}
	if m.client != nil {
		status.StderrTail = m.client.StderrTail()
	}
	return status
}

func (m *Manager) EnsureStarted(ctx context.Context) (PiClient, error) {
	m.mu.Lock()
	if m.client != nil {
		client := m.client
		if !clientClosed(client) {
			m.mu.Unlock()
			return client, nil
		}
		m.client = nil
		m.isStreaming = false
	}
	if m.state.SelectedFolder == "" {
		m.mu.Unlock()
		return nil, fmt.Errorf("no folder selected")
	}
	state := m.state
	m.mu.Unlock()

	selectedBefore := state.SelectedFolder
	canonical, effective, err := m.policy.Resolve(selectedBefore)
	if err != nil {
		return nil, fmt.Errorf("selected folder is no longer allowed or available: %w", err)
	}
	if canonical != state.SelectedFolder {
		state.SelectedFolder = canonical
		state.SessionFile = ""
		state.SessionID = ""
	}

	model := ""
	if state.SelectedModel != nil {
		model = state.SelectedModel.Provider + "/" + state.SelectedModel.ID
	}
	opts := pi.Options{
		Binary:       m.cfg.Pi.Binary,
		CWD:          state.SelectedFolder,
		SessionFile:  state.SessionFile,
		Model:        model,
		Trust:        effective.Trust,
		Tools:        chooseStrings(effective.Tools, m.cfg.Pi.Tools),
		ExcludeTools: chooseStrings(effective.ExcludeTools, m.cfg.Pi.ExcludeTools),
	}
	client, err := m.factory.Start(ctx, opts)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.state.SelectedFolder != selectedBefore {
		m.mu.Unlock()
		_ = client.Stop(ctx)
		return m.EnsureStarted(ctx)
	}
	if state.SelectedFolder != selectedBefore {
		m.state.SelectedFolder = state.SelectedFolder
		m.state.SessionFile = ""
		m.state.SessionID = ""
		m.policyForSelected = effective
	}
	// Another goroutine may have started one while this process spawned. Keep the first live one and stop ours.
	if m.client != nil {
		if !clientClosed(m.client) {
			existing := m.client
			m.mu.Unlock()
			_ = client.Stop(ctx)
			return existing, nil
		}
		m.client = nil
		m.isStreaming = false
	}
	m.client = client
	m.mu.Unlock()
	go m.forwardEvents(client)
	_ = m.refreshState(ctx, client)
	return client, nil
}

func (m *Manager) refreshState(ctx context.Context, client PiClient) error {
	state, err := client.GetState(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.state.SessionFile = state.SessionFile
	m.state.SessionID = state.SessionID
	m.isStreaming = state.IsStreaming
	stored := m.state
	m.mu.Unlock()
	return m.store.Save(&stored)
}

func (m *Manager) forwardEvents(client PiClient) {
	for event := range client.Events() {
		if event.Type == "extension_ui_request" {
			m.handleExtensionUIRequest(client, event)
		}
		m.mu.Lock()
		switch event.Type {
		case "agent_start":
			m.isStreaming = true
		case "agent_end":
			m.isStreaming = false
		}
		m.mu.Unlock()
		if isCriticalStreamEvent(event.Type) {
			m.events <- event
			continue
		}
		select {
		case m.events <- event:
		default:
		}
	}
	m.mu.Lock()
	if m.client == client {
		m.client = nil
		m.isStreaming = false
	}
	m.mu.Unlock()
}

func isCriticalStreamEvent(typ string) bool {
	switch typ {
	case "message_update", "agent_start", "agent_end":
		return true
	default:
		return false
	}
}

func (m *Manager) handleExtensionUIRequest(client PiClient, event pi.Event) {
	var request struct {
		ID     string `json:"id"`
		Method string `json:"method"`
	}
	if err := json.Unmarshal(event.Raw, &request); err != nil || request.ID == "" {
		return
	}
	switch request.Method {
	case "input", "editor":
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.RespondExtensionUI(ctx, request.ID, map[string]any{"cancelled": true})
	}
}

func clientClosed(client PiClient) bool {
	closed, ok := client.(interface{ IsClosed() bool })
	return ok && closed.IsClosed()
}

func chooseStrings(primary, fallback []string) []string {
	if len(primary) > 0 {
		return append([]string(nil), primary...)
	}
	return append([]string(nil), fallback...)
}
