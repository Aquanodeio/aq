package main

import (
	"os"
	"testing"
)

// Piped input carries one line per field. Every prompt must get its own line:
// a reader per prompt used to swallow the rest of the pipe, so the second field
// of a two-field provider read EOF.
func TestPromptFromTerminal_PipedFieldsEachGetTheirLine(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("client-id-value\nclient-secret-value\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin, oldReader := os.Stdin, stdinReader
	os.Stdin, stdinReader = r, nil
	t.Cleanup(func() { os.Stdin, stdinReader = oldStdin, oldReader; r.Close() })

	first, err := promptFromTerminal("Client ID", false)
	if err != nil || first != "client-id-value" {
		t.Fatalf("first field = %q, %v", first, err)
	}
	second, err := promptFromTerminal("Client secret", true)
	if err != nil || second != "client-secret-value" {
		t.Fatalf("second field = %q, %v (want the second piped line, not EOF)", second, err)
	}
}
