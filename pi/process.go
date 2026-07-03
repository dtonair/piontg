package pi

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Options controls the Pi RPC subprocess.
type Options struct {
	Binary       string
	CWD          string
	SessionDir   string
	SessionFile  string
	Model        string
	Trust        string // "approve" or "no-approve"
	Tools        []string
	ExcludeTools []string
	ExtraArgs    []string
}

func buildArgs(opts Options) []string {
	args := []string{"--mode", "rpc"}
	if opts.SessionDir != "" {
		args = append(args, "--session-dir", opts.SessionDir)
	}
	if opts.SessionFile != "" {
		args = append(args, "--session", opts.SessionFile)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	switch opts.Trust {
	case "approve":
		args = append(args, "--approve")
	case "no-approve":
		args = append(args, "--no-approve")
	}
	if len(opts.Tools) > 0 {
		args = append(args, "--tools", strings.Join(opts.Tools, ","))
	}
	if len(opts.ExcludeTools) > 0 {
		args = append(args, "--exclude-tools", strings.Join(opts.ExcludeTools, ","))
	}
	args = append(args, opts.ExtraArgs...)
	return args
}

func newCommand(ctx context.Context, opts Options) (*exec.Cmd, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	binary := opts.Binary
	if binary == "" {
		binary = "pi"
	}
	if opts.CWD == "" {
		return nil, fmt.Errorf("cwd is required")
	}
	// The Pi RPC process is long-lived and outlives individual Telegram update
	// handlers. Do not bind it to the handler context, which is cancelled as soon
	// as the update is processed.
	cmd := exec.Command(binary, buildArgs(opts)...)
	cmd.Dir = opts.CWD
	return cmd, nil
}
