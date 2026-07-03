package telegram

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"piontg/authz"
	"piontg/pi"
	"piontg/render"
	"piontg/session"
)

const (
	callbackFolderPrefix  = "folder:"
	callbackModelPrefix   = "model:"
	callbackCommandPrefix = "cmd:"
)

type Handler struct {
	messenger Messenger
	session   Session
	folders   FolderPolicy
	auth      authz.Authorizer
	logger    *slog.Logger

	mu          sync.Mutex
	modelTokens map[string]pi.ModelInfo
}

func NewHandler(messenger Messenger, sess Session, folderPolicy FolderPolicy, authorizer authz.Authorizer, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{messenger: messenger, session: sess, folders: folderPolicy, auth: authorizer, logger: logger, modelTokens: make(map[string]pi.ModelInfo)}
}

func (h *Handler) HandleUpdate(ctx context.Context, update Update) error {
	if update.Message != nil {
		return h.handleMessage(ctx, *update.Message)
	}
	if update.Callback != nil {
		return h.handleCallback(ctx, *update.Callback)
	}
	return nil
}

func (h *Handler) StartEventRendering(ctx context.Context, chatID int64) {
	sink := &RenderSink{Messenger: h.messenger, ChatID: chatID}
	r := render.New(sink)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-h.session.Events():
				if !ok {
					return
				}
				if err := r.HandleEvent(ctx, event); err != nil {
					h.logger.Warn("render pi event", "type", event.Type, "error", err)
				}
			}
		}
	}()
}

func (h *Handler) handleMessage(ctx context.Context, msg Message) error {
	if !h.auth.IsAllowed(msg.UserID) {
		_, _ = h.messenger.SendMessage(ctx, msg.ChatID, "Not authorized.", nil)
		return nil
	}
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return nil
	}
	if strings.HasPrefix(text, "/") {
		return h.handleCommand(ctx, msg.ChatID, commandName(text))
	}
	if err := h.session.Prompt(ctx, text); err != nil {
		_, _ = h.messenger.SendMessage(ctx, msg.ChatID, "Error: "+err.Error(), nil)
		return err
	}
	return nil
}

func (h *Handler) handleCommand(ctx context.Context, chatID int64, command string) error {
	switch command {
	case "start":
		return h.sendStart(ctx, chatID)
	case "help":
		_, err := h.messenger.SendMessage(ctx, chatID, helpText(), nil)
		return err
	case "folder":
		return h.sendFolderPicker(ctx, chatID)
	case "model":
		return h.sendModelPicker(ctx, chatID)
	case "new":
		cancelled, err := h.session.NewSession(ctx)
		if err != nil {
			_, _ = h.messenger.SendMessage(ctx, chatID, "Could not start new session: "+err.Error(), nil)
			return err
		}
		if cancelled {
			_, err = h.messenger.SendMessage(ctx, chatID, "New session cancelled.", nil)
			return err
		}
		_, err = h.messenger.SendMessage(ctx, chatID, "Started a new Pi session.", nil)
		return err
	case "abort":
		if err := h.session.Abort(ctx); err != nil {
			_, _ = h.messenger.SendMessage(ctx, chatID, "Abort failed: "+err.Error(), nil)
			return err
		}
		_, err := h.messenger.SendMessage(ctx, chatID, "Abort requested.", nil)
		return err
	case "status":
		_, err := h.messenger.SendMessage(ctx, chatID, formatStatus(h.session.Status()), nil)
		return err
	case "stop":
		if err := h.session.Stop(ctx); err != nil {
			_, _ = h.messenger.SendMessage(ctx, chatID, "Stop failed: "+err.Error(), nil)
			return err
		}
		_, err := h.messenger.SendMessage(ctx, chatID, "Pi stopped.", nil)
		return err
	default:
		_, err := h.messenger.SendMessage(ctx, chatID, "Unknown command. Use /help.", nil)
		return err
	}
}

func (h *Handler) handleCallback(ctx context.Context, cb Callback) error {
	if !h.auth.IsAllowed(cb.UserID) {
		return h.messenger.AnswerCallback(ctx, cb.ID, "Not authorized")
	}
	data := cb.Data
	switch {
	case strings.HasPrefix(data, callbackCommandPrefix):
		_ = h.messenger.AnswerCallback(ctx, cb.ID, "OK")
		return h.handleCommand(ctx, cb.ChatID, strings.TrimPrefix(data, callbackCommandPrefix))
	case strings.HasPrefix(data, callbackFolderPrefix):
		token := strings.TrimPrefix(data, callbackFolderPrefix)
		path, _, err := h.folders.ResolveToken(token)
		if err != nil {
			_ = h.messenger.AnswerCallback(ctx, cb.ID, "Folder unavailable")
			_, _ = h.messenger.SendMessage(ctx, cb.ChatID, "Folder unavailable: "+err.Error(), nil)
			return err
		}
		if err := h.session.SelectFolder(ctx, path); err != nil {
			_ = h.messenger.AnswerCallback(ctx, cb.ID, "Folder select failed")
			_, _ = h.messenger.SendMessage(ctx, cb.ChatID, "Could not select folder: "+err.Error(), nil)
			return err
		}
		_ = h.messenger.AnswerCallback(ctx, cb.ID, "Folder selected")
		_, err = h.messenger.SendMessage(ctx, cb.ChatID, "Selected folder:\n"+path+"\n\nUse /model to choose a model, then send a message.", nil)
		return err
	case strings.HasPrefix(data, callbackModelPrefix):
		token := strings.TrimPrefix(data, callbackModelPrefix)
		model, ok := h.lookupModel(token)
		if !ok {
			_ = h.messenger.AnswerCallback(ctx, cb.ID, "Model list expired")
			_, err := h.messenger.SendMessage(ctx, cb.ChatID, "Model list expired. Run /model again.", nil)
			return err
		}
		if err := h.session.SelectModel(ctx, model.Provider, model.ID); err != nil {
			_ = h.messenger.AnswerCallback(ctx, cb.ID, "Model select failed")
			_, _ = h.messenger.SendMessage(ctx, cb.ChatID, "Could not select model: "+err.Error(), nil)
			return err
		}
		_ = h.messenger.AnswerCallback(ctx, cb.ID, "Model selected")
		_, err := h.messenger.SendMessage(ctx, cb.ChatID, "Selected model: "+model.Provider+"/"+model.ID, nil)
		return err
	default:
		return h.messenger.AnswerCallback(ctx, cb.ID, "Unknown action")
	}
}

