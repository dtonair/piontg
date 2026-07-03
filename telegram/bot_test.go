package telegram

import (
	"errors"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type recordingBot struct {
	sends      []tgbotapi.MessageConfig
	requests   []tgbotapi.EditMessageTextConfig
	sendErrs   []error
	requestErr []error
}

func (b *recordingBot) Send(c tgbotapi.Chattable) (tgbotapi.Message, error) {
	msg, ok := c.(tgbotapi.MessageConfig)
	if !ok {
		return tgbotapi.Message{}, errors.New("unexpected send config")
	}
	b.sends = append(b.sends, msg)
	if len(b.sendErrs) > 0 {
		err := b.sendErrs[0]
		b.sendErrs = b.sendErrs[1:]
		if err != nil {
			return tgbotapi.Message{}, err
		}
	}
	return tgbotapi.Message{MessageID: len(b.sends)}, nil
}

func (b *recordingBot) Request(c tgbotapi.Chattable) (*tgbotapi.APIResponse, error) {
	edit, ok := c.(tgbotapi.EditMessageTextConfig)
	if !ok {
		return nil, errors.New("unexpected request config")
	}
	b.requests = append(b.requests, edit)
	if len(b.requestErr) > 0 {
		err := b.requestErr[0]
		b.requestErr = b.requestErr[1:]
		if err != nil {
			return nil, err
		}
	}
	return &tgbotapi.APIResponse{Ok: true}, nil
}

func (b *recordingBot) GetUpdatesChan(tgbotapi.UpdateConfig) tgbotapi.UpdatesChannel {
	return nil
}

func (b *recordingBot) StopReceivingUpdates() {}

func TestMessengerAdapterSendsMarkdownV2(t *testing.T) {
	bot := &recordingBot{}
	messenger := NewMessengerAdapter(bot)
	if _, err := messenger.SendMessage(nil, 123, "Repo **piontg**", nil); err != nil {
		t.Fatal(err)
	}
	if len(bot.sends) != 1 {
		t.Fatalf("sends = %#v", bot.sends)
	}
	if bot.sends[0].ParseMode != telegramMarkdownParseMode {
		t.Fatalf("ParseMode = %q", bot.sends[0].ParseMode)
	}
	if bot.sends[0].Text != "Repo *piontg*" {
		t.Fatalf("Text = %q", bot.sends[0].Text)
	}
}

func TestMessengerAdapterEditsMarkdownV2(t *testing.T) {
	bot := &recordingBot{}
	messenger := NewMessengerAdapter(bot)
	if err := messenger.EditMessage(nil, 123, 456, "Use `pi --mode rpc`."); err != nil {
		t.Fatal(err)
	}
	if len(bot.requests) != 1 {
		t.Fatalf("requests = %#v", bot.requests)
	}
	if bot.requests[0].ParseMode != telegramMarkdownParseMode {
		t.Fatalf("ParseMode = %q", bot.requests[0].ParseMode)
	}
	if bot.requests[0].Text != "Use `pi --mode rpc`\\." {
		t.Fatalf("Text = %q", bot.requests[0].Text)
	}
}

func TestMessengerAdapterFallsBackToPlainTextOnParseError(t *testing.T) {
	bot := &recordingBot{sendErrs: []error{errors.New("Bad Request: can't parse entities")}}
	messenger := NewMessengerAdapter(bot)
	if _, err := messenger.SendMessage(nil, 123, "Repo **piontg**", nil); err != nil {
		t.Fatal(err)
	}
	if len(bot.sends) != 2 {
		t.Fatalf("sends = %#v", bot.sends)
	}
	if bot.sends[1].ParseMode != "" || bot.sends[1].Text != "Repo **piontg**" {
		t.Fatalf("fallback send = %#v", bot.sends[1])
	}
}
