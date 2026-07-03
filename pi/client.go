package pi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const defaultRequestTimeout = 30 * time.Second

type pendingRequest struct {
	command string
	ch      chan Response
}

// Client is a Pi RPC subprocess client.
type Client struct {
	opts Options
	cmd  *exec.Cmd

	stdin io.WriteCloser

	mu      sync.Mutex
	pending map[string]pendingRequest
	closed  bool
	exitErr error

	nextID uint64
	events chan Event
	done   chan error
	stderr *tailBuffer
}

func NewClient(opts Options) *Client {
	return &Client{
		opts:    opts,
		pending: make(map[string]pendingRequest),
		events:  make(chan Event, 128),
		done:    make(chan error, 1),
		stderr:  newTailBuffer(16 * 1024),
	}
}

func (c *Client) Start(ctx context.Context) error {
	cmd, err := newCommand(ctx, c.opts)
	if err != nil {
		return err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open pi stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open pi stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("open pi stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start pi: %w", err)
	}

	c.cmd = cmd
	c.stdin = stdin
	go c.readStdout(stdout)
	go c.readStderr(stderr)
	go c.wait(cmd)
	return nil
}

func (c *Client) Events() <-chan Event { return c.events }

func (c *Client) IsClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *Client) StderrTail() string { return c.stderr.String() }

func (c *Client) Stop(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		err := c.exitErr
		c.mu.Unlock()
		return err
	}
	stdin := c.stdin
	cmd := c.cmd
	c.mu.Unlock()

	if stdin != nil {
		_ = stdin.Close()
	}
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	done := make(chan error, 1)
	go func() { done <- <-c.done }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		return ctx.Err()
	}
}

func (c *Client) GetState(ctx context.Context) (SessionState, error) {
	var out SessionState
	err := c.requestData(ctx, "get_state", nil, &out)
	return out, err
}

func (c *Client) GetAvailableModels(ctx context.Context) ([]ModelInfo, error) {
	var out modelsData
	if err := c.requestData(ctx, "get_available_models", nil, &out); err != nil {
		return nil, err
	}
	return out.Models, nil
}

func (c *Client) GetCommands(ctx context.Context) ([]CommandInfo, error) {
	var out commandsData
	if err := c.requestData(ctx, "get_commands", nil, &out); err != nil {
		return nil, err
	}
	return out.Commands, nil
}

func (c *Client) SetModel(ctx context.Context, provider, modelID string) (ModelInfo, error) {
	var out ModelInfo
	err := c.requestData(ctx, "set_model", map[string]any{"provider": provider, "modelId": modelID}, &out)
	return out, err
}

func (c *Client) Prompt(ctx context.Context, message string) error {
	_, err := c.Request(ctx, "prompt", map[string]any{"message": message})
	return err
}

func (c *Client) FollowUp(ctx context.Context, message string) error {
	_, err := c.Request(ctx, "follow_up", map[string]any{"message": message})
	return err
}

func (c *Client) Steer(ctx context.Context, message string) error {
	_, err := c.Request(ctx, "steer", map[string]any{"message": message})
	return err
}

func (c *Client) Abort(ctx context.Context) error {
	_, err := c.Request(ctx, "abort", nil)
	return err
}

// RespondExtensionUI sends a response for an RPC extension UI dialog request.
// It is fire-and-forget: Pi does not emit a normal command response for this record.
func (c *Client) RespondExtensionUI(ctx context.Context, requestID string, payload map[string]any) error {
	if requestID == "" {
		return errors.New("extension UI request ID is required")
	}
	cmd := make(map[string]any, len(payload)+2)
	cmd["type"] = "extension_ui_response"
	cmd["id"] = requestID
	for k, v := range payload {
		cmd[k] = v
	}
	line, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshal extension UI response: %w", err)
	}
	line = append(line, '\n')

	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		if c.exitErr != nil {
			return c.exitErr
		}
		return errors.New("pi rpc client is closed")
	}
	if c.stdin == nil {
		return errors.New("pi rpc client is not started")
	}
	_, err = c.stdin.Write(line)
	if err != nil {
		return fmt.Errorf("write extension UI response: %w", err)
	}
	return nil
}

func (c *Client) NewSession(ctx context.Context) (bool, error) {
	var out newSessionData
	if err := c.requestData(ctx, "new_session", nil, &out); err != nil {
		return false, err
	}
	return out.Cancelled, nil
}

func (c *Client) GetSessionStats(ctx context.Context) (SessionStats, error) {
	var out SessionStats
	err := c.requestData(ctx, "get_session_stats", nil, &out)
	return out, err
}

