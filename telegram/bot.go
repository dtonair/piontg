package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type BotAPI interface {
	Send(c tgbotapi.Chattable) (tgbotapi.Message, error)
	Request(c tgbotapi.Chattable) (*tgbotapi.APIResponse, error)
	GetUpdatesChan(config tgbotapi.UpdateConfig) tgbotapi.UpdatesChannel
	StopReceivingUpdates()
}

type MessengerAdapter struct {
	bot BotAPI
}

func NewMessengerAdapter(bot BotAPI) *MessengerAdapter {
	return &MessengerAdapter{bot: bot}
}

func (m *MessengerAdapter) SendMessage(_ context.Context, chatID int64, text string, keyboard InlineKeyboard) (int, error) {
	msg := tgbotapi.NewMessage(chatID, telegramMarkdown(text))
	msg.ParseMode = telegramMarkdownParseMode
	if len(keyboard) > 0 {
		msg.ReplyMarkup = toTelegramKeyboard(keyboard)
	}
	sent, err := m.bot.Send(msg)
	if err != nil {
		if !isTelegramParseError(err) {
			return 0, err
		}
		msg.Text = text
		msg.ParseMode = ""
		sent, err = m.bot.Send(msg)
		if err != nil {
			return 0, err
		}
	}
	return sent.MessageID, nil
}

func (m *MessengerAdapter) EditMessage(_ context.Context, chatID int64, messageID int, text string) error {
	edit := tgbotapi.NewEditMessageText(chatID, messageID, telegramMarkdown(text))
	edit.ParseMode = telegramMarkdownParseMode
	_, err := m.bot.Request(edit)
	if err != nil && isTelegramParseError(err) {
		edit.Text = text
		edit.ParseMode = ""
		_, err = m.bot.Request(edit)
	}
	return err
}

func (m *MessengerAdapter) AnswerCallback(_ context.Context, callbackID, text string) error {
	callback := tgbotapi.NewCallback(callbackID, text)
	_, err := m.bot.Request(callback)
	return err
}

func RunPolling(ctx context.Context, bot BotAPI, handler *Handler, allowedUserID int64, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	updates := bot.GetUpdatesChan(tgbotapi.NewUpdate(0))
	defer bot.StopReceivingUpdates()
	handler.StartEventRendering(ctx, allowedUserID)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case update, ok := <-updates:
			if !ok {
				return nil
			}
			converted, ok := convertUpdate(update)
			if !ok {
				continue
			}
			requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := handler.HandleUpdate(requestCtx, converted)
			cancel()
			if err != nil {
				logger.Warn("handle telegram update", "error", err)
			}
		}
	}
}

func NewRealBot(token string) (*tgbotapi.BotAPI, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("telegram token is required")
	}
	return tgbotapi.NewBotAPI(token)
}

func convertUpdate(update tgbotapi.Update) (Update, bool) {
	if update.Message != nil && update.Message.From != nil {
		return Update{Message: &Message{ChatID: update.Message.Chat.ID, UserID: update.Message.From.ID, Text: update.Message.Text}}, true
	}
	if update.CallbackQuery != nil && update.CallbackQuery.From != nil && update.CallbackQuery.Message != nil {
		return Update{Callback: &Callback{
			ID:        update.CallbackQuery.ID,
			ChatID:    update.CallbackQuery.Message.Chat.ID,
			UserID:    update.CallbackQuery.From.ID,
			MessageID: update.CallbackQuery.Message.MessageID,
			Data:      update.CallbackQuery.Data,
		}}, true
	}
	return Update{}, false
}

func toTelegramKeyboard(keyboard InlineKeyboard) tgbotapi.InlineKeyboardMarkup {
	rows := make([][]tgbotapi.InlineKeyboardButton, 0, len(keyboard))
	for _, row := range keyboard {
		buttons := make([]tgbotapi.InlineKeyboardButton, 0, len(row))
		for _, button := range row {
			buttons = append(buttons, tgbotapi.NewInlineKeyboardButtonData(button.Text, button.Data))
		}
		rows = append(rows, buttons)
	}
	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}
