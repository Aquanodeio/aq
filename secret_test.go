package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

func secretTestCred(serverURL string) *config.Credential {
	return &config.Credential{Token: "aq_sk_test", TeamID: "team-1", APIURL: serverURL}
}

// --- runSecretSet: wire behavior ---------------------------------------

// TestRunSecretSetEnvPostsValueStringOnTheWire asserts the exact request an
// env secret produces: POST /secrets/teams/<teamId> with {name,type,value}
// where value is the bare string, matching secrets.schemas.ts's
// CreateSecretSchema.
func TestRunSecretSetEnvPostsValueStringOnTheWire(t *testing.T) {
	var gotPath string
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ = readAll(r)
		writeData(w, map[string]any{
			"id": "sec-1", "name": "HF_TOKEN", "type": "env",
			"createdAt": "2026-09-01T00:00:00Z", "updatedAt": "2026-09-01T00:00:00Z", "rotatedAt": nil,
		})
	}))
	defer srv.Close()

	var out bytes.Buffer
	opts := secretSetOptions{
		cred: secretTestCred(srv.URL), name: "HF_TOKEN", secretType: "env",
		envValue: "hf_supersecret", out: &out,
	}
	if err := runSecretSet(opts); err != nil {
		t.Fatalf("runSecretSet: %v", err)
	}
	if gotPath != "/secrets/teams/team-1" {
		t.Fatalf("path = %q, want /secrets/teams/team-1", gotPath)
	}

	var decoded struct {
		Name  string `json:"name"`
		Type  string `json:"type"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode request body: %v (body: %s)", err, body)
	}
	if decoded.Name != "HF_TOKEN" || decoded.Type != "env" || decoded.Value != "hf_supersecret" {
		t.Fatalf("decoded = %+v, want name=HF_TOKEN type=env value=hf_supersecret", decoded)
	}
}

// TestRunSecretSetRegistryPostsCredentialObjectOnTheWire asserts the registry
// shape: value is an OBJECT {server,username,token}, never a string.
func TestRunSecretSetRegistryPostsCredentialObjectOnTheWire(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAll(r)
		writeData(w, map[string]any{
			"id": "sec-2", "name": "ghcr", "type": "registry",
			"createdAt": "2026-09-01T00:00:00Z", "updatedAt": "2026-09-01T00:00:00Z", "rotatedAt": nil,
		})
	}))
	defer srv.Close()

	var out bytes.Buffer
	opts := secretSetOptions{
		cred: secretTestCred(srv.URL), name: "ghcr", secretType: "registry",
		server: "ghcr.io", username: "octocat", token: "ghp_abc123", out: &out,
	}
	if err := runSecretSet(opts); err != nil {
		t.Fatalf("runSecretSet: %v", err)
	}

	var decoded struct {
		Value api.RegistryCredential `json:"value"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode request body: %v (body: %s)", err, body)
	}
	if decoded.Value != (api.RegistryCredential{Server: "ghcr.io", Username: "octocat", Token: "ghp_abc123"}) {
		t.Fatalf("value = %+v, want the registry credential object", decoded.Value)
	}
}

// TestRunSecretSetNeverPrintsTheValue: the create response carries no value
// field at all, but this pins the OUTPUT side too: a future edit that
// echoed opts.envValue/opts.token into the success line would leak a secret
// to a terminal, a log, or a CI transcript.
func TestRunSecretSetNeverPrintsTheValue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"id": "sec-1", "name": "HF_TOKEN", "type": "env", "rotatedAt": nil})
	}))
	defer srv.Close()

	var out bytes.Buffer
	opts := secretSetOptions{
		cred: secretTestCred(srv.URL), name: "HF_TOKEN", secretType: "env",
		envValue: "hf_supersecret_value_marker", out: &out,
	}
	if err := runSecretSet(opts); err != nil {
		t.Fatalf("runSecretSet: %v", err)
	}
	if strings.Contains(out.String(), "hf_supersecret_value_marker") {
		t.Fatalf("output must never contain the secret value, got: %s", out.String())
	}
}

