package telegram

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"

	"piontg/pi"
)

const DefaultMaxImageBytes = 5 * 1024 * 1024

type TelegramFileURLer interface {
	GetFileDirectURL(fileID string) (string, error)
}

type TelegramImageFetcher struct {
	bot      TelegramFileURLer
	client   *http.Client
	maxBytes int64
}

func NewTelegramImageFetcher(bot TelegramFileURLer) *TelegramImageFetcher {
	return &TelegramImageFetcher{bot: bot, client: newTelegramHTTPClient(), maxBytes: DefaultMaxImageBytes}
}

func NewTelegramImageFetcherWithClient(bot TelegramFileURLer, client *http.Client, maxBytes int64) *TelegramImageFetcher {
	return &TelegramImageFetcher{bot: bot, client: client, maxBytes: maxBytes}
}

func (f *TelegramImageFetcher) FetchImage(ctx context.Context, ref ImageRef) (pi.ImageContent, error) {
	if ref.FileID == "" {
		return pi.ImageContent{}, fmt.Errorf("telegram image file ID is required")
	}
	maxBytes := f.maxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxImageBytes
	}
	if ref.Size > maxBytes {
		return pi.ImageContent{}, fmt.Errorf("telegram image is too large: %d bytes exceeds %d byte limit", ref.Size, maxBytes)
	}
	if f.bot == nil {
		return pi.ImageContent{}, fmt.Errorf("telegram image fetcher is not configured")
	}
	url, err := f.bot.GetFileDirectURL(ref.FileID)
	if err != nil {
		return pi.ImageContent{}, fmt.Errorf("get telegram image URL: %w", err)
	}
	if url == "" {
		return pi.ImageContent{}, fmt.Errorf("get telegram image URL: empty URL")
	}
	client := f.client
	if client == nil {
		client = newTelegramHTTPClient()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return pi.ImageContent{}, fmt.Errorf("create telegram image request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return pi.ImageContent{}, fmt.Errorf("download telegram image: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return pi.ImageContent{}, fmt.Errorf("download telegram image: HTTP status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return pi.ImageContent{}, fmt.Errorf("read telegram image: %w", err)
	}
	if int64(len(body)) > maxBytes {
		return pi.ImageContent{}, fmt.Errorf("telegram image is too large: exceeds %d byte limit", maxBytes)
	}
	return pi.ImageContent{Type: pi.ImageContentTypeImage, MimeType: "image/jpeg", Data: base64.StdEncoding.EncodeToString(body)}, nil
}
