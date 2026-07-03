package pi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type captureWriteCloser struct {
	writes chan []byte
	closed chan struct{}
}

func newCaptureWriteCloser() *captureWriteCloser {
	return &captureWriteCloser{writes: make(chan []byte, 10), closed: make(chan struct{})}
}

func (w *captureWriteCloser) Write(p []byte) (int, error) {
	copy := append([]byte(nil), p...)
	w.writes <- copy
	return len(p), nil
}

func (w *captureWriteCloser) Close() error {
	select {
	case <-w.closed:
	default:
		close(w.closed)
	}
	return nil
}

func testClient() (*Client, *captureWriteCloser) {
	writer := newCaptureWriteCloser()
	client := NewClient(Options{})
	client.stdin = writer
	return client, writer
}

func TestRequestWritesCommandAndCorrelatesResponse(t *testing.T) {
	client, writer := testClient()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	resultCh := make(chan Response, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, err := client.Request(ctx, "get_state", nil)
		resultCh <- resp
		errCh <- err
	}()

	var sent map[string]any
	select {
	case line := <-writer.writes:
		if err := json.Unmarshal(line, &sent); err != nil {
			t.Fatalf("sent json: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for command write")
	}
	if sent["type"] != "get_state" || sent["id"] == "" {
		t.Fatalf("sent command = %#v", sent)
	}

	line := `{"id":"` + sent["id"].(string) + `","type":"response","command":"get_state","success":true,"data":{"sessionId":"abc","isStreaming":false}}`
	if err := client.handleLine([]byte(line)); err != nil {
		t.Fatalf("handleLine() error = %v", err)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("Request() error = %v", err)
	}
	resp := <-resultCh
	if resp.Command != "get_state" || !resp.Success {
		t.Fatalf("response = %#v", resp)
	}
}

func TestRequestReturnsFailedResponseAsError(t *testing.T) {
	client, writer := testClient()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		_, err := client.Request(ctx, "set_model", map[string]any{"provider": "bad"})
		errCh <- err
	}()
	var sent map[string]any
	line := <-writer.writes
	if err := json.Unmarshal(line, &sent); err != nil {
		t.Fatal(err)
	}
	resp := `{"id":"` + sent["id"].(string) + `","type":"response","command":"set_model","success":false,"error":"Model not found"}`
	if err := client.handleLine([]byte(resp)); err != nil {
		t.Fatal(err)
	}
	if err := <-errCh; err == nil || !strings.Contains(err.Error(), "Model not found") {
		t.Fatalf("Request() error = %v", err)
	}
}

func TestRespondExtensionUIWritesFireAndForgetResponse(t *testing.T) {
	client, writer := testClient()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.RespondExtensionUI(ctx, "ui-1", map[string]any{"cancelled": true}); err != nil {
		t.Fatalf("RespondExtensionUI() error = %v", err)
	}
	var sent map[string]any
	select {
	case line := <-writer.writes:
		if err := json.Unmarshal(line, &sent); err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for write")
	}
	if sent["type"] != "extension_ui_response" || sent["id"] != "ui-1" || sent["cancelled"] != true {
		t.Fatalf("sent = %#v", sent)
	}
}

func TestRequestTimesOutAndRemovesPending(t *testing.T) {
	client, _ := testClient()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	_, err := client.Request(ctx, "get_state", nil)
	if err == nil {
		t.Fatal("Request() error = nil")
	}
	client.mu.Lock()
	pending := len(client.pending)
	client.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending = %d", pending)
	}
}

func TestHandleLineRoutesEvents(t *testing.T) {
	client, _ := testClient()
	if err := client.handleLine([]byte(`{"type":"agent_start"}`)); err != nil {
		t.Fatalf("handleLine() error = %v", err)
	}
	select {
	case event := <-client.Events():
		if event.Type != "agent_start" {
			t.Fatalf("event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestTypedMethodsDecodeData(t *testing.T) {
	client, writer := testClient()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	modelsCh := make(chan []ModelInfo, 1)
	errCh := make(chan error, 1)
	go func() {
		models, err := client.GetAvailableModels(ctx)
		modelsCh <- models
		errCh <- err
	}()
	var sent map[string]any
	if err := json.Unmarshal(<-writer.writes, &sent); err != nil {
		t.Fatal(err)
	}
	resp := `{"id":"` + sent["id"].(string) + `","type":"response","command":"get_available_models","success":true,"data":{"models":[{"provider":"anthropic","id":"claude","reasoning":true,"contextWindow":200000}]}}`
	if err := client.handleLine([]byte(resp)); err != nil {
		t.Fatal(err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("GetAvailableModels() error = %v", err)
	}
	models := <-modelsCh
	if len(models) != 1 || models[0].Provider != "anthropic" || models[0].ID != "claude" {
		t.Fatalf("models = %#v", models)
	}
}

func TestStartedProcessOutlivesStartContextCancellation(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-pi")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
while IFS= read -r line; do
	id=$(printf '%s\n' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
	printf '{"id":"%s","type":"response","command":"get_state","success":true,"data":{"sessionId":"sid","isStreaming":false}}\n' "$id"
done
`), 0o755); err != nil {
		t.Fatal(err)
	}

	client := NewClient(Options{Binary: script, CWD: dir})
	ctx, cancel := context.WithCancel(context.Background())
	if err := client.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	cancel()
	time.Sleep(100 * time.Millisecond)

	requestCtx, requestCancel := context.WithTimeout(context.Background(), time.Second)
	defer requestCancel()
	state, err := client.GetState(requestCtx)
	if err != nil {
		t.Fatalf("GetState() after start context cancellation error = %v", err)
	}
	if state.SessionID != "sid" {
		t.Fatalf("state = %#v", state)
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	if err := client.Stop(stopCtx); err != nil {
		t.Fatalf("Stop() after start context cancellation error = %v", err)
	}
}

func TestFailRejectsPendingRequests(t *testing.T) {
	client, _ := testClient()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		_, err := client.Request(ctx, "get_state", nil)
		errCh <- err
	}()

	deadline := time.After(time.Second)
	for {
		client.mu.Lock()
		pending := len(client.pending)
		client.mu.Unlock()
		if pending == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("pending request not registered")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	client.fail(errors.New("boom"))
	if err := <-errCh; err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Request() error = %v", err)
	}
}

func TestOptionalPiIntegrationGetState(t *testing.T) {
	if os.Getenv("PIONTG_PI_INTEGRATION") != "1" {
		t.Skip("set PIONTG_PI_INTEGRATION=1 to run")
	}
	dir := t.TempDir()
	client := NewClient(Options{Binary: "pi", CWD: dir, Trust: "no-approve", ExtraArgs: []string{"--no-session"}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v; stderr=%s", err, client.StderrTail())
	}
	defer client.Stop(context.Background())
	state, err := client.GetState(ctx)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("GetState() error = %v; stderr=%s state=%#v", err, client.StderrTail(), state)
	}
}
