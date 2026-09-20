package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Aquanodeio/aq/internal/config"
)

// jobPullServer answers the lookups doJobPull needs before it can act:
// job resolution (GET /jobs) and a run's artifacts listing/download, wired
// through the mux the caller supplies so each test can shape exactly the
// fixture it wants -- never mocking the JSON decode itself (the contract at
// risk here is the field names on the wire, not our own struct).
func jobPullServer(t *testing.T, artifacts http.HandlerFunc, download http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/jobs", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "job-1", "name": "myjob"}})
	})
	if artifacts != nil {
		mux.HandleFunc("/jobs/job-1/runs/run-1/artifacts", artifacts)
	}
	if download != nil {
		mux.HandleFunc("/jobs/job-1/runs/run-1/artifacts/download", download)
	}
	return httptest.NewServer(mux)
}

func basePullOpts(serverURL string, dest string) jobPullOptions {
	return jobPullOptions{
		cred:   &config.Credential{Token: "aq_sk_test", TeamID: "team-1", APIURL: serverURL},
		target: "myjob",
		runID:  "run-1",
		dest:   dest,
		out:    &bytes.Buffer{},
		errOut: &bytes.Buffer{},
	}
}

// TestJobPullCouldNotLookNeverReadsAsNoOutputs is the artifacts listing's
// whole reason to be four-state rather than a plain list: "unreachable"
// (mjolnir could not be asked) is a DIFFERENT fact from "ok" with zero
// entries, and collapsing the two would tell an owner "nothing landed" when
// the true answer is "we don't know yet". This pins that doJobPull's error
// text for "unreachable" never contains the phrase an empty-but-ok run gets.
func TestJobPullCouldNotLookNeverReadsAsNoOutputs(t *testing.T) {
	srv := jobPullServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"source": "unreachable", "attemptOrdinal": 3, "artifacts": []any{}, "truncated": false})
	}, nil)
	defer srv.Close()

	dest := t.TempDir()
	err := doJobPull(basePullOpts(srv.URL, dest))
	if err == nil {
		t.Fatal("want an error for an unreachable artifacts listing")
	}
	msg := err.Error()
	if strings.Contains(msg, "no output files") || strings.Contains(msg, "no outputs") {
		t.Fatalf("unreachable must never read as an empty-but-ok run, got: %s", msg)
	}
	if !strings.Contains(msg, "could not reach") {
		t.Fatalf("want the could-not-look wording surfaced, got: %s", msg)
	}
}

// TestJobPullSourceStatesRefuseWithDistinctReasons covers the remaining two
// not-ok states, each of which is a genuinely different fact from the
// other and from "unreachable" above: no_attempt_yet is "never had a box",
// no_box is "this attempt's box was never assigned". Neither is a network
// or server failure, so both are refused locally with wording naming which.
func TestJobPullSourceStatesRefuseWithDistinctReasons(t *testing.T) {
	cases := []struct {
		source string
		want   string
	}{
		{"no_attempt_yet", "never had a box"},
		{"no_box", "no box attached"},
	}
	for _, c := range cases {
		t.Run(c.source, func(t *testing.T) {
			srv := jobPullServer(t, func(w http.ResponseWriter, r *http.Request) {
				writeData(w, map[string]any{"source": c.source, "attemptOrdinal": nil, "artifacts": []any{}, "truncated": false})
			}, nil)
			defer srv.Close()

			err := doJobPull(basePullOpts(srv.URL, t.TempDir()))
			if err == nil {
				t.Fatalf("want an error for source %q", c.source)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("source %q: want error containing %q, got: %s", c.source, c.want, err.Error())
			}
		})
	}
}

// TestJobPullOkWithNoArtifactsIsNotAnError: a run still executing (or one
// that genuinely produced nothing) answers "ok" with an empty list -- a
// legitimate, non-error outcome, distinct from every "could not look" case
// above.
func TestJobPullOkWithNoArtifactsIsNotAnError(t *testing.T) {
	srv := jobPullServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"source": "ok", "attemptOrdinal": 1, "artifacts": []any{}, "truncated": false})
	}, nil)
	defer srv.Close()

	var out bytes.Buffer
	opts := basePullOpts(srv.URL, t.TempDir())
	opts.out = &out
	if err := doJobPull(opts); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "no output files") {
		t.Fatalf("want the empty-run message, got: %s", out.String())
	}
}

