package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

const fixtureKey = "rpa_FIXTURE_SECRET_VALUE_0123456789"

func connectorFixtureProviders() []api.ConnectorProvider {
	return []api.ConnectorProvider{
		{Provider: "runpod", Fields: []api.ConnectorField{{Name: "RUNPOD_API_KEY", Label: "RunPod API key", Secret: true}}},
		{Provider: "vastai", Fields: []api.ConnectorField{{Name: "VASTAI_API_KEY", Secret: true}}},
	}
}

func fixtureAddDeps(out *bytes.Buffer, verify func(provider, path string) (string, error)) accountsAddDeps {
	return accountsAddDeps{
		providers: func() ([]api.ConnectorProvider, error) { return connectorFixtureProviders(), nil },
		prompt:    func(string, bool) (string, error) { return fixtureKey, nil },
		verify:    verify,
		now:       func() time.Time { return time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC) },
		out:       out,
	}
}

func sandboxConfigDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cfg")
	t.Setenv("AQ_CONFIG_DIR", dir)
	return dir
}

// accounts.json is the executor's input file: 0600, keyed by the env var names
// the server listed, with the label beside them. The fingerprint bookkeeping
// lives in a separate file so the executor never reads a key it did not define.
func TestAccountsAddWritesExecutorFileAt0600(t *testing.T) {
	dir := sandboxConfigDir(t)
	var out bytes.Buffer
	var verifiedPath string
	deps := fixtureAddDeps(&out, func(provider, path string) (string, error) {
		verifiedPath = path
		return "ok " + keyFingerprint(fixtureKey) + "\n", nil
	})
	if err := runAccountsAdd(deps, "RunPod", "work"); err != nil {
		t.Fatalf("add: %v", err)
	}
	accPath := filepath.Join(dir, accountsFileName)
	if verifiedPath != accPath {
		t.Fatalf("verify ran against %q, want the stored file %q", verifiedPath, accPath)
	}
	for _, p := range []string{accPath, filepath.Join(dir, accountsMetaFileName)} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %#o, want 0600", p, info.Mode().Perm())
		}
	}
	got := accountsFile{}
	if err := readJSONFile(accPath, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got["runpod"]) != 2 || got["runpod"]["RUNPOD_API_KEY"] != fixtureKey || got["runpod"]["label"] != "work" {
		t.Fatalf("accounts.json = %v, want runpod -> {RUNPOD_API_KEY, label}", got)
	}
	meta, _ := os.ReadFile(filepath.Join(dir, accountsMetaFileName))
	if strings.Contains(string(meta), fixtureKey) {
		t.Fatal("the metadata file must hold the fingerprint only, never the key")
	}
	if !strings.Contains(string(meta), keyFingerprint(fixtureKey)) {
		t.Fatalf("metadata lacks the fingerprint: %s", meta)
	}
	if !strings.Contains(out.String(), "ok sha256:") || strings.Contains(out.String(), fixtureKey) {
		t.Fatalf("add must print the verify line and never the key, got %q", out.String())
	}

	// ls shows provider, label, fingerprint, verified-at, never the key.
	var ls bytes.Buffer
	if err := accountsLs(&ls); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"runpod", "work", keyFingerprint(fixtureKey), "2026-11-10T12:00:00Z"} {
		if !strings.Contains(ls.String(), s) {
			t.Fatalf("ls missing %q:\n%s", s, ls.String())
		}
	}
	if strings.Contains(ls.String(), fixtureKey) {
		t.Fatal("ls printed the key")
	}
}

