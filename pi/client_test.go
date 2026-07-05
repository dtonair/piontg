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

func TestPromptWritesTextOnlyPayloadWithoutImages(t *testing.T) {
	client, writer := testClient()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- client.Prompt(ctx, "msg") }()

	sent := readSentCommand(t, ctx, writer)
	if sent["type"] != "prompt" || sent["message"] != "msg" {
		t.Fatalf("sent command = %#v", sent)
	}
	if _, ok := sent["images"]; ok {
		t.Fatalf("text-only prompt included images: %#v", sent)
	}
	respondOK(t, client, sent, "prompt")
	if err := <-errCh; err != nil {
		t.Fatalf("Prompt() error = %v", err)
	}
}

func TestPromptWritesImagePayload(t *testing.T) {
	client, writer := testClient()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	image := ImageContent{Type: ImageContentTypeImage, Data: "YWJj", MimeType: "image/jpeg"}
	errCh := make(chan error, 1)
	go func() { errCh <- client.Prompt(ctx, "msg", image) }()

	sent := readSentCommand(t, ctx, writer)
	if sent["type"] != "prompt" || sent["message"] != "msg" {
		t.Fatalf("sent command = %#v", sent)
	}
	images, ok := sent["images"].([]any)
	if !ok || len(images) != 1 {
		t.Fatalf("images = %#v", sent["images"])
	}
	got, ok := images[0].(map[string]any)
	if !ok || got["type"] != "image" || got["data"] != "YWJj" || got["mimeType"] != "image/jpeg" {
		t.Fatalf("image payload = %#v", images[0])
	}
	respondOK(t, client, sent, "prompt")
	if err := <-errCh; err != nil {
		t.Fatalf("Prompt() error = %v", err)
	}
}

func TestSteerAndFollowUpWriteImagePayload(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(context.Context, *Client, ImageContent) error
		want string
	}{
		{name: "steer", call: func(ctx context.Context, c *Client, image ImageContent) error { return c.Steer(ctx, "msg", image) }, want: "steer"},
		{name: "follow_up", call: func(ctx context.Context, c *Client, image ImageContent) error { return c.FollowUp(ctx, "msg", image) }, want: "follow_up"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, writer := testClient()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			errCh := make(chan error, 1)
			go func() {
				errCh <- tc.call(ctx, client, ImageContent{Type: ImageContentTypeImage, Data: "ZGF0YQ==", MimeType: "image/png"})
			}()
			sent := readSentCommand(t, ctx, writer)
			if sent["type"] != tc.want || sent["message"] != "msg" {
				t.Fatalf("sent command = %#v", sent)
			}
			images, ok := sent["images"].([]any)
			if !ok || len(images) != 1 {
				t.Fatalf("images = %#v", sent["images"])
			}
			got := images[0].(map[string]any)
			if got["type"] != "image" || got["data"] != "ZGF0YQ==" || got["mimeType"] != "image/png" {
				t.Fatalf("image payload = %#v", got)
			}
			respondOK(t, client, sent, tc.want)
			if err := <-errCh; err != nil {
				t.Fatalf("%s() error = %v", tc.name, err)
			}
		})
	}
}

func readSentCommand(t *testing.T, ctx context.Context, writer *captureWriteCloser) map[string]any {
	t.Helper()
	var sent map[string]any
	select {
	case line := <-writer.writes:
		if err := json.Unmarshal(line, &sent); err != nil {
			t.Fatalf("sent json: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for command write")
	}
	return sent
}

func respondOK(t *testing.T, client *Client, sent map[string]any, command string) {
	t.Helper()
	line := `{"id":"` + sent["id"].(string) + `","type":"response","command":"` + command + `","success":true}`
	if err := client.handleLine([]byte(line)); err != nil {
		t.Fatalf("handleLine() error = %v", err)
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

func TestHandleLineDoesNotDropCriticalEventsWhenQueueFull(t *testing.T) {
	client, _ := testClient()
	for i := 0; i < cap(client.events); i++ {
		client.events <- Event{Type: "queued"}
	}

	done := make(chan error, 1)
	go func() {
		done <- client.handleLine([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"x"}}`))
	}()

	select {
	case err := <-done:
		t.Fatalf("critical event was not backpressured, err=%v", err)
	case <-time.After(20 * time.Millisecond):
	}

	<-client.events
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("handleLine() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for critical event delivery")
	}

	found := false
	for len(client.events) > 0 {
		if (<-client.events).Type == "message_update" {
			found = true
		}
	}
	if !found {
		t.Fatal("critical message_update was not delivered")
	}
}

func TestHandleLineDropsNonCriticalEventsWhenQueueFull(t *testing.T) {
	client, _ := testClient()
	for i := 0; i < cap(client.events); i++ {
		client.events <- Event{Type: "queued"}
	}
	if err := client.handleLine([]byte(`{"type":"tool_execution_start","toolName":"bash"}`)); err != nil {
		t.Fatalf("handleLine() error = %v", err)
	}
	for len(client.events) > 0 {
		if event := <-client.events; event.Type == "tool_execution_start" {
			t.Fatal("non-critical event should have been dropped when queue is full")
		}
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

func TestGetCommandsDecodeData(t *testing.T) {
	client, writer := testClient()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	commandsCh := make(chan []CommandInfo, 1)
	errCh := make(chan error, 1)
	go func() {
		commands, err := client.GetCommands(ctx)
		commandsCh <- commands
		errCh <- err
	}()
	var sent map[string]any
	if err := json.Unmarshal(<-writer.writes, &sent); err != nil {
		t.Fatal(err)
	}
	if sent["type"] != "get_commands" {
		t.Fatalf("sent command = %#v", sent)
	}
	resp := `{"id":"` + sent["id"].(string) + `","type":"response","command":"get_commands","success":true,"data":{"commands":[{"name":"skill:asana-cli","description":"Use Asana","source":"skill","location":"user","path":"/home/user/.agents/skills/asana-cli/SKILL.md"},{"name":"fix-tests","description":"Fix failing tests","source":"prompt","location":"project","path":"/repo/.pi/prompts/fix-tests.md"}]}}`
	if err := client.handleLine([]byte(resp)); err != nil {
		t.Fatal(err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("GetCommands() error = %v", err)
	}
	commands := <-commandsCh
	if len(commands) != 2 {
		t.Fatalf("commands = %#v", commands)
	}
	if commands[0].Name != "skill:asana-cli" || commands[0].Description != "Use Asana" || commands[0].Source != "skill" || commands[0].Location != "user" || commands[0].Path == "" {
		t.Fatalf("first command = %#v", commands[0])
	}
	if commands[1].Name != "fix-tests" || commands[1].Source != "prompt" || commands[1].Location != "project" {
		t.Fatalf("second command = %#v", commands[1])
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

	requestCtx, requestCancel := context.WithTimeout(context.Background(), 5*time.Second)
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
