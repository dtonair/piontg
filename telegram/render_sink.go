package telegram

import "context"

// RenderSink adapts the render package to Telegram chat messages.
type RenderSink struct {
	Messenger Messenger
	ChatID    int64
}

func (s *RenderSink) SendMessage(ctx context.Context, text string) (int, error) {
	return s.Messenger.SendMessage(ctx, s.ChatID, text, nil)
}

func (s *RenderSink) EditMessage(ctx context.Context, messageID int, text string) error {
	return s.Messenger.EditMessage(ctx, s.ChatID, messageID, text)
}
