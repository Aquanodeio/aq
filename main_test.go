package main

import "testing"

// TestVersionDefault is a smoke test so `go test ./...` has something to run and
// CI is green from the first commit.
func TestVersionDefault(t *testing.T) {
	if version == "" {
		t.Fatal("version must not be empty")
	}
}
