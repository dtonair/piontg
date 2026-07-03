package session

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"piontg/config"
	"piontg/folders"
	"piontg/pi"
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
	if loaded.SelectedFolder != "" {
		canonical, effective, err := policy.Resolve(loaded.SelectedFolder)
		if err == nil {
			m.state.SelectedFolder = canonical
			m.policyForSelected = effective
		} else {
			m.state.SelectedFolder = ""
			m.state.SessionFile = ""
			m.state.SessionID = ""
			_ = m.store.Save(&m.state)
		}
	}
	return m, nil
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

func (m *Manager) Prompt(ctx context.Context, message string) error {
	client, err := m.EnsureStarted(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	streaming := m.isStreaming
	behavior := m.cfg.Pi.DefaultStreamingBehavior
	m.mu.Unlock()
	if !streaming {
		return client.Prompt(ctx, message)
	}
	if behavior == config.StreamingSteer {
		return client.Steer(ctx, message)
	}
	return client.FollowUp(ctx, message)
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
	effective := m.policyForSelected
	m.mu.Unlock()

	model := ""
	if state.SelectedModel != nil {
		model = state.SelectedModel.Provider + "/" + state.SelectedModel.ID
	}
	opts := pi.Options{
		Binary:       m.cfg.Pi.Binary,
		CWD:          state.SelectedFolder,
		SessionDir:   m.cfg.Pi.SessionDir,
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

func (m *Manager) handleExtensionUIRequest(client PiClient, event pi.Event) {
	var request struct {
		ID     string `json:"id"`
		Method string `json:"method"`
	}
	if err := json.Unmarshal(event.Raw, &request); err != nil || request.ID == "" {
		return
	}
	switch request.Method {
	case "select", "confirm", "input", "editor":
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