func TestAccountsAddKeepsNothingWhenVerificationFails(t *testing.T) {
	dir := sandboxConfigDir(t)
	var out bytes.Buffer
	deps := fixtureAddDeps(&out, func(string, string) (string, error) { return "", errors.New("401 unauthorized") })
	err := runAccountsAdd(deps, "runpod", "")
	if err == nil || !strings.Contains(err.Error(), "401 unauthorized") {
		t.Fatalf("want the provider's error verbatim, got %v", err)
	}
	got := accountsFile{}
	if err := readJSONFile(filepath.Join(dir, accountsFileName), &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["runpod"]; ok {
		t.Fatal("a rejected key stayed stored")
	}
}

func TestAccountsAddRefusesAWS(t *testing.T) {
	sandboxConfigDir(t)
	err := runAccountsAdd(fixtureAddDeps(&bytes.Buffer{}, nil), "aws", "")
	if err == nil || err.Error() != "aws is not supported in local mode" {
		t.Fatalf("got %v", err)
	}
}

func TestAccountsAddRefusesAProviderTheServerDoesNotList(t *testing.T) {
	sandboxConfigDir(t)
	err := runAccountsAdd(fixtureAddDeps(&bytes.Buffer{}, nil), "nope", "")
	if err == nil || !strings.Contains(err.Error(), "runpod") {
		t.Fatalf("want a refusal naming the supported providers, got %v", err)
	}
}

func TestAccountsRmRemovesTheProvider(t *testing.T) {
	dir := sandboxConfigDir(t)
	deps := fixtureAddDeps(&bytes.Buffer{}, func(string, string) (string, error) { return "ok", nil })
	if err := runAccountsAdd(deps, "runpod", ""); err != nil {
		t.Fatal(err)
	}
	if err := accountsRm([]string{"runpod"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	got := accountsFile{}
	_ = readJSONFile(filepath.Join(dir, accountsFileName), &got)
	if len(got) != 0 {
		t.Fatalf("accounts left after rm: %v", got)
	}
	if err := accountsRm([]string{"runpod"}, &bytes.Buffer{}); err == nil {
		t.Fatal("removing an absent account must say so")
	}
}

// mjolnirLocalServer serves the executor download route and the archive.
func mjolnirLocalServer(t *testing.T, archive []byte, status int) *httptest.Server {
	t.Helper()
	sum := sha256.Sum256(archive)
	mux := http.NewServeMux()
	mux.HandleFunc("/artifacts/mjolnir-local/download-url", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if status != http.StatusOK {
			w.WriteHeader(status)
			fmt.Fprint(w, `{"success":false,"error":"no mjolnir-local build for this platform"}`)
			return
		}
		fmt.Fprintf(w, `{"success":true,"data":{"url":"http://%s/archive","sha256":%q,"version":"v9.9.9","asset":"mjolnir-local_Test.tar.gz"}}`,
			r.Host, hex.EncodeToString(sum[:]))
	})
	mux.HandleFunc("/archive", func(w http.ResponseWriter, _ *http.Request) { w.Write(archive) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func isolateExecutorInstall(t *testing.T) string {
	t.Helper()
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("PATH", "/usr/bin:/bin")
	return filepath.Join(dataHome, "aquanode", "bin", "mjolnir")
}

func skipWithoutSh(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("no /bin/sh")
	}
}

// A tarball whose member does not exec must be refused, and must not be left
// where `aq connect` would run it.
func TestEnsureMjolnirLocalRefusesAMemberThatDoesNotExec(t *testing.T) {
	skipWithoutSh(t)
	dest := isolateExecutorInstall(t)
	srv := mjolnirLocalServer(t, ogreTarball(t, "mjolnir", []byte("this is not an executable, it has no shebang\n")), http.StatusOK)

	_, err := ensureMjolnirLocal(api.New(srv.URL), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "does not run") {
		t.Fatalf("want a refusal saying the executor does not run, got %v", err)
	}
	if _, serr := os.Stat(dest); !os.IsNotExist(serr) {
		t.Fatalf("a member that does not exec was left installed at %s", dest)
	}
	if _, serr := os.Stat(dest + ".new"); !os.IsNotExist(serr) {
		t.Fatal("the staging file was left behind")
	}
}

func TestEnsureMjolnirLocalExits12WhenNoBuildForThePlatform(t *testing.T) {
	isolateExecutorInstall(t)
	srv := mjolnirLocalServer(t, nil, http.StatusNotFound)
	_, err := ensureMjolnirLocal(api.New(srv.URL), &bytes.Buffer{})
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 12 {
		t.Fatalf("want exit 12, got %v", err)
	}
}

const stubExecutor = `#!/bin/sh
if [ "$1" = version ]; then echo 'mjolnir v9.9.9'; exit 0; fi
if [ "$1" = local ]; then
  while [ $# -gt 0 ]; do
    case "$1" in
      --session-file) S="$2"; shift ;;
    esac
    shift
  done
  MODE=$(stat -f %Lp "$S" 2>/dev/null || stat -c %a "$S")
  echo "session-file-mode $MODE"
  echo "connected as team-1: runpod (work)"
  trap 'echo "stub: session ended"; exit 0' INT TERM
  while :; do sleep 0.1; done
fi
exit 9
`

// aq connect against a fake orchestrator and a stub executor: installs it,
// hands it a 0600 session file carrying the stored credential, streams its
// "connected" line, and Ctrl-C (context cancel) ends the child and removes the
// session file.
func TestConnectRunsTheStubAndHandsItA0600SessionFile(t *testing.T) {
	skipWithoutSh(t)
	cfg := sandboxConfigDir(t)
	isolateExecutorInstall(t)
	srv := mjolnirLocalServer(t, ogreTarball(t, "mjolnir", []byte(stubExecutor)), http.StatusOK)

	accPath := filepath.Join(cfg, accountsFileName)
	if err := writeSecretFile(accPath, accountsFile{"runpod": {"RUNPOD_API_KEY": fixtureKey, "label": "work"}}); err != nil {
		t.Fatal(err)
	}
	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}

	pr := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runConnect(ctx, connectOptions{
			client: api.NewAuthed(srv.URL, cred.Token, cred.TeamID), cred: cred, apiURL: srv.URL, accountsPath: accPath, out: pr,
		})
	}()

	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(pr.String(), "connected as team-1: runpod (work)") {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("never saw the connected line, output:\n%s", pr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(pr.String(), "session-file-mode 600") {
		t.Fatalf("the executor saw a session file that was not 0600:\n%s", pr.String())
	}
	sessionPath := filepath.Join(cfg, connectorSessionFile)
	if info, err := os.Stat(sessionPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("session file while running: %v %v", info, err)
	}
	body, _ := os.ReadFile(sessionPath)
	if !strings.Contains(string(body), "aq_sk_test") || !strings.Contains(string(body), "team-1") {
		t.Fatalf("session file lacks the credential: %s", body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Ctrl-C must end the session cleanly, got %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("connect did not return after Ctrl-C")
	}
	if _, err := os.Stat(sessionPath); !os.IsNotExist(err) {
		t.Fatal("the session file (it holds the credential) outlived the session")
	}
	if !strings.Contains(pr.String(), "stub: session ended") {
		t.Fatalf("the child was not interrupted:\n%s", pr.String())
	}
	if strings.Contains(pr.String(), fixtureKey) {
		t.Fatal("connect output carried a provider key")
	}
}

func TestConnectRefusesWithNoStoredAccounts(t *testing.T) {
	cfg := sandboxConfigDir(t)
	err := runConnect(context.Background(), connectOptions{accountsPath: filepath.Join(cfg, accountsFileName), out: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "aq accounts add") {
		t.Fatalf("want a pointer to `aq accounts add`, got %v", err)
	}
}

// The server's 409 connector_offline becomes exit 11 with the server's message
// plus the hint; any other 409 (and other codes) pass through untouched.
func TestConnectorOffline409MapsToExit11(t *testing.T) {
	off := &api.APIError{Status: 409, Message: "No connector is online for RunPod. Run `aq connect` on the machine that holds your RunPod key.", Data: []byte(`{"code":"connector_offline"}`)}
	got := asConnectorOffline(fmt.Errorf("start failed: %w", off))
	var ee *exitError
	if !errors.As(got, &ee) || ee.code != 11 {
		t.Fatalf("want exit 11, got %v", got)
	}
	if !strings.Contains(got.Error(), "No connector is online for RunPod") || !strings.Contains(got.Error(), "Start `aq connect` in another terminal") {
		t.Fatalf("want the server message plus the hint, got %q", got.Error())
	}
	for _, other := range []error{
		&api.APIError{Status: 409, Message: "no offer", Data: nil},
		&api.APIError{Status: 409, Message: "cap", Data: []byte(`{"code":"connector_cap"}`)},
		&api.APIError{Status: 402, Message: "x", Data: []byte(`{"code":"connector_offline"}`)},
		errors.New("plain"),
	} {
		if out := asConnectorOffline(other); out != other {
			t.Fatalf("%v must pass through unchanged, got %v", other, out)
		}
	}
	if !strings.Contains(usageText, "  11 ") || !strings.Contains(usageText, "connector_offline") {
		t.Fatal("aq --help must document exit 11")
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }
