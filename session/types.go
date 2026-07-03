package session

import (
	"context"

	"piontg/pi"
)

// PiClient is the subset of Pi RPC behavior needed by the session manager.
type PiClient interface {
	Events() <-chan pi.Event
	Stop(ctx context.Context) error
	GetState(ctx context.Context) (pi.SessionState, error)
	GetAvailableModels(ctx context.Context) ([]pi.ModelInfo, error)
	GetCommands(ctx context.Context) ([]pi.CommandInfo, error)
	SetModel(ctx context.Context, provider, modelID string) (pi.ModelInfo, error)
	Prompt(ctx context.Context, message string) error
	FollowUp(ctx context.Context, message string) error
	Steer(ctx context.Context, message string) error
	Abort(ctx context.Context) error
	RespondExtensionUI(ctx context.Context, requestID string, payload map[string]any) error
	NewSession(ctx context.Context) (bool, error)
	StderrTail() string
}

type ClientFactory interface {
	Start(ctx context.Context, opts pi.Options) (PiClient, error)
}

type RealClientFactory struct{}

func (RealClientFactory) Start(ctx context.Context, opts pi.Options) (PiClient, error) {
	client := pi.NewClient(opts)
	if err := client.Start(ctx); err != nil {
		return nil, err
	}
	return client, nil
}

type Status struct {
	SelectedFolder string
	SelectedModel  string
	SessionFile    string
	SessionID      string
	IsStarted      bool
	IsStreaming    bool
	StderrTail     string
}
