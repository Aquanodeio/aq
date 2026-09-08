package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Aquanodeio/aq/internal/config"
)

// jobLogsStreamServer wires up the two routes runJobLogsFollow needs before it
// can reach either handler under test: resolveJobID's GET /jobs (so
// "job-1" resolves without a name lookup), and the poll GET
// .../logs the stream falls back to. The stream route itself is the caller's
// to set.
func jobLogsStreamServer(t *testing.T, streamHandler http.HandlerFunc, pollHandler http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/jobs", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "job-1", "name": "myjob"}})
	})
	mux.HandleFunc("/jobs/job-1/runs/run-1/logs/stream", streamHandler)
	mux.HandleFunc("/jobs/job-1/runs/run-1/logs", pollHandler)
	return httptest.NewServer(mux)
}

// The stream must print chunks as they arrive and, once it hits the `end`
// frame, hand off to the poll starting from the offset the LAST chunk
// reported, never from 0 and never from a locally-recomputed offset+len(chunk).
func TestJobLogsStreamParsesFramesAndAdvancesOffset(t *testing.T) {
	srv := jobLogsStreamServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: chunk\ndata: {\"chunk\":\"hello \",\"nextOffset\":6,\"size\":6,\"truncated\":false}\n\n")
			fmt.Fprint(w, "event: chunk\ndata: {\"chunk\":\"world\\n\",\"nextOffset\":12,\"size\":12,\"truncated\":false}\n\n")
			fmt.Fprint(w, "event: heartbeat\ndata: {\"nextOffset\":12}\n\n")
			fmt.Fprint(w, "event: end\ndata: {\"reason\":\"exited\",\"nextOffset\":12}\n\n")
		},
		func(w http.ResponseWriter, r *http.Request) {
			if got := r.URL.Query().Get("offset"); got != "12" {
				t.Errorf("poll fallback used offset %q, want the stream's last nextOffset (12)", got)
			}
			writeData(w, map[string]any{"chunk": "", "nextOffset": 12, "size": 12, "truncated": false, "source": "archived"})
		},
	)
	defer srv.Close()

	var out, errOut bytes.Buffer
	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	err := runJobLogsFollow(jobLogsOptions{
		cred:   cred,
		jobRef: "myjob",
		runID:  "run-1",
		follow: true,
		out:    &out,
		errOut: &errOut,
	})
	if err != nil {
		t.Fatalf("runJobLogsFollow: %v", err)
	}
	if got := out.String(); got != "hello world\n" {
		t.Fatalf("stdout = %q, want the two chunks in order with no gap or duplication", got)
	}
}

// A non-200 on the stream (the SSE endpoint erroring or not existing on an
// older server) must never surface as a bare failure: it falls back to the
// poll from offset 0, since nothing was ever streamed.
func TestJobLogsStreamNon200FallsBackToPoll(t *testing.T) {
	polled := false
	srv := jobLogsStreamServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, "boom")
		},
		func(w http.ResponseWriter, r *http.Request) {
			polled = true
			if got := r.URL.Query().Get("offset"); got != "0" {
				t.Errorf("poll fallback used offset %q, want 0 (the stream never produced a chunk)", got)
			}
			writeData(w, map[string]any{"chunk": "hi\n", "nextOffset": 3, "size": 3, "truncated": false, "source": "archived"})
		},
	)
	defer srv.Close()

	var out, errOut bytes.Buffer
	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	err := runJobLogsFollow(jobLogsOptions{
		cred:   cred,
		jobRef: "myjob",
		runID:  "run-1",
		follow: true,
		out:    &out,
		errOut: &errOut,
	})
	if err != nil {
		t.Fatalf("runJobLogsFollow: %v", err)
	}
	if !polled {
		t.Fatal("poll fallback was never called after the stream returned a non-200")
	}
	if out.String() != "hi\n" {
		t.Fatalf("stdout = %q, want the poll fallback's chunk", out.String())
	}
	if !strings.Contains(errOut.String(), "falling back to polling") {
		t.Fatalf("stderr should say the tail fell back to polling, got: %q", errOut.String())
	}
}