func (h *Handler) sendStart(ctx context.Context, chatID int64) error {
	keyboard := InlineKeyboard{{
		{Text: "Choose folder", Data: "cmd:folder"},
		{Text: "Choose model", Data: "cmd:model"},
	}}
	// cmd:* callbacks are intentionally not handled yet; commands are clearer and less stateful.
	_, err := h.messenger.SendMessage(ctx, chatID, "Pi Telegram bot ready.\n\n"+formatStatus(h.session.Status())+"\n\nUse /folder to begin.", keyboard)
	return err
}

func (h *Handler) sendFolderPicker(ctx context.Context, chatID int64) error {
	choices, err := h.folders.Discover()
	if err != nil {
		_, _ = h.messenger.SendMessage(ctx, chatID, "Could not list folders: "+err.Error(), nil)
		return err
	}
	if len(choices) == 0 {
		_, err := h.messenger.SendMessage(ctx, chatID, "No folders configured.", nil)
		return err
	}
	keyboard := make(InlineKeyboard, 0, len(choices))
	for _, choice := range choices {
		keyboard = append(keyboard, []Button{{Text: truncate(choice.Label, 60), Data: callbackFolderPrefix + choice.Token}})
	}
	_, err = h.messenger.SendMessage(ctx, chatID, "Choose a folder:", keyboard)
	return err
}

func (h *Handler) sendModelPicker(ctx context.Context, chatID int64) error {
	models, err := h.session.AvailableModels(ctx)
	if err != nil {
		_, _ = h.messenger.SendMessage(ctx, chatID, "Could not list models: "+err.Error(), nil)
		return err
	}
	if len(models) == 0 {
		_, err := h.messenger.SendMessage(ctx, chatID, "No Pi models available. Configure Pi auth/API keys first.", nil)
		return err
	}
	sort.Slice(models, func(i, j int) bool {
		return models[i].Provider+models[i].ID < models[j].Provider+models[j].ID
	})
	keyboard := make(InlineKeyboard, 0, len(models))
	h.mu.Lock()
	h.modelTokens = make(map[string]pi.ModelInfo, len(models))
	for _, model := range models {
		token := modelToken(model)
		h.modelTokens[token] = model
		label := model.Provider + "/" + model.ID
		if model.Name != "" {
			label = model.Provider + "/" + model.Name
		}
		keyboard = append(keyboard, []Button{{Text: truncate(label, 60), Data: callbackModelPrefix + token}})
	}
	h.mu.Unlock()
	_, err = h.messenger.SendMessage(ctx, chatID, "Choose a model:", keyboard)
	return err
}

func (h *Handler) lookupModel(token string) (pi.ModelInfo, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	model, ok := h.modelTokens[token]
	return model, ok
}

func commandName(text string) string {
	name := strings.TrimPrefix(strings.Fields(text)[0], "/")
	if idx := strings.IndexByte(name, '@'); idx >= 0 {
		name = name[:idx]
	}
	return strings.ToLower(name)
}

func modelToken(model pi.ModelInfo) string {
	sum := sha256.Sum256([]byte(model.Provider + "/" + model.ID))
	return base64.RawURLEncoding.EncodeToString(sum[:])[:16]
}

func formatStatus(status session.Status) string {
	folder := status.SelectedFolder
	if folder == "" {
		folder = "(none)"
	}
	model := status.SelectedModel
	if model == "" {
		model = "(none)"
	}
	return fmt.Sprintf("Status:\nFolder: %s\nModel: %s\nStarted: %v\nStreaming: %v\nSession: %s", folder, model, status.IsStarted, status.IsStreaming, emptyDefault(status.SessionID, "(none)"))
}

func helpText() string {
	return strings.Join([]string{
		"Commands:",
		"/start - show current state",
		"/folder - choose a configured folder",
		"/model - choose a Pi model",
		"/new - start a new Pi session",
		"/abort - abort current Pi turn",
		"/status - show current status",
		"/stop - stop Pi process",
		"/help - show this help",
		"",
		"After selecting a folder/model, send a normal message to chat with Pi.",
	}, "\n")
}

func emptyDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return string(r[:max])
	}
	return string(r[:max-1]) + "…"
}