func (c *Client) Request(ctx context.Context, command string, payload map[string]any) (Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultRequestTimeout)
		defer cancel()
	}

	id := strconv.FormatUint(atomic.AddUint64(&c.nextID, 1), 10)
	cmd := make(map[string]any, len(payload)+2)
	cmd["id"] = id
	cmd["type"] = command
	for k, v := range payload {
		cmd[k] = v
	}
	line, err := json.Marshal(cmd)
	if err != nil {
		return Response{}, fmt.Errorf("marshal rpc command %q: %w", command, err)
	}
	line = append(line, '\n')

	respCh := make(chan Response, 1)
	c.mu.Lock()
	if c.closed {
		err := c.exitErr
		if err == nil {
			err = errors.New("pi rpc client is closed")
		}
		c.mu.Unlock()
		return Response{}, err
	}
	if c.stdin == nil {
		c.mu.Unlock()
		return Response{}, errors.New("pi rpc client is not started")
	}
	c.pending[id] = pendingRequest{command: command, ch: respCh}
	_, err = c.stdin.Write(line)
	if err != nil {
		delete(c.pending, id)
		c.mu.Unlock()
		return Response{}, fmt.Errorf("write rpc command %q: %w", command, err)
	}
	c.mu.Unlock()

	select {
	case resp := <-respCh:
		if !resp.Success {
			if resp.Error == "" {
				resp.Error = "rpc command failed"
			}
			return resp, fmt.Errorf("pi rpc %s failed: %s", command, resp.Error)
		}
		return resp, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return Response{}, fmt.Errorf("pi rpc %s timeout: %w", command, ctx.Err())
	}
}

func (c *Client) requestData(ctx context.Context, command string, payload map[string]any, out any) error {
	resp, err := c.Request(ctx, command, payload)
	if err != nil {
		return err
	}
	if out == nil || len(resp.Data) == 0 || string(resp.Data) == "null" {
		return nil
	}
	if err := json.Unmarshal(resp.Data, out); err != nil {
		return fmt.Errorf("decode rpc %s response data: %w", command, err)
	}
	if stats, ok := out.(*SessionStats); ok {
		stats.Raw = append(stats.Raw[:0], resp.Data...)
	}
	return nil
}

func (c *Client) readStdout(stdout io.Reader) {
	err := readJSONLLines(stdout, func(line []byte) error {
		return c.handleLine(line)
	})
	if err != nil {
		c.fail(fmt.Errorf("read pi stdout: %w", err))
	}
}

func (c *Client) readStderr(stderr io.Reader) {
	_, _ = io.Copy(c.stderr, stderr)
}

func (c *Client) wait(cmd *exec.Cmd) {
	err := cmd.Wait()
	c.fail(err)
}

func (c *Client) handleLine(line []byte) error {
	raw, err := decodeRaw(line)
	if err != nil {
		return err
	}
	var typ string
	if err := json.Unmarshal(raw["type"], &typ); err != nil {
		return fmt.Errorf("rpc line missing type: %w", err)
	}
	if typ == "response" {
		var resp Response
		if err := json.Unmarshal(line, &resp); err != nil {
			return fmt.Errorf("decode rpc response: %w", err)
		}
		resp.Raw = append(resp.Raw[:0], line...)
		c.deliverResponse(resp)
		return nil
	}
	event := Event{Type: typ, Raw: append([]byte(nil), line...)}
	if isCriticalStreamEvent(typ) {
		c.events <- event
		return nil
	}
	select {
	case c.events <- event:
	default:
		// Do not block stdout reading for non-critical events. Assistant stream
		// text and turn boundary events are delivered above without silent drops.
	}
	return nil
}

func isCriticalStreamEvent(typ string) bool {
	switch typ {
	case "message_update", "agent_start", "agent_end":
		return true
	default:
		return false
	}
}

func (c *Client) deliverResponse(resp Response) {
	c.mu.Lock()
	pending, ok := c.pending[resp.ID]
	if ok {
		delete(c.pending, resp.ID)
	}
	c.mu.Unlock()
	if ok {
		pending.ch <- resp
	}
}

func (c *Client) fail(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.exitErr = err
	pending := c.pending
	c.pending = make(map[string]pendingRequest)
	c.mu.Unlock()

	failure := Response{Type: "response", Success: false}
	if err != nil {
		failure.Error = err.Error()
	} else {
		failure.Error = "pi rpc process exited"
	}
	for _, req := range pending {
		resp := failure
		resp.Command = req.command
		req.ch <- resp
	}
	select {
	case c.done <- err:
	default:
	}
}
