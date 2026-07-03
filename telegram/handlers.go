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
	"time"

	"piontg/authz"
	"piontg/pi"
	"piontg/render"
	"piontg/session"
)

const (
	callbackFolderPrefix  = "folder:"
	callbackModelPrefix   = "model:"
	callbackSkillPrefix   = "skill:"
	callbackCommandPrefix = "cmd:"
)

type Handler struct {
	messenger Messenger
	session   Session
	folders   FolderPolicy
	auth      authz.Authorizer
	logger    *slog.Logger

	mu             sync.Mutex
	modelTokens    map[string]pi.ModelInfo
	commandTokens  map[string]pi.CommandInfo
	typingInterval time.Duration
}

func NewHandler(messenger Messenger, sess Session, folderPolicy FolderPolicy, authorizer authz.Authorizer, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{messenger: messenger, session: sess, folders: folderPolicy, auth: authorizer, logger: logger, modelTokens: make(map[string]pi.ModelInfo), commandTokens: make(map[string]pi.CommandInfo), typingInterval: render.DefaultTypingInterval}
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
	typingInterval := h.typingInterval
	if typingInterval <= 0 {
		typingInterval = render.DefaultTypingInterval
	}
	r.SetTypingInterval(typingInterval)
	go func() {
		ticker := time.NewTicker(typingInterval)
		defer ticker.Stop()
		typingActive := h.session.Status().IsStreaming
		if typingActive {
			if err := r.SendTyping(ctx); err != nil {
				h.logger.Warn("send typing heartbeat", "error", err)
			}
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !h.session.Status().IsStreaming {
					typingActive = false
					continue
				}
				typingActive = true
				if err := r.SendTyping(ctx); err != nil {
					h.logger.Warn("send typing heartbeat", "error", err)
				}
			case event, ok := <-h.session.Events():
				if !ok {
					return
				}
				if event.Type == "agent_start" {
					typingActive = true
					if err := r.SendTyping(ctx); err != nil {
						h.logger.Warn("send typing heartbeat", "error", err)
					}
				}
				if err := r.HandleEvent(ctx, event); err != nil {
					h.logger.Warn("render pi event", "type", event.Type, "error", err)
				}
				if event.Type == "agent_end" {
					typingActive = false
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
	if strings.HasPrefix(text, "/skill:") {
		return h.sendPrompt(ctx, msg.ChatID, text)
	}
	if strings.HasPrefix(text, "/") {
		return h.handleCommand(ctx, msg.ChatID, commandName(text))
	}
	return h.sendPrompt(ctx, msg.ChatID, text)
}

func (h *Handler) sendPrompt(ctx context.Context, chatID int64, text string) error {
	if err := h.session.Prompt(ctx, text); err != nil {
		_, _ = h.messenger.SendMessage(ctx, chatID, "Error: "+err.Error(), nil)
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
	case "skills":
		return h.sendSkillPicker(ctx, chatID)
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
	case strings.HasPrefix(data, callbackSkillPrefix):
		token := strings.TrimPrefix(data, callbackSkillPrefix)
		command, ok := h.lookupCommand(token)
		if !ok {
			_ = h.messenger.AnswerCallback(ctx, cb.ID, "Skill list expired")
			_, err := h.messenger.SendMessage(ctx, cb.ChatID, "Skill list expired. Run /skills again.", nil)
			return err
		}
		_ = h.messenger.AnswerCallback(ctx, cb.ID, "Skill details")
		_, err := h.messenger.SendMessage(ctx, cb.ChatID, formatSkillCommandDetails(command), nil)
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
	}, {
		{Text: "Show skills", Data: "cmd:skills"},
	}}

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

func (h *Handler) sendSkillPicker(ctx context.Context, chatID int64) error {
	commands, err := h.session.AvailableCommands(ctx)
	if err != nil {
		_, _ = h.messenger.SendMessage(ctx, chatID, "Could not list skills: "+err.Error(), nil)
		return err
	}
	skills := make([]pi.CommandInfo, 0, len(commands))
	for _, command := range commands {
		if command.Source == "skill" || strings.HasPrefix(command.Name, "skill:") {
			skills = append(skills, command)
		}
	}
	if len(skills) == 0 {
		_, err := h.messenger.SendMessage(ctx, chatID, "No Pi skills available for the selected folder.", nil)
		return err
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	keyboard := make(InlineKeyboard, 0, len(skills))
	h.mu.Lock()
	h.commandTokens = make(map[string]pi.CommandInfo, len(skills))
	for _, skill := range skills {
		token := commandToken(skill)
		h.commandTokens[token] = skill
		label := strings.TrimPrefix(skill.Name, "skill:")
		keyboard = append(keyboard, []Button{{Text: truncate(label, 60), Data: callbackSkillPrefix + token}})
	}
	h.mu.Unlock()
	_, err = h.messenger.SendMessage(ctx, chatID, "Choose a skill to view details:", keyboard)
	return err
}

func (h *Handler) lookupModel(token string) (pi.ModelInfo, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	model, ok := h.modelTokens[token]
	return model, ok
}

func (h *Handler) lookupCommand(token string) (pi.CommandInfo, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	command, ok := h.commandTokens[token]
	return command, ok
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

func commandToken(command pi.CommandInfo) string {
	sum := sha256.Sum256([]byte(command.Name + "\x00" + command.Path))
	return base64.RawURLEncoding.EncodeToString(sum[:])[:16]
}

func formatSkillCommandDetails(command pi.CommandInfo) string {
	usage := "/" + command.Name
	lines := []string{
		"Skill: " + strings.TrimPrefix(command.Name, "skill:"),
	}
	if command.Description != "" {
		lines = append(lines, "", command.Description)
	}
	lines = append(lines, "", "Invoke by sending:", usage+" <request>")
	if command.Location != "" {
		lines = append(lines, "", "Location: "+command.Location)
	}
	if command.Path != "" {
		lines = append(lines, "Path: "+command.Path)
	}
	return strings.Join(lines, "\n")
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
		"/skills - show available Pi skills",
		"/new - start a new Pi session",
		"/abort - abort current Pi turn",
		"/status - show current status",
		"/stop - stop Pi process",
		"/help - show this help",
		"",
		"After selecting a folder/model, send a normal message to chat with Pi.",
		"Skill commands like /skill:name are forwarded to Pi.",
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
