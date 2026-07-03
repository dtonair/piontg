package pi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// readJSONLLines reads strict LF-delimited JSONL records. It splits only on
// byte '\n' and trims one optional trailing '\r'. Unicode line separators inside
// JSON strings are preserved because they are not delimiters.
func readJSONLLines(r io.Reader, onLine func([]byte) error) error {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes.TrimSuffix(line, []byte{'\n'})
			line = bytes.TrimSuffix(line, []byte{'\r'})
			if len(bytes.TrimSpace(line)) > 0 {
				if cbErr := onLine(line); cbErr != nil {
					return cbErr
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func decodeRaw(line []byte) (map[string]json.RawMessage, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, fmt.Errorf("decode rpc json line: %w", err)
	}
	return raw, nil
}