// TestJobPullDownloadsAndSkipsAlreadyPresentFiles is the resumability
// contract: a file already on disk at the artifact's own reported size is
// skipped (never re-downloaded), while a missing one is fetched and written
// atomically.
func TestJobPullDownloadsAndSkipsAlreadyPresentFiles(t *testing.T) {
	const wantBytes = "hello from the box"
	srv := jobPullServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			writeData(w, map[string]any{
				"source":         "ok",
				"attemptOrdinal": 1,
				"truncated":      false,
				"artifacts": []map[string]any{
					{"key": "log", "sizeBytes": int64(len("already here")), "lastModified": "2026-09-20T00:00:00Z"},
					{"key": "outputs/model.pt", "sizeBytes": int64(len(wantBytes)), "lastModified": "2026-09-20T00:00:00Z"},
				},
			})
		},
		func(w http.ResponseWriter, r *http.Request) {
			key := r.URL.Query().Get("key")
			if key != "outputs/model.pt" {
				t.Fatalf("download must never be called for a file already satisfied on disk, got key %q", key)
			}
			writeData(w, map[string]any{"url": "https://example.invalid/dl/outputs-model", "expiresAt": "2026-09-20T01:00:00Z", "attemptOrdinal": 1})
		},
	)
	defer srv.Close()

	dest := t.TempDir()
	// Pre-seed "log" at exactly its reported size so it must be skipped.
	if err := os.WriteFile(filepath.Join(dest, "log"), []byte("already here"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	opts := basePullOpts(srv.URL, dest)
	opts.out = &out
	opts.httpGet = func(url string) (*http.Response, error) {
		if url != "https://example.invalid/dl/outputs-model" {
			t.Fatalf("unexpected download URL %q", url)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(wantBytes))}, nil
	}

	if err := doJobPull(opts); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dest, "outputs", "model.pt"))
	if err != nil {
		t.Fatalf("want outputs/model.pt written, got: %v", err)
	}
	if string(got) != wantBytes {
		t.Fatalf("outputs/model.pt = %q, want %q", got, wantBytes)
	}
	if !strings.Contains(out.String(), "Pulled 1 file") {
		t.Fatalf("want a summary reporting exactly one pulled file, got: %s", out.String())
	}
}

// TestLatestRunWithABoxSkipsQueuedRunningAndUnservable pins the default-run
// selection doJobPull uses when --run is not given: only "succeeded" or
// "failed" runs actually reached a box and could have artifacts.
// "unservable" never got a box at all (nothing to list), and
// "queued"/"running" have not landed anything yet -- neither is terminal.
func TestLatestRunWithABoxSkipsQueuedRunningAndUnservable(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/jobs/job-1/runs", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{
			{"id": "run-4", "status": "running"},
			{"id": "run-3", "status": "unservable"},
			{"id": "run-2", "status": "succeeded"},
			{"id": "run-1", "status": "failed"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := newControlClient(&config.Credential{Token: "t", TeamID: "team-1", APIURL: srv.URL})
	got, err := latestRunWithABox(client, "job-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "run-2" {
		t.Fatalf("latestRunWithABox = %q, want the first succeeded/failed run in listing order (run-2)", got)
	}
}

// TestLatestRunWithABoxRefusesWhenNothingQualifies: every run is either not
// terminal or never got a box, so there is nothing to pull and the CLI must
// say so rather than guessing.
func TestLatestRunWithABoxRefusesWhenNothingQualifies(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/jobs/job-1/runs", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "run-1", "status": "unservable"}, {"id": "run-2", "status": "queued"}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := newControlClient(&config.Credential{Token: "t", TeamID: "team-1", APIURL: srv.URL})
	if _, err := latestRunWithABox(client, "job-1"); err == nil {
		t.Fatal("want an error when no run has reached a box")
	}
}

// TestSafeArtifactPathRefusesEscapes: the artifact key is untrusted wire
// input, and nothing here should ever be able to write outside the
// destination directory the caller named.
func TestSafeArtifactPathRefusesEscapes(t *testing.T) {
	dest := "/tmp/aq-pull-dest"
	for _, key := range []string{"../escape", "a/../../escape", "/etc/passwd", ".."} {
		if _, err := safeArtifactPath(dest, key); err == nil {
			t.Errorf("safeArtifactPath(%q, %q) = nil error, want a refusal", dest, key)
		}
	}
	got, err := safeArtifactPath(dest, "outputs/model.pt")
	if err != nil {
		t.Fatalf("safeArtifactPath must accept an ordinary nested key: %v", err)
	}
	want := filepath.Join(dest, "outputs", "model.pt")
	if got != want {
		t.Fatalf("safeArtifactPath = %q, want %q", got, want)
	}
}