// TestRunSecretSetWrapsA409WhenNameTaken pins the standard error-wrap shape
// this CLI uses elsewhere (job.go's "could not create job ...: %w").
func TestRunSecretSetWrapsA409WhenNameTaken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusConflict, `a secret named "HF_TOKEN" already exists on this team`)
	}))
	defer srv.Close()

	opts := secretSetOptions{cred: secretTestCred(srv.URL), name: "HF_TOKEN", secretType: "env", envValue: "x", out: &bytes.Buffer{}}
	err := runSecretSet(opts)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "could not create secret") || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected the wrap plus the backend message, got: %v", err)
	}
}

// --- secretSet: local flag validation (no server) -----------------------

func TestSecretSetRequiresType(t *testing.T) {
	detachedSandbox(t)
	err := secretSet([]string{"HF_TOKEN", "--value", "x"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "--type") {
		t.Fatalf("error should name --type, got: %v", err)
	}
}

func TestSecretSetRejectsUnknownType(t *testing.T) {
	detachedSandbox(t)
	err := secretSet([]string{"HF_TOKEN", "--type", "ssh-key", "--value", "x"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), `"ssh-key"`) {
		t.Fatalf("error should name the bad type, got: %v", err)
	}
}

// An env secret's name doubles as the env var key a job's Runs see at
// dispatch, so a name shaped like "my secret" or "1FOO" must be refused
// locally, before any network round trip.
func TestSecretSetRejectsInvalidEnvName(t *testing.T) {
	detachedSandbox(t)
	err := secretSet([]string{"not a valid name", "--type", "env", "--value", "x"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "env var identifier") {
		t.Fatalf("error should explain the env-key naming rule, got: %v", err)
	}
}

// A registry secret's name is just a label, so the env-key format rule must
// NOT apply to it, this is the negative case pinning that.
func TestSecretSetAllowsAnyLabelForRegistryName(t *testing.T) {
	detachedSandbox(t)
	err := secretSet([]string{"my ghcr creds", "--type", "registry", "--server", "ghcr.io", "--username", "u", "--token", "t"})
	// No server is running, so this must fail at requireLogin (past all local
	// validation), never at the name check.
	if err == nil {
		t.Fatal("expected an error (no stored credential in the sandbox)")
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("expected to reach the login check, got: %v", err)
	}
}

func TestSecretSetRegistryRequiresAllThreeFields(t *testing.T) {
	detachedSandbox(t)
	cases := [][]string{
		{"ghcr", "--type", "registry", "--username", "u", "--token", "t"},
		{"ghcr", "--type", "registry", "--server", "ghcr.io", "--token", "t"},
		{"ghcr", "--type", "registry", "--server", "ghcr.io", "--username", "u"},
	}
	for _, args := range cases {
		if err := secretSet(args); err == nil {
			t.Fatalf("args %v: expected an error", args)
		} else if !strings.Contains(err.Error(), "--server, --username and --token") {
			t.Fatalf("args %v: expected the missing-fields message, got: %v", args, err)
		}
	}
}

func TestSecretSetRequiresAName(t *testing.T) {
	detachedSandbox(t)
	err := secretSet([]string{"--type", "env", "--value", "x"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "usage: aq secret set") {
		t.Fatalf("expected the usage string, got: %v", err)
	}
}

// --- runSecretList --------------------------------------------------------

func TestRunSecretListRendersNameAndTypeNeverAValue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/secrets/teams/team-1" {
			t.Errorf("path = %q, want /secrets/teams/team-1", r.URL.Path)
		}
		writeData(w, map[string]any{"secrets": []map[string]any{
			{"id": "sec-1", "name": "HF_TOKEN", "type": "env", "createdAt": "2026-09-01T00:00:00Z", "updatedAt": "2026-09-01T00:00:00Z", "rotatedAt": nil},
			{"id": "sec-2", "name": "ghcr", "type": "registry", "createdAt": "2026-09-02T00:00:00Z", "updatedAt": "2026-09-03T00:00:00Z", "rotatedAt": "2026-09-03T00:00:00Z"},
		}})
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := runSecretList(secretListOptions{cred: secretTestCred(srv.URL), out: &out}); err != nil {
		t.Fatalf("runSecretList: %v", err)
	}
	got := out.String()
	for _, want := range []string{"HF_TOKEN", "env", "ghcr", "registry", "2026-09-03T00:00:00Z", "never"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q, got: %s", want, got)
		}
	}
}

func TestRunSecretListEmptyPrintsNudge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"secrets": []map[string]any{}})
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := runSecretList(secretListOptions{cred: secretTestCred(srv.URL), out: &out}); err != nil {
		t.Fatalf("runSecretList: %v", err)
	}
	if !strings.Contains(out.String(), "aq secret set") {
		t.Fatalf("expected the empty-state nudge, got: %s", out.String())
	}
}

// --- runSecretRemove -------------------------------------------------------

// TestRunSecretRemoveResolvesNameToIDThenDeletes: the delete route is keyed
// by secretId, not name, so removing by name must resolve first (same
// two-step pattern resolveJobID uses) and the DELETE must carry the id.
func TestRunSecretRemoveResolvesNameToIDThenDeletes(t *testing.T) {
	var deletedPath string
	mux := http.NewServeMux()
	mux.HandleFunc("/secrets/teams/team-1", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"secrets": []map[string]any{
			{"id": "sec-1", "name": "HF_TOKEN", "type": "env", "rotatedAt": nil},
		}})
	})
	mux.HandleFunc("/secrets/teams/team-1/sec-1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.NotFound(w, r)
			return
		}
		deletedPath = r.URL.Path
		writeData(w, map[string]any{"success": true})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var out bytes.Buffer
	opts := secretRemoveOptions{cred: secretTestCred(srv.URL), target: "HF_TOKEN", out: &out}
	if err := runSecretRemove(opts); err != nil {
		t.Fatalf("runSecretRemove: %v", err)
	}
	if deletedPath != "/secrets/teams/team-1/sec-1" {
		t.Fatalf("DELETE path = %q, want /secrets/teams/team-1/sec-1 (resolved id, not the typed name)", deletedPath)
	}
}

func TestRunSecretRemoveUnknownNameErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"secrets": []map[string]any{}})
	}))
	defer srv.Close()

	opts := secretRemoveOptions{cred: secretTestCred(srv.URL), target: "ghost", out: &bytes.Buffer{}}
	err := runSecretRemove(opts)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), `no secret named "ghost"`) {
		t.Fatalf("expected the no-match message, got: %v", err)
	}
}

// --- runSecretRotate --------------------------------------------------------

func secretRotateListServer(t *testing.T, secretType string, rotate http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/secrets/teams/team-1", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"secrets": []map[string]any{
			{"id": "sec-1", "name": "myname", "type": secretType, "rotatedAt": nil},
		}})
	})
	mux.HandleFunc("/secrets/teams/team-1/sec-1/rotate", rotate)
	return httptest.NewServer(mux)
}

// TestRunSecretRotateEnvSendsBareStringValue mirrors the create-side wire
// assertion: rotating an env secret must send `{"value":"<string>"}`, with
// no `type` key at all (RotateSecretRequest carries none, the backend keeps
// the secret's existing type across a rotation).
func TestRunSecretRotateEnvSendsBareStringValue(t *testing.T) {
	var body []byte
	srv := secretRotateListServer(t, "env", func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAll(r)
		writeData(w, map[string]any{"id": "sec-1", "name": "myname", "type": "env", "rotatedAt": "2026-09-07T00:00:00Z"})
	})
	defer srv.Close()

	opts := secretRotateOptions{
		cred: secretTestCred(srv.URL), target: "myname",
		valuePassed: true, value: "new-value", out: &bytes.Buffer{},
	}
	if err := runSecretRotate(opts); err != nil {
		t.Fatalf("runSecretRotate: %v", err)
	}
	if strings.Contains(string(body), `"type"`) {
		t.Fatalf("rotate body must never carry a type key, got: %s", body)
	}
	var decoded struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode: %v (body: %s)", err, body)
	}
	if decoded.Value != "new-value" {
		t.Fatalf("value = %q, want new-value", decoded.Value)
	}
}

