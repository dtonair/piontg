package pi

import (
	"context"
	"reflect"
	"testing"
)

func TestBuildArgs(t *testing.T) {
	args := buildArgs(Options{
		SessionDir:   "/sessions",
		SessionFile:  "/sessions/session.jsonl",
		Model:        "anthropic/claude-sonnet",
		Trust:        "no-approve",
		Tools:        []string{"read", "grep"},
		ExcludeTools: []string{"bash"},
		ExtraArgs:    []string{"--name", "telegram"},
	})
	want := []string{
		"--mode", "rpc",
		"--session-dir", "/sessions",
		"--session", "/sessions/session.jsonl",
		"--model", "anthropic/claude-sonnet",
		"--no-approve",
		"--tools", "read,grep",
		"--exclude-tools", "bash",
		"--name", "telegram",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
}

func TestBuildArgsApprove(t *testing.T) {
	args := buildArgs(Options{Trust: "approve"})
	want := []string{"--mode", "rpc", "--approve"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
}

func TestNewCommandRequiresCWD(t *testing.T) {
	_, err := newCommand(context.Background(), Options{})
	if err == nil {
		t.Fatal("newCommand() error = nil")
	}
}
