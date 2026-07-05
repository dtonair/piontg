package telegram

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"piontg/pi"
)

type fakeFileURLer struct {
	url   string
	err   error
	calls []string
}

func (f *fakeFileURLer) GetFileDirectURL(fileID string) (string, error) {
	f.calls = append(f.calls, fileID)
	return f.url, f.err
}

func TestTelegramImageFetcherRejectsDeclaredOversizeBeforeFetch(t *testing.T) {
	bot := &fakeFileURLer{url: "http://example.invalid/file"}
	fetcher := NewTelegramImageFetcherWithClient(bot, http.DefaultClient, 10)
	_, err := fetcher.FetchImage(context.Background(), ImageRef{FileID: "file", Size: 11})
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("FetchImage() error = %v", err)
	}
	if len(bot.calls) != 0 {
		t.Fatalf("GetFileDirectURL was called: %#v", bot.calls)
	}
}

func TestTelegramImageFetcherCapsResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("12345678901"))
	}))
	defer server.Close()
	bot := &fakeFileURLer{url: server.URL}
	fetcher := NewTelegramImageFetcherWithClient(bot, server.Client(), 10)
	_, err := fetcher.FetchImage(context.Background(), ImageRef{FileID: "file"})
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("FetchImage() error = %v", err)
	}
}

func TestTelegramImageFetcherReturnsPiImageContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("image bytes"))
	}))
	defer server.Close()
	bot := &fakeFileURLer{url: server.URL}
	fetcher := NewTelegramImageFetcherWithClient(bot, server.Client(), DefaultMaxImageBytes)
	got, err := fetcher.FetchImage(context.Background(), ImageRef{FileID: "file", Size: 10})
	if err != nil {
		t.Fatalf("FetchImage() error = %v", err)
	}
	want := pi.ImageContent{Type: pi.ImageContentTypeImage, MimeType: "image/jpeg", Data: base64.StdEncoding.EncodeToString([]byte("image bytes"))}
	if got != want {
		t.Fatalf("FetchImage() = %#v, want %#v", got, want)
	}
	if len(bot.calls) != 1 || bot.calls[0] != "file" {
		t.Fatalf("GetFileDirectURL calls = %#v", bot.calls)
	}
}

func TestTelegramImageFetcherSurfacesURLError(t *testing.T) {
	fetcher := NewTelegramImageFetcherWithClient(&fakeFileURLer{err: errors.New("api failed")}, http.DefaultClient, DefaultMaxImageBytes)
	_, err := fetcher.FetchImage(context.Background(), ImageRef{FileID: "file"})
	if err == nil || !strings.Contains(err.Error(), "get telegram image URL") || !strings.Contains(err.Error(), "api failed") {
		t.Fatalf("FetchImage() error = %v", err)
	}
}

func TestTelegramImageFetcherSurfacesHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer server.Close()
	fetcher := NewTelegramImageFetcherWithClient(&fakeFileURLer{url: server.URL}, server.Client(), DefaultMaxImageBytes)
	_, err := fetcher.FetchImage(context.Background(), ImageRef{FileID: "file"})
	if err == nil || !strings.Contains(err.Error(), "HTTP status 404") {
		t.Fatalf("FetchImage() error = %v", err)
	}
}
