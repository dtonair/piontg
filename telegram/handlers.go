package telegram

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
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
	callbackUIPrefix      = "ui:"

	extensionUIDefaultTTL      = 5 * time.Minute
	extensionUITimeoutGrace    = 2 * time.Second
	extensionUIMaxOptions      = 20
	extensionUIButtonLabelMax  = 60
	extensionUIMessageTextMax  = 3500
	extensionUICallbackDataMax = 64
)

type Handler struct {
	messenger    Messenger
	session      Session
	folders      FolderPolicy
	auth         authz.Authorizer
	imageFetcher ImageFetcher
	logger       *slog.Logger

	mu             sync.Mutex
	modelTokens    map[string]pi.ModelInfo
	commandTokens  map[string]pi.CommandInfo
	uiTokens       map[string]pendingExtensionUI
	uiHandled      map[string]time.Time
	typingInterval time.Duration
}

// extensionUIRequest is the subset of Pi RPC extension_ui_request fields that
// piontg can render or route through Telegram.
type extensionUIRequest struct {
	ID          string   `json:"id"`
	Method      string   `json:"method"`
	Title       string   `json:"title,omitempty"`
	Message     string   `json:"message,omitempty"`
	Options     []string `json:"options,omitempty"`
	TimeoutMS   int      `json:"timeout,omitempty"`
	NotifyType  string   `json:"notifyType,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
	Prefill     string   `json:"prefill,omitempty"`
}

type pendingExtensionUI struct {
	RequestID string
	Method    string
	Options   []string
	ExpiresAt time.Time
	MessageID int
}

func NewHandler(messenger Messenger, sess Session, folderPolicy FolderPolicy, authorizer authz.Authorizer, logger *slog.Logger) *Handler {
	return NewHandlerWithImageFetcher(messenger, sess, folderPolicy, authorizer, nil, logger)
}

func NewHandlerWithImageFetcher(messenger Messenger, sess Session, folderPolicy FolderPolicy, authorizer authz.Authorizer, imageFetcher ImageFetcher, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{messenger: messenger, session: sess, folders: folderPolicy, auth: authorizer, imageFetcher: imageFetcher, logger: logger, modelTokens: make(map[string]pi.ModelInfo), commandTokens: make(map[string]pi.CommandInfo), uiTokens: make(map[string]pendingExtensionUI), uiHandled: make(map[string]time.Time), typingInterval: render.DefaultTypingInterval}
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
				if h.handleExtensionUIEvent(ctx, chatID, event) {
					continue
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
	if len(msg.Images) > 0 {
		return h.sendImagePrompt(ctx, msg)
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

func (h *Handler) sendImagePrompt(ctx context.Context, msg Message) error {
	if h.imageFetcher == nil {
		err := fmt.Errorf("image support is not configured")
		_, _ = h.messenger.SendMessage(ctx, msg.ChatID, "Error: "+err.Error(), nil)
		return err
	}
	prompt := strings.TrimSpace(msg.Caption)
	if prompt == "" {
		prompt = "Please analyze the attached image."
	}
	image, err := h.imageFetcher.FetchImage(ctx, msg.Images[0])
	if err != nil {
		_, _ = h.messenger.SendMessage(ctx, msg.ChatID, "Could not fetch image: "+err.Error(), nil)
		return err
	}
	if err := h.session.PromptRequest(ctx, session.PromptRequest{Message: prompt, Images: []pi.ImageContent{image}}); err != nil {
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
	case strings.HasPrefix(data, callbackUIPrefix):
		return h.handleExtensionUICallback(ctx, cb)
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

func (h *Handler) handleExtensionUICallback(ctx context.Context, cb Callback) error {
	parsed, err := parseExtensionUICallbackData(cb.Data)
	if err != nil {
		return h.messenger.AnswerCallback(ctx, cb.ID, "Invalid approval action")
	}
	pending, ok, expired := h.resolvePendingExtensionUI(parsed.token, time.Now())
	if expired {
		h.logger.Warn("expired extension UI callback", "token", parsed.token, "action", parsed.action)
		return h.messenger.AnswerCallback(ctx, cb.ID, "Approval expired. Please retry.")
	}
	if !ok {
		if h.extensionUITokenHandled(parsed.token, time.Now()) {
			h.logger.Warn("duplicate extension UI callback", "token", parsed.token, "action", parsed.action)
			return h.messenger.AnswerCallback(ctx, cb.ID, "Already handled.")
		}
		h.logger.Warn("unknown extension UI callback", "token", parsed.token, "action", parsed.action)
		return h.messenger.AnswerCallback(ctx, cb.ID, "Approval not found. Please retry.")
	}

	payload, label, err := extensionUIResponsePayload(pending, parsed)
	if err != nil {
		h.logger.Warn("invalid extension UI callback", "request_id", pending.RequestID, "method", pending.Method, "action", parsed.action, "error", err)
		return h.messenger.AnswerCallback(ctx, cb.ID, "Invalid approval action")
	}
	if err := h.session.RespondExtensionUI(ctx, pending.RequestID, payload); err != nil {
		h.logger.Warn("respond extension UI", "request_id", pending.RequestID, "method", pending.Method, "error", err)
		_, _ = h.messenger.SendMessage(ctx, cb.ChatID, "Could not answer Pi approval request: "+err.Error(), nil)
		return h.messenger.AnswerCallback(ctx, cb.ID, "Approval failed")
	}
	h.markExtensionUITokenHandled(parsed.token, time.Now())
	return h.messenger.AnswerCallback(ctx, cb.ID, label)
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

func (h *Handler) handleExtensionUIEvent(ctx context.Context, chatID int64, event pi.Event) bool {
	if event.Type != "extension_ui_request" {
		return false
	}
	request, err := parseExtensionUIRequest(event)
	if err != nil {
		h.logger.Warn("parse extension UI request", "error", err)
		return true
	}
	switch request.Method {
	case "select":
		if err := h.sendExtensionUISelect(ctx, chatID, request); err != nil {
			h.logger.Warn("send extension UI select", "request_id", request.ID, "error", err)
		}
	case "confirm":
		if err := h.sendExtensionUIConfirm(ctx, chatID, request); err != nil {
			h.logger.Warn("send extension UI confirm", "request_id", request.ID, "error", err)
		}
	case "notify":
		if err := h.sendExtensionUINotify(ctx, chatID, request); err != nil {
			h.logger.Warn("send extension UI notify", "request_id", request.ID, "error", err)
		}
	case "input", "editor":
		_, err := h.messenger.SendMessage(ctx, chatID, "Pi requested an unsupported Telegram UI dialog ("+request.Method+"); it was cancelled.", nil)
		if err != nil {
			h.logger.Warn("send unsupported extension UI notice", "request_id", request.ID, "method", request.Method, "error", err)
		}
	default:
		h.logger.Warn("unsupported extension UI method", "request_id", request.ID, "method", request.Method)
	}
	return true
}

func (h *Handler) sendExtensionUISelect(ctx context.Context, chatID int64, request extensionUIRequest) error {
	if len(request.Options) == 0 {
		h.cancelExtensionUIRequest(ctx, request, "select request has no options")
		_, err := h.messenger.SendMessage(ctx, chatID, "Pi requested a selection with no options; it was cancelled.", nil)
		return err
	}
	if len(request.Options) > extensionUIMaxOptions {
		h.cancelExtensionUIRequest(ctx, request, "select request has too many options")
		_, err := h.messenger.SendMessage(ctx, chatID, fmt.Sprintf("Pi requested a selection with %d options, above the Telegram limit of %d; it was cancelled.", len(request.Options), extensionUIMaxOptions), nil)
		return err
	}
	token, pending, err := h.storePendingExtensionUI(request, time.Now())
	if err != nil {
		return err
	}
	keyboard := make(InlineKeyboard, 0, len(pending.Options)+1)
	for i, option := range pending.Options {
		keyboard = append(keyboard, []Button{{Text: truncate(option, extensionUIButtonLabelMax), Data: extensionUICallbackData(token, "opt", i)}})
	}
	keyboard = append(keyboard, []Button{{Text: "Cancel", Data: extensionUICallbackData(token, "cancel", -1)}})
	messageID, err := h.messenger.SendMessage(ctx, chatID, formatExtensionUIRequest("Pi needs a selection", request), keyboard)
	if err != nil {
		h.mu.Lock()
		delete(h.uiTokens, token)
		h.mu.Unlock()
		h.cancelExtensionUIRequest(ctx, request, "send Telegram select failed")
		return err
	}
	h.setPendingExtensionUIMessageID(token, messageID)
	return nil
}

func (h *Handler) sendExtensionUIConfirm(ctx context.Context, chatID int64, request extensionUIRequest) error {
	token, _, err := h.storePendingExtensionUI(request, time.Now())
	if err != nil {
		return err
	}
	keyboard := InlineKeyboard{{
		{Text: "Approve", Data: extensionUICallbackData(token, "yes", -1)},
		{Text: "Deny", Data: extensionUICallbackData(token, "no", -1)},
	}}
	messageID, err := h.messenger.SendMessage(ctx, chatID, formatExtensionUIRequest("Pi needs confirmation", request), keyboard)
	if err != nil {
		h.mu.Lock()
		delete(h.uiTokens, token)
		h.mu.Unlock()
		h.cancelExtensionUIRequest(ctx, request, "send Telegram confirm failed")
		return err
	}
	h.setPendingExtensionUIMessageID(token, messageID)
	return nil
}

func (h *Handler) sendExtensionUINotify(ctx context.Context, chatID int64, request extensionUIRequest) error {
	text := strings.TrimSpace(request.Message)
	if text == "" {
		text = strings.TrimSpace(request.Title)
	}
	if text == "" {
		text = "Pi sent a notification."
	}
	prefix := "Pi notification"
	if request.NotifyType != "" {
		prefix += " (" + request.NotifyType + ")"
	}
	_, err := h.messenger.SendMessage(ctx, chatID, truncate(prefix+":\n"+text, extensionUIMessageTextMax), nil)
	return err
}

func (h *Handler) cancelExtensionUIRequest(ctx context.Context, request extensionUIRequest, reason string) {
	if request.ID == "" {
		return
	}
	if err := h.session.RespondExtensionUI(ctx, request.ID, map[string]any{"cancelled": true}); err != nil {
		h.logger.Warn("cancel extension UI request", "request_id", request.ID, "method", request.Method, "reason", reason, "error", err)
	}
}

func (h *Handler) setPendingExtensionUIMessageID(token string, messageID int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	pending, ok := h.uiTokens[token]
	if !ok {
		return
	}
	pending.MessageID = messageID
	h.uiTokens[token] = pending
}

func formatExtensionUIRequest(defaultTitle string, request extensionUIRequest) string {
	title := strings.TrimSpace(request.Title)
	if title == "" {
		title = defaultTitle
	}
	parts := []string{title}
	if message := strings.TrimSpace(request.Message); message != "" {
		parts = append(parts, "", message)
	}
	return truncate(strings.Join(parts, "\n"), extensionUIMessageTextMax)
}

func extensionUICallbackData(token, action string, index int) string {
	data := callbackUIPrefix + token + ":" + action
	if index >= 0 {
		data += ":" + strconv.Itoa(index)
	}
	return data
}

type extensionUICallback struct {
	token  string
	action string
	index  int
}

func parseExtensionUICallbackData(data string) (extensionUICallback, error) {
	if !strings.HasPrefix(data, callbackUIPrefix) {
		return extensionUICallback{}, fmt.Errorf("missing UI callback prefix")
	}
	parts := strings.Split(strings.TrimPrefix(data, callbackUIPrefix), ":")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		return extensionUICallback{}, fmt.Errorf("invalid UI callback format")
	}
	parsed := extensionUICallback{token: parts[0], action: parts[1], index: -1}
	if len(parts) == 3 {
		index, err := strconv.Atoi(parts[2])
		if err != nil || index < 0 {
			return extensionUICallback{}, fmt.Errorf("invalid UI callback option index")
		}
		parsed.index = index
	}
	return parsed, nil
}

func extensionUIResponsePayload(pending pendingExtensionUI, cb extensionUICallback) (map[string]any, string, error) {
	switch cb.action {
	case "opt":
		if pending.Method != "select" {
			return nil, "", fmt.Errorf("option action for %s request", pending.Method)
		}
		value, ok := extensionUIOption(pending, cb.index)
		if !ok {
			return nil, "", fmt.Errorf("option index %d out of range", cb.index)
		}
		return map[string]any{"value": value}, "Selected", nil
	case "cancel":
		return map[string]any{"cancelled": true}, "Cancelled", nil
	case "yes":
		if pending.Method != "confirm" {
			return nil, "", fmt.Errorf("approve action for %s request", pending.Method)
		}
		return map[string]any{"confirmed": true}, "Approved", nil
	case "no":
		if pending.Method != "confirm" {
			return nil, "", fmt.Errorf("deny action for %s request", pending.Method)
		}
		return map[string]any{"confirmed": false}, "Denied", nil
	default:
		return nil, "", fmt.Errorf("unknown action %q", cb.action)
	}
}

func parseExtensionUIRequest(event pi.Event) (extensionUIRequest, error) {
	if event.Type != "extension_ui_request" {
		return extensionUIRequest{}, fmt.Errorf("unexpected event type %q", event.Type)
	}
	var request extensionUIRequest
	if err := json.Unmarshal(event.Raw, &request); err != nil {
		return extensionUIRequest{}, fmt.Errorf("parse extension UI request: %w", err)
	}
	if request.ID == "" {
		return extensionUIRequest{}, fmt.Errorf("extension UI request ID is required")
	}
	if request.Method == "" {
		return extensionUIRequest{}, fmt.Errorf("extension UI request method is required")
	}
	return request, nil
}

func (h *Handler) storePendingExtensionUI(request extensionUIRequest, now time.Time) (string, pendingExtensionUI, error) {
	if request.ID == "" {
		return "", pendingExtensionUI{}, fmt.Errorf("extension UI request ID is required")
	}
	if request.Method == "" {
		return "", pendingExtensionUI{}, fmt.Errorf("extension UI request method is required")
	}
	if now.IsZero() {
		now = time.Now()
	}
	pending := pendingExtensionUI{
		RequestID: request.ID,
		Method:    request.Method,
		Options:   append([]string(nil), request.Options...),
		ExpiresAt: extensionUIExpiry(request, now),
	}
	for {
		token, err := randomExtensionUIToken()
		if err != nil {
			return "", pendingExtensionUI{}, err
		}
		h.mu.Lock()
		h.cleanupExpiredExtensionUILocked(now)
		if _, exists := h.uiTokens[token]; !exists {
			h.uiTokens[token] = pending
			h.mu.Unlock()
			return token, pending, nil
		}
		h.mu.Unlock()
	}
}

func (h *Handler) resolvePendingExtensionUI(token string, now time.Time) (pendingExtensionUI, bool, bool) {
	if now.IsZero() {
		now = time.Now()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	pending, ok := h.uiTokens[token]
	if !ok {
		return pendingExtensionUI{}, false, false
	}
	delete(h.uiTokens, token)
	if !pending.ExpiresAt.IsZero() && now.After(pending.ExpiresAt) {
		return pendingExtensionUI{}, false, true
	}
	return pending, true, false
}

func (h *Handler) markExtensionUITokenHandled(token string, now time.Time) {
	if now.IsZero() {
		now = time.Now()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.uiHandled[token] = now.Add(extensionUIDefaultTTL)
}

func (h *Handler) extensionUITokenHandled(token string, now time.Time) bool {
	if now.IsZero() {
		now = time.Now()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	expiresAt, ok := h.uiHandled[token]
	if !ok {
		return false
	}
	if now.After(expiresAt) {
		delete(h.uiHandled, token)
		return false
	}
	return true
}

func (h *Handler) cleanupExpiredExtensionUI(now time.Time) {
	if now.IsZero() {
		now = time.Now()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cleanupExpiredExtensionUILocked(now)
}

func (h *Handler) cleanupExpiredExtensionUILocked(now time.Time) {
	for token, pending := range h.uiTokens {
		if !pending.ExpiresAt.IsZero() && now.After(pending.ExpiresAt) {
			delete(h.uiTokens, token)
		}
	}
	for token, expiresAt := range h.uiHandled {
		if now.After(expiresAt) {
			delete(h.uiHandled, token)
		}
	}
}

func extensionUIExpiry(request extensionUIRequest, now time.Time) time.Time {
	if request.TimeoutMS > 0 {
		return now.Add(time.Duration(request.TimeoutMS)*time.Millisecond + extensionUITimeoutGrace)
	}
	return now.Add(extensionUIDefaultTTL)
}

func randomExtensionUIToken() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate extension UI token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func extensionUIOption(pending pendingExtensionUI, index int) (string, bool) {
	if index < 0 || index >= len(pending.Options) {
		return "", false
	}
	return pending.Options[index], true
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
		"You can also send one photo/screenshot with an optional caption (5 MiB max, image-capable model required).",
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
