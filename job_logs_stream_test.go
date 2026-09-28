package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// A server that keeps answering "unreachable" on the poll route forever must
// never hang the follow loop once the run itself has actually gone terminal
// (e.g. `aq job cancel` landing from another terminal while this one keeps
// streaming). pollRunLogsFrom must ask the run directly and stop as soon as
// it sees a terminal status, independent of what the log source ever says.
func TestJobLogsFollowStopsWhenRunGoesTerminalDespitePersistentUnreachable(t *testing.T) {
	var pollCount int
	mux := http.NewServeMux()
	mux.HandleFunc("/jobs", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "job-1", "name": "myjob"}})
	})
	mux.HandleFunc("/jobs/job-1/runs/run-1/logs/stream", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("/jobs/job-1/runs/run-1/logs", func(w http.ResponseWriter, r *http.Request) {
		pollCount++
		writeData(w, map[string]any{"chunk": "", "nextOffset": 0, "size": 0, "truncated": false, "source": "unreachable"})
	})
	mux.HandleFunc("/jobs/job-1/runs/run-1", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"id": "run-1", "status": "cancelled", "reason": "cancelled by user"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var out, errOut bytes.Buffer
	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}

	done := make(chan error, 1)
	go func() {
		done <- runJobLogsFollow(jobLogsOptions{
			cred:   cred,
			jobRef: "myjob",
			runID:  "run-1",
			follow: true,
			out:    &out,
			errOut: &errOut,
			sleep:  func(time.Duration) {},
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runJobLogsFollow: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runJobLogsFollow did not return within 5s: it outlived the terminal run")
	}
	if pollCount == 0 {
		t.Fatal("expected the poll fallback to be hit at least once")
	}
	if !strings.Contains(errOut.String(), "can't reach the machine") {
		t.Fatalf("stderr should still warn about unreachable, got: %q", errOut.String())
	}
}

// The end-to-end shape of the bug: `aq job run` must exit non-zero within a
// bounded time when the run it is streaming goes `cancelled` while the log
// endpoint keeps answering `unreachable` forever. Before the fix,
// streamJobRunToCompletion never reached waitForRunTerminal because
// runJobLogsFollow (via pollRunLogsFrom) never returned.
func TestStreamJobRunToCompletionExitsNonZeroWhenLogStaysUnreachablePastCancel(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/jobs", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "job-1", "name": "myjob"}})
	})
	mux.HandleFunc("/jobs/job-1/runs/run-1/logs/stream", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("/jobs/job-1/runs/run-1/logs", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"chunk": "", "nextOffset": 0, "size": 0, "truncated": false, "source": "unreachable"})
	})
	mux.HandleFunc("/jobs/job-1/runs/run-1", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"id": "run-1", "status": "cancelled", "reason": "cancelled by user"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}

	done := make(chan error, 1)
	go func() {
		done <- streamJobRunToCompletion(cred, "job-1", "run-1", io.Discard, io.Discard, func(time.Duration) {}, 0)
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a non-nil error: the run ended cancelled")
		}
		if !strings.Contains(err.Error(), "cancelled") {
			t.Fatalf("error should name the cancelled status, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("streamJobRunToCompletion did not return within 5s: the log follow outlived the terminal run")
	}
}

// box_gone is a normal terminal end state (the batch box was already
// released and never wrote an archived log), not a failure signal by
// itself: the follow must stop on it exactly like archived, and let the
// run's own status (not the log source) decide the exit code.
func TestJobLogsFollowStopsOnBoxGone(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/jobs", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "job-1", "name": "myjob"}})
	})
	mux.HandleFunc("/jobs/job-1/runs/run-1/logs/stream", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("/jobs/job-1/runs/run-1/logs", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"chunk": "", "nextOffset": 0, "size": 0, "truncated": false, "source": "box_gone"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var out, errOut bytes.Buffer
	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}

	done := make(chan error, 1)
	go func() {
		done <- runJobLogsFollow(jobLogsOptions{
			cred:   cred,
			jobRef: "myjob",
			runID:  "run-1",
			follow: true,
			out:    &out,
			errOut: &errOut,
			sleep:  func(time.Duration) {},
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runJobLogsFollow: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runJobLogsFollow did not return within 5s on box_gone")
	}
}
