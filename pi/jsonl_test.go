package pi

import (
	"strings"
	"testing"
)

func TestReadJSONLLinesSplitsOnlyOnLF(t *testing.T) {
	input := "{\"type\":\"message_update\",\"text\":\"before after\"}\n{\"type\":\"agent_end\"}\n"
	var lines []string
	err := readJSONLLines(strings.NewReader(input), func(line []byte) error {
		lines = append(lines, string(line))
		return nil
	})
	if err != nil {
		t.Fatalf("readJSONLLines() error = %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("lines = %#v", lines)
	}
	if !strings.Contains(lines[0], "before after") {
		t.Fatalf("unicode separator was not preserved: %q", lines[0])
	}
}

func TestReadJSONLLinesTrimsCRLFAndFinalLine(t *testing.T) {
	input := "{\"type\":\"one\"}\r\n{\"type\":\"two\"}"
	var lines []string
	err := readJSONLLines(strings.NewReader(input), func(line []byte) error {
		lines = append(lines, string(line))
		return nil
	})
	if err != nil {
		t.Fatalf("readJSONLLines() error = %v", err)
	}
	want := []string{"{\"type\":\"one\"}", "{\"type\":\"two\"}"}
	if len(lines) != len(want) {
		t.Fatalf("lines = %#v", lines)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("lines[%d] = %q, want %q", i, lines[i], want[i])
		}
	}
}