func TestRunSecretRotateRegistrySendsCredentialObject(t *testing.T) {
	var body []byte
	srv := secretRotateListServer(t, "registry", func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAll(r)
		writeData(w, map[string]any{"id": "sec-1", "name": "myname", "type": "registry", "rotatedAt": "2026-09-07T00:00:00Z"})
	})
	defer srv.Close()

	opts := secretRotateOptions{
		cred: secretTestCred(srv.URL), target: "myname",
		server: "ghcr.io", username: "u2", token: "t2", out: &bytes.Buffer{},
	}
	if err := runSecretRotate(opts); err != nil {
		t.Fatalf("runSecretRotate: %v", err)
	}
	var decoded struct {
		Value api.RegistryCredential `json:"value"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode: %v (body: %s)", err, body)
	}
	if decoded.Value != (api.RegistryCredential{Server: "ghcr.io", Username: "u2", Token: "t2"}) {
		t.Fatalf("value = %+v", decoded.Value)
	}
}

// TestRunSecretRotateRejectsRegistryFlagsForAnEnvSecret: the shape must match
// the secret's ACTUAL stored type, discovered by resolving it first, never
// guessed from whichever flags happened to be filled in.
func TestRunSecretRotateRejectsRegistryFlagsForAnEnvSecret(t *testing.T) {
	srv := secretRotateListServer(t, "env", func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("must never reach the network: the shape mismatch is a local refusal")
	})
	defer srv.Close()

	opts := secretRotateOptions{
		cred: secretTestCred(srv.URL), target: "myname",
		server: "ghcr.io", username: "u", token: "t", out: &bytes.Buffer{},
	}
	err := runSecretRotate(opts)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "is type env") {
		t.Fatalf("error should name the secret's real type, got: %v", err)
	}
}

func TestRunSecretRotateRejectsValueFlagForARegistrySecret(t *testing.T) {
	srv := secretRotateListServer(t, "registry", func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("must never reach the network: the shape mismatch is a local refusal")
	})
	defer srv.Close()

	opts := secretRotateOptions{
		cred: secretTestCred(srv.URL), target: "myname",
		valuePassed: true, value: "x", out: &bytes.Buffer{},
	}
	err := runSecretRotate(opts)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "is type registry") {
		t.Fatalf("error should name the secret's real type, got: %v", err)
	}
}

// --- readValueFromFlagOrStdin / flagPassed --------------------------------

func TestReadValueFromFlagOrStdinPrefersTheFlag(t *testing.T) {
	v, err := readValueFromFlagOrStdin(true, "flag-value", "value", strings.NewReader("stdin-value\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != "flag-value" {
		t.Fatalf("v = %q, want flag-value (stdin must be untouched when the flag was passed)", v)
	}
}

func TestReadValueFromFlagOrStdinFallsBackToStdinAndTrimsOneTrailingNewline(t *testing.T) {
	v, err := readValueFromFlagOrStdin(false, "", "value", strings.NewReader("piped-secret\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != "piped-secret" {
		t.Fatalf("v = %q, want piped-secret with the trailing newline trimmed", v)
	}
}

// A flag value is used byte-for-byte -- an explicit trailing newline someone
// actually typed into --value must survive, unlike stdin's.
func TestReadValueFromFlagOrStdinNeverTrimsAnExplicitFlagValue(t *testing.T) {
	v, err := readValueFromFlagOrStdin(true, "value-with-newline\n", "value", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != "value-with-newline\n" {
		t.Fatalf("v = %q, want the flag value untouched", v)
	}
}

func TestReadValueFromFlagOrStdinRejectsAllWhitespaceStdin(t *testing.T) {
	_, err := readValueFromFlagOrStdin(false, "", "value", strings.NewReader("\n"))
	if err == nil {
		t.Fatal("expected an error for an empty stdin read")
	}
}

func TestFlagPassedDistinguishesExplicitFromDefault(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	v := fs.String("value", "", "")
	if err := fs.Parse([]string{"--value", "x"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	_ = v
	if !flagPassed(fs, "value") {
		t.Fatal("flagPassed should report true for an explicitly passed flag")
	}
	if flagPassed(fs, "other") {
		t.Fatal("flagPassed should report false for a flag that was never parsed")
	}
}

// --- requireTeamID ---------------------------------------------------------

func TestRequireTeamIDErrorsWhenEmpty(t *testing.T) {
	_, err := requireTeamID(&config.Credential{Token: "x", TeamID: ""})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "aq login") {
		t.Fatalf("error should name the fix, got: %v", err)
	}
}

func TestRequireTeamIDPassesThroughWhenSet(t *testing.T) {
	got, err := requireTeamID(&config.Credential{Token: "x", TeamID: "team-9"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "team-9" {
		t.Fatalf("got = %q, want team-9", got)
	}
}

// --- secret dispatch ---------------------------------------------------------

func TestSecretDispatchRejectsUnknownSubcommand(t *testing.T) {
	err := secret([]string{"frobnicate"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "frobnicate") {
		t.Fatalf("error should name the unknown subcommand, got: %v", err)
	}
}

func TestSecretDispatchWithNoArgsListsSubcommands(t *testing.T) {
	err := secret(nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"set", "list", "rm", "rotate"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should list %q, got: %v", want, err)
		}
	}
}

// --- `aq job create --secret` wiring ---------------------------------------

// TestCreateJobSendsSecretsOnTheWire: --secret is repeatable and appends to
// CreateJobRequest.Secrets in the order given.
func TestCreateJobSendsSecretsOnTheWire(t *testing.T) {
	var body []byte
	srv := jobCreateServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAll(r)
		writeData(w, map[string]any{"id": "ep-1", "name": "myenv", "versionId": 555})
	})
	defer srv.Close()

	opts := baseCreateOpts(srv.URL)
	opts.secrets = []string{"HF_TOKEN", "WANDB_KEY"}
	if err := runJobCreate(opts); err != nil {
		t.Fatalf("runJobCreate: %v", err)
	}

	var decoded api.CreateJobRequest
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if len(decoded.Secrets) != 2 || decoded.Secrets[0] != "HF_TOKEN" || decoded.Secrets[1] != "WANDB_KEY" {
		t.Fatalf("secrets = %v, want [HF_TOKEN WANDB_KEY] in order (raw body: %s)", decoded.Secrets, body)
	}
}

// TestCreateJobOmitsSecretsWhenNotPassed: no --secret at all must leave the
// key off the wire entirely, not send an empty array -- CreateJobRequest.Secrets
// carries `omitempty` for exactly this.
func TestCreateJobOmitsSecretsWhenNotPassed(t *testing.T) {
	var body []byte
	srv := jobCreateServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAll(r)
		writeData(w, map[string]any{"id": "ep-1", "name": "myenv", "versionId": 555})
	})
	defer srv.Close()

	opts := baseCreateOpts(srv.URL)
	if err := runJobCreate(opts); err != nil {
		t.Fatalf("runJobCreate: %v", err)
	}
	if strings.Contains(string(body), "secrets") {
		t.Fatalf("no --secret passed must omit the key entirely, got: %s", body)
	}
}

// TestJobCreateParsesRepeatableSecretFlag exercises the actual flag-parsing
// entry point, not just runJobCreate; --secret must accumulate rather than
// overwrite, same as ssh.go's -L.
func TestJobCreateParsesRepeatableSecretFlag(t *testing.T) {
	detachedSandbox(t)
	// No server and no stored credential: this only proves the flags parsed
	// and reached the login check, mirroring TestJobCreateNeedsNoCapFlag.
	err := jobCreate([]string{jobTestSetupID, "3", "--max-instances", "1", "--secret", "A", "--secret", "B"})
	if err == nil {
		t.Fatal("expected an error (no stored credential in the sandbox)")
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("expected to reach the login check, got: %v", err)
	}
}
