package api

import (
	"context"
	"testing"
)

// TestNewRunLogsStreamRequestSendsOffsetNotFrom is the #1007 stream-connect
// blind spot: the orchestrator's stream route shares resolveRunLogsTarget
// with the poll route, which reads req.query.offset. NewRunLogsStreamRequest
// used to send "from" instead (only ogre's own direct endpoint uses that
// name, wire contract section 1), so a resume at a non-zero offset silently
// asked the orchestrator for offset 0 -- dormant only because the CLI's one
// caller always starts the stream at offset 0 today. The existing suite
// (job_logs_stream_test.go) only ever asserts the POLL fallback's query
// param, never the stream connect's, which is exactly why this shipped
// unnoticed.
func TestNewRunLogsStreamRequestSendsOffsetNotFrom(t *testing.T) {
	client := NewAuthed("http://example.invalid", "tok", "team-1")

	req, err := client.NewRunLogsStreamRequest(context.Background(), "job-1", "run-1", 812, 0)
	if err != nil {
		t.Fatalf("NewRunLogsStreamRequest: %v", err)
	}

	q := req.URL.Query()
	if got := q.Get("offset"); got != "812" {
		t.Errorf(`stream request URL.Query().Get("offset") = %q, want "812"`, got)
	}
	if got := q.Get("from"); got != "" {
		t.Errorf(`stream request still sends "from"=%q; the orchestrator's stream route reads "offset", not "from"`, got)
	}
}

// TestNewRunLogsStreamRequestOmitsAttemptWhenZero mirrors GetRunLogs's own
// "0 means latest" handling (attempt=0 must not appear on the wire) so the
// stream connect and the poll stay symmetric on every param but the path.
func TestNewRunLogsStreamRequestOmitsAttemptWhenZero(t *testing.T) {
	client := NewAuthed("http://example.invalid", "tok", "team-1")

	req, err := client.NewRunLogsStreamRequest(context.Background(), "job-1", "run-1", 0, 0)
	if err != nil {
		t.Fatalf("NewRunLogsStreamRequest: %v", err)
	}

	if got := req.URL.Query().Get("attempt"); got != "" {
		t.Errorf(`attempt=0 should be omitted from the stream request, got %q`, got)
	}
}
