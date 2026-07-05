package telegram

import (
	"context"

	"piontg/folders"
	"piontg/pi"
	"piontg/session"
)

type Messenger interface {
	SendMessage(ctx context.Context, chatID int64, text string, keyboard InlineKeyboard) (int, error)
	EditMessage(ctx context.Context, chatID int64, messageID int, text string) error
	SendChatAction(ctx context.Context, chatID int64, action string) error
	AnswerCallback(ctx context.Context, callbackID, text string) error
}

type InlineKeyboard [][]Button

type Button struct {
	Text string
	Data string
}

type Update struct {
	Message  *Message
	Callback *Callback
}

type Message struct {
	ChatID       int64
	UserID       int64
	Text         string
	Caption      string
	Images       []ImageRef
	MediaGroupID string
}

type ImageRef struct {
	FileID       string
	FileUniqueID string
	Size         int64
	Width        int
	Height       int
	Source       string
}

type Callback struct {
	ID        string
	ChatID    int64
	UserID    int64
	MessageID int
	Data      string
}

type Session interface {
	SelectFolder(ctx context.Context, path string) error
	SelectModel(ctx context.Context, provider, modelID string) error
	AvailableModels(ctx context.Context) ([]pi.ModelInfo, error)
	AvailableCommands(ctx context.Context) ([]pi.CommandInfo, error)
	Prompt(ctx context.Context, message string) error
	PromptRequest(ctx context.Context, req session.PromptRequest) error
	RespondExtensionUI(ctx context.Context, requestID string, payload map[string]any) error
	Abort(ctx context.Context) error
	NewSession(ctx context.Context) (bool, error)
	Stop(ctx context.Context) error
	Status() session.Status
	Events() <-chan pi.Event
}

type ImageFetcher interface {
	FetchImage(ctx context.Context, ref ImageRef) (pi.ImageContent, error)
}

type FolderPolicy interface {
	Discover() ([]folders.Choice, error)
	ResolveToken(token string) (string, folders.EffectivePolicy, error)
}
