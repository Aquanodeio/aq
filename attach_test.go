package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// customerKeys is a real-shaped authorized_keys: several unrelated keys, a
// comment, and no trailing newline. On a machine somebody holds on a multi-year
// lease this file is frequently their only way in, and aq cannot restore access
// to it if we get this wrong.
const customerKeys = "# ops team\n" +
	"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAA alice@laptop\n" +
	"ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQAA bob@desktop\n" +
	"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBBB deploy@ci"

// attachPreflightOutput assembles what the box prints for the preflight script.
func attachPreflightOutput(akState, keys, port string) string {
	return fullSurvey +
		"ak=" + akState + "\n" +
		"ak_begin\n" + keys + "\nak_end\n" +
		"port=" + port + "\n" +
		"preflight_ok=1\n"
}

// A preflight that cannot read the file a later step writes near must refuse.
// "Could not look" blocks — it never proceeds to a write.
func TestParseAttachPreflightRefusesAnUnreadableAuthorizedKeys(t *testing.T) {
	_, err := parseAttachPreflight([]byte(attachPreflightOutput("unreadable", "", "free")))
	if err == nil || !strings.Contains(err.Error(), "authorized_keys") {
		t.Fatalf("expected a refusal naming authorized_keys, got %v", err)
	}
}

func TestParseAttachPreflightReadsTheKeysAndThePortState(t *testing.T) {
	pre, err := parseAttachPreflight([]byte(attachPreflightOutput("readable", customerKeys, "busy")))
	if err != nil {
		t.Fatalf("parseAttachPreflight: %v", err)
	}
	if !strings.Contains(pre.authorizedKeys, "alice@laptop") || !strings.Contains(pre.authorizedKeys, "deploy@ci") {
		t.Fatalf("keys were not read back: %q", pre.authorizedKeys)
	}
	if pre.port != portBusy {
		t.Fatalf("port state decoded wrong: got %v, want portBusy", pre.port)
	}

	// An absent file is a fact, not a failure — a fresh box has no keys yet.
	pre, err = parseAttachPreflight([]byte(attachPreflightOutput("absent", "", "notool")))
	if err != nil {
		t.Fatalf("parseAttachPreflight(absent): %v", err)
	}
	if pre.port != portNoTool {
		t.Fatalf("port=notool must decode as portNoTool, got %v", pre.port)
	}
}

// The collapse bug itself. If a box's raw preflight output ever
// arrives with "ak_end" glued onto the previous line (the shape produced by an
// authorized_keys file with no trailing newline, before the script-level fix
// below), the parser loses the "port=" line entirely — it was never observed.
// The bug this test pins is what parseAttachPreflight does with that: it must
// report the LOUD, distinct portUnobserved state, never the misleading
// portNoTool ("no ss or netstat on the box") that a two-state boolean scheme
// collapsed it into. A user who had ss and got told "no tool" acted on a false
// statement about their own box; portUnobserved says only what is true — aq
// could not tell.
func TestParseAttachPreflightUnobservedPortStateNeverCollapsesToNoTool(t *testing.T) {
	raw := fullSurvey +
		"ak=readable\n" +
		"ak_begin\n" +
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAA alice@laptopak_end\n" + // no newline before ak_end
		"port=busy\n" +
		"preflight_ok=1\n"

	pre, err := parseAttachPreflight([]byte(raw))
	if err != nil {
		t.Fatalf("parseAttachPreflight: %v", err)
	}
	if pre.port == portNoTool {
		t.Fatalf("an unobserved port line must never collapse into portNoTool (\"no ss or netstat\") — that is a false claim about the box")
	}
	if pre.port != portUnobserved {
		t.Fatalf("got %v, want portUnobserved", pre.port)
	}
}

// End to end. Runs the actual script `attachPreflightScript` produces through
// a real shell against a real file on disk lacking a trailing newline — the
// exact shape seen on the live boxes this fix was written against — with a
// stub `ss` on PATH standing in for the box's real one (so the test result
// does not depend on what happens to be listening on the machine running it).
// Before the attachPreflightScript / parseAttachPreflight fix this fails with
// portUnobserved/portNoTool instead of portBusy.
func TestAttachPreflightScriptSurvivesAnAuthorizedKeysWithoutTrailingNewline(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on PATH")
	}

	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatal(err)
	}
	// No trailing newline — matches what cloud-init/terraform commonly write.
	akContent := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAA alice@laptop"
	if err := os.WriteFile(filepath.Join(sshDir, "authorized_keys"), []byte(akContent), 0600); err != nil {
		t.Fatal(err)
	}

	// A fake `ss` that deterministically reports the port busy, so the test
	// asserts something regardless of what is actually listening on the runner.
	bin := t.TempDir()
	ssStub := "#!/bin/sh\necho 'LISTEN 0 128 *:8443 *:*'\n"
	ssPath := filepath.Join(bin, "ss")
	if err := os.WriteFile(ssPath, []byte(ssStub), 0755); err != nil {
		t.Fatal(err)
	}

	script := attachPreflightScript(filepath.Join(home, "workspace"), 8443)
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running attachPreflightScript: %v\n%s", err, out)
	}

	pre, err := parseAttachPreflight(out)
	if err != nil {
		t.Fatalf("parseAttachPreflight: %v\nraw output:\n%s", err, out)
	}
	if pre.port != portBusy {
		t.Fatalf("authorized_keys without a trailing newline swallowed the port state: got %v, want portBusy\nraw output:\n%s", pre.port, out)
	}
}

// The summary tells the user their keys are safe without printing them. Their
// keys are theirs and do not belong in our terminal output.
func TestAuthorizedKeysSummaryCountsWithoutEchoing(t *testing.T) {
	got := authorizedKeysSummary(customerKeys)
	if !strings.Contains(got, "3 existing key") {
		t.Errorf("summary = %q, want a count of 3", got)
	}
	if strings.Contains(got, "alice@laptop") {
		t.Errorf("summary must not echo the keys: %q", got)
	}
	if got := authorizedKeysSummary(""); !strings.Contains(got, "no existing keys") {
		t.Errorf("empty summary = %q", got)
	}
}

// applyMarkerRegion is the only way aq edits a file on a box it does not own.
// Merge, never replace: every byte outside the markers survives.
func TestApplyMarkerRegionLeavesForeignContentByteIdentical(t *testing.T) {
	merged, err := applyMarkerRegion(customerKeys, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIZZZ aquanode\n")
	if err != nil {
		t.Fatalf("applyMarkerRegion: %v", err)
	}
	if !strings.HasSuffix(merged, customerKeys) {
		t.Fatalf("the customer's file must survive byte for byte, got:\n%q", merged)
	}

	// Re-applying replaces only our own region, and removing it restores the
	// original exactly — which is what `aq release` has to be able to do.
	twice, err := applyMarkerRegion(merged, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIYYY aquanode\n")
	if err != nil {
		t.Fatalf("applyMarkerRegion (second pass): %v", err)
	}
	if strings.Contains(twice, "AAAAIZZZ") {
		t.Error("the second pass should have replaced our own region")
	}
	if !strings.HasSuffix(twice, customerKeys) {
		t.Fatalf("the customer's file drifted on the second pass:\n%q", twice)
	}
	if strings.Count(twice, beginMarker) != 1 {
		t.Errorf("expected exactly one marker region, got:\n%s", twice)
	}
}

// A damaged marker pair is refused rather than repaired: guessing at the
// boundary of a region we are about to overwrite is how you delete somebody's
// only access to a box.
func TestApplyMarkerRegionRefusesDamagedMarkers(t *testing.T) {
	for _, broken := range []string{
		beginMarker + "\nkey\n",
		"key\n" + endMarker + "\n",
		endMarker + "\nkey\n" + beginMarker + "\n",
		beginMarker + "\n" + beginMarker + "\nkey\n" + endMarker + "\n",
	} {
		if _, err := applyMarkerRegion(broken, "x\n"); err == nil {
			t.Errorf("expected a refusal for:\n%s", broken)
		}
	}
}

// --dry-run must create nothing in Aquanode and write nothing on the box, and
// must state the one-box-one-setup ceiling before the user spends anything on
// the assumption it can be split.
func TestAttachDryRunCreatesNothingAndStatesTheLimit(t *testing.T) {
	detachedSandbox(t, testHost())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("dry run called the API: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	run, seen := stubRunner(t, map[string]string{
		"preflight_ok": attachPreflightOutput("readable", customerKeys, "free"),
		"ogre status":  `{"gpu":[]}`,
	})

	var out bytes.Buffer
	err := runAttach(attachOptions{
		alias:  "lease-a",
		dryRun: true,
		out:    &out,
		errOut: &bytes.Buffer{},
		client: api.NewAuthed(srv.URL, "aq_sk_test", "team-1"),
		run:    run,
	})
	if err != nil {
		t.Fatalf("runAttach --dry-run: %v", err)
	}

	text := out.String()
	for _, want := range []string{
		"one box is one deployment running one setup",
		"cannot",
		"partition",
		"# BEGIN aquanode",
		"bill nothing",
		"--dry-run: nothing was written on the box",
	} {
		if !strings.Contains(strings.ToLower(text), strings.ToLower(want)) {
			t.Errorf("attach --dry-run output is missing %q:\n%s", want, text)
		}
	}
	for _, cmd := range *seen {
		for _, forbidden := range []string{"mv -f", "nohup", "chmod 0600"} {
			if strings.Contains(cmd, forbidden) {
				t.Errorf("dry run issued a writing command %q", forbidden)
			}
		}
	}
	if hosts, _ := config.LoadHosts(); hosts[0].Attached() {
		t.Fatal("dry run recorded the box as attached")
	}
}

// The probe is the gate. A failure must be loud, must surface the
// orchestrator's own reason verbatim, and must leave nothing recorded as
// reachable.
func TestAttachFailsLoudlyWhenTheProbeFails(t *testing.T) {
	detachedSandbox(t, testHost())
	const reason = "dial tcp 1.2.3.4:8443: i/o timeout"

	mux := http.NewServeMux()
	mux.HandleFunc("/deployments/external", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"deploymentId": 4242, "installToken": "tok-1", "ogrePort": 8443})
	})
	mux.HandleFunc("/deployments/external/4242/install-config", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok-1" {
			t.Errorf("install-config must authenticate with the install token, got %q", got)
		}
		if r.Header.Get("x-api-key") != "" {
			t.Error("install-config must not carry the user's API key")
		}
		writeData(w, map[string]any{
			"ogreJwtSecret": "jwt-secret", "ogreProxyPassword": "proxy-pass",
			"tlsCertPem": "-----BEGIN CERTIFICATE-----\nAAA\n-----END CERTIFICATE-----\n",
			"tlsKeyPem":  "-----BEGIN PRIVATE KEY-----\nBBB\n-----END PRIVATE KEY-----\n",
			// A NUMBER, like every other deploymentId the orchestrator emits —
			// this fixture said "4242" and that quoted string is the whole
			// reason the type error survived to a live box.
			"ogrePort": 8443, "orchestratorUrl": "https://server.example", "deploymentId": 4242,
		})
	})
	mux.HandleFunc("/deployments/external/4242/activate", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": false,
			"error":   "unreachable",
			"data":    map[string]any{"error": "unreachable", "reason": reason},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var wrote []string
	run := func(_ config.Host, remote string) ([]byte, error) {
		wrote = append(wrote, remote)
		switch {
		case strings.Contains(remote, "preflight_ok"):
			return []byte(attachPreflightOutput("readable", customerKeys, "free")), nil
		case strings.Contains(remote, "__AQ_ABSENT__"):
			return []byte("__AQ_ABSENT__\n"), nil
		default:
			return []byte(""), nil
		}
	}

	var out bytes.Buffer
	err := runAttach(attachOptions{
		alias: "lease-a", yes: true, out: &out, errOut: &bytes.Buffer{},
		client: api.NewAuthed(srv.URL, "aq_sk_test", "team-1"),
		run:    run,
	})
	if err == nil {
		t.Fatal("a failed probe must fail the command")
	}
	if !strings.Contains(err.Error(), reason) {
		t.Errorf("the probe's reason must be surfaced verbatim; got:\n%v", err)
	}
	if !strings.Contains(err.Error(), "detached mode") {
		t.Errorf("the failure should point at detached mode as the complete alternative; got:\n%v", err)
	}
	// A port-mapped box (simplepod, vast.ai and similar) can never satisfy
	// attach's listen-port==dial-port requirement, and a probe failure there
	// looked identical to any other unreachable box — nothing told the user
	// why. The refusal must name the port-mapped case explicitly rather than
	// leaving them to guess from a bare timeout.
	if !strings.Contains(err.Error(), "container-pool") || !strings.Contains(err.Error(), "SAME port") {
		t.Errorf("the failure must explain the port-mapped/container-pool limitation; got:\n%v", err)
	}
	hosts, _ := config.LoadHosts()
	if hosts[0].Attached() {
		t.Fatal("a box whose probe failed must never be recorded as attached")
	}
	// The refusal above tells the user to run `aq release lease-a`. That
	// command only works if something local marks the row as releasable —
	// without it, `aq release` answers "not attached, nothing to release" and
	// the orchestrator's PROVISIONING row is stranded with no CLI path to
	// clear it.
	if !hosts[0].Releasable() {
		t.Fatal("a refused attach must leave the host releasable — the refusal's own `aq release` command must work")
	}
	if hosts[0].ReleaseTargetID() != 4242 {
		t.Fatalf("release target = %d, want 4242 (the deployment AdoptExternal created)", hosts[0].ReleaseTargetID())
	}
}

func TestAttachRecordsTheBoxOnlyAfterASuccessfulProbe(t *testing.T) {
	detachedSandbox(t, testHost())

	var envWritten string
	mux := http.NewServeMux()
	mux.HandleFunc("/deployments/external", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["host"] != "1.2.3.4" {
			t.Errorf("adopt sent host=%v, want the box's ssh host", body["host"])
		}
		if body["ogrePort"] != float64(defaultOgrePort) {
			t.Errorf("adopt sent ogrePort=%v, want %d", body["ogrePort"], defaultOgrePort)
		}
		writeData(w, map[string]any{"deploymentId": 4242, "installToken": "tok-1", "ogrePort": 8443})
	})
	mux.HandleFunc("/deployments/external/4242/install-config", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{
			"ogreJwtSecret": "jwt-secret", "ogreProxyPassword": "proxy-pass",
			"tlsCertPem": "CERTPEM", "tlsKeyPem": "KEYPEM",
			"ogrePort": 8443, "orchestratorUrl": "https://server.example", "deploymentId": 4242,
		})
	})
	mux.HandleFunc("/deployments/external/4242/activate", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"status": "ACTIVE", "agentLastSeenAt": "2026-08-27T10:00:00Z"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	run := func(_ config.Host, remote string) ([]byte, error) {
		switch {
		case strings.Contains(remote, "preflight_ok"):
			return []byte(attachPreflightOutput("readable", customerKeys, "free")), nil
		case strings.Contains(remote, "__AQ_ABSENT__"):
			return []byte("__AQ_ABSENT__\n"), nil
		case strings.Contains(remote, ogreEnvHeredocTag):
			envWritten = remote
			return nil, nil
		default:
			return nil, nil
		}
	}

	var out bytes.Buffer
	err := runAttach(attachOptions{
		alias: "lease-a", yes: true, out: &out, errOut: &bytes.Buffer{},
		client: api.NewAuthed(srv.URL, "aq_sk_test", "team-1"),
		run:    run,
	})
	if err != nil {
		t.Fatalf("runAttach: %v", err)
	}

	hosts, err := config.LoadHosts()
	if err != nil {
		t.Fatal(err)
	}
	if !hosts[0].Attached() || hosts[0].DeploymentID != 4242 {
		t.Fatalf("the box was not recorded as attached: %+v", hosts[0])
	}
	if hosts[0].PublicHost != "1.2.3.4" || hosts[0].OgrePort != 8443 {
		t.Fatalf("attach state recorded wrong: %+v", hosts[0])
	}

	// The env file is written inside our markers, base64-encodes the PEM
	// material, and never lands as a partial file under the real name.
	for _, want := range []string{
		beginMarker,
		endMarker,
		"JWT_SECRET=",
		"OGRE_TLS_CERT='" + base64.StdEncoding.EncodeToString([]byte("CERTPEM")) + "'",
		"mv -f",
	} {
		if !strings.Contains(envWritten, want) {
			t.Errorf("the env write is missing %q:\n%s", want, envWritten)
		}
	}
	if strings.Contains(envWritten, "CERTPEM\n-----") {
		t.Error("PEM material must be base64-encoded, not embedded raw")
	}

	text := out.String()
	for _, want := range []string{"bills nothing", "aq release lease-a", "no provider is ever contacted"} {
		if !strings.Contains(text, want) {
			t.Errorf("the success message is missing %q:\n%s", want, text)
		}
	}
}

// A box already attached must not be adopted twice — the second row would be a
// duplicate the user has no way to tell apart.
func TestAttachRefusesAnAlreadyAttachedBox(t *testing.T) {
	h := testHost()
	h.DeploymentID = 4242
	h.AttachedAt = "2026-08-27T00:00:00Z"
	detachedSandbox(t, h)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("must not call the API: %s", r.URL.Path)
	}))
	defer srv.Close()

	err := runAttach(attachOptions{
		alias: "lease-a", yes: true, out: &bytes.Buffer{}, errOut: &bytes.Buffer{},
		client: api.NewAuthed(srv.URL, "k", "t"),
		run: func(config.Host, string) ([]byte, error) {
			t.Fatal("must not reach the box")
			return nil, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "aq release") {
		t.Fatalf("expected a refusal naming `aq release`, got %v", err)
	}
}

// configureOgreOnBox must refuse a credential file it could not read rather
// than replacing something it did not write.
func TestConfigureOgreRefusesAnUnreadableEnvFile(t *testing.T) {
	detachedSandbox(t, testHost())
	opts := attachOptions{out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}.withDefaults()
	opts.run = func(config.Host, string) ([]byte, error) { return []byte("__AQ_UNREADABLE__\n"), nil }
	err := configureOgreOnBox(opts, testHost(), &api.ExternalInstallConfig{OgreJWTSecret: "s"}, 4242, 8443)
	if err == nil || !strings.Contains(err.Error(), "cannot read") {
		t.Fatalf("expected a refusal on an unreadable env file, got %v", err)
	}
}

// The restart script is handed to the remote shell as ONE argv element, so that
// shell's own /proc/<pid>/cmdline contains the whole script. A literal
// `ogre -port 8443` in it makes `pkill -f` match the session running the attach
// — observed live on 2026-08-27: ssh died with status 255 before ogre started,
// deterministically, on a real box. `pgrep -f` had the mirror bug and reported
// the daemon alive by matching that same shell.
func TestRestartOgreScriptNeverSpellsTheMatchPatternLiterally(t *testing.T) {
	script := restartOgreScript(8443)

	if strings.Contains(script, "ogre -port 8443") {
		t.Fatalf("script contains the literal match pattern, so pkill/pgrep -f will match the shell running it:\n%s", script)
	}
	if !strings.Contains(script, "AQ_OGRE_PORT=8443") {
		t.Fatalf("port must reach the box as a variable:\n%s", script)
	}
	// Self-exclusion must be explicit, not an accident of spelling. It now lives
	// in ONE place (the aq_pids helper every enumeration goes through) so the
	// assertion is that the helper carries it and that nothing enumerates
	// matching pids without it.
	if !strings.Contains(script, `[ "$pid" = "$$" ] && continue`) {
		t.Fatalf("the pid enumeration must skip $$:\n%s", script)
	}
	if strings.Count(script, "pgrep -f --") != 1 {
		t.Fatalf("every pid lookup must go through the one self-excluding helper:\n%s", script)
	}
	if strings.Contains(script, "pkill") {
		t.Fatalf("pkill cannot exclude the caller; use the explicit pgrep loop:\n%s", script)
	}
	// The daemon still has to be started with the port it was told.
	if !strings.Contains(script, `nohup ogre -port "$AQ_OGRE_PORT"`) {
		t.Fatalf("daemon must still be launched on the requested port:\n%s", script)
	}
}

// The restart must not put a new daemon into the port before the old one has
// let go of it. ogre refuses to start a second instance on a bound port (its
// guard is correct and stays), so a SIGTERM-then-immediately-start left the old
// daemon dead, the new one refused, and NOTHING listening. Observed on a real
// re-attach, with the deployment row stranded in PROVISIONING.
func TestRestartOgreScriptWaitsForTheOldDaemonBeforeBinding(t *testing.T) {
	script := restartOgreScript(8444)

	kill := strings.Index(script, "kill \"$pid\"")
	wait := strings.Index(script, `while [ "$AQ_WAIT" -lt 30 ]`)
	start := strings.Index(script, "nohup ogre -port")
	if kill < 0 || wait < 0 || start < 0 {
		t.Fatalf("script is missing the kill / wait / start steps:\n%s", script)
	}
	if !(kill < wait && wait < start) {
		t.Fatalf("the wait must sit between the kill and the start:\n%s", script)
	}

	// Refuse rather than race: if the old daemon will not go, starting anyway
	// only feeds the second-instance guard.
	refusal := strings.Index(script, "refusing to start a second one")
	if refusal < 0 || refusal > start {
		t.Fatalf("script must refuse to start while the port is still held:\n%s", script)
	}

	// A fractional sleep is a GNU extension busybox rejects, which would turn
	// the whole wait into a no-op on a minimal image.
	if strings.Contains(script, "sleep 0.") {
		t.Fatalf("wait must use whole-second sleeps:\n%s", script)
	}
}

// The liveness check must not accept a pid that was already running. A daemon
// still winding down matches the same pattern, so counting matches would report
// a green restart for a new process that never came up.
func TestRestartOgreScriptRejectsAnOldPidAsProofOfLife(t *testing.T) {
	script := restartOgreScript(8444)

	if !strings.Contains(script, "AQ_OGRE_OLD=") {
		t.Fatalf("script must record the pre-start pid set:\n%s", script)
	}
	if !strings.Contains(script, `case " $AQ_OGRE_OLD " in *" $pid "*) continue ;; esac`) {
		t.Fatalf("liveness must skip pids that were already running:\n%s", script)
	}
}

// Release stops what attach started, and asserts the OUTCOME rather than the
// commands: a kill returning 0 is not a process that exited.
func TestReleaseOgreScriptWaitsAndProvesTheDaemonIsGone(t *testing.T) {
	script := releaseOgreScript(8444)

	if strings.Contains(script, "ogre -port 8444") {
		t.Fatalf("script contains the literal match pattern, so it would match the shell running it:\n%s", script)
	}
	if !strings.Contains(script, `while [ "$AQ_WAIT" -lt 30 ]`) {
		t.Fatalf("release must wait for the daemon to exit:\n%s", script)
	}
	if !strings.Contains(script, "rm -f '"+ogreEnvPath+"'") {
		t.Fatalf("release must remove the credential file aq wrote:\n%s", script)
	}
	okMarker := strings.Index(script, "echo release_ok=1")
	stillRunning := strings.Index(script, "is still running")
	if okMarker < 0 || stillRunning < 0 || stillRunning > okMarker {
		t.Fatalf("the success marker must be unreachable while a daemon survives:\n%s", script)
	}
}

// The control port must not be the port ogre's own terminal proxy binds. That
// number is a constant inside ogre, not something we hand it, so a control API
// sitting there means the box can never have a browser terminal, and ogre
// decides "a proxy is already running" from a bare TCP connect, so it would
// report started=false pointing at the control API and the console would
// advertise a terminal that is not there (#878).
func TestDefaultOgrePortDoesNotCollideWithTheTerminalProxy(t *testing.T) {
	if defaultOgrePort == terminalProxyPort {
		t.Fatalf("the attach default (%d) is the terminal proxy's port; both features cannot share it", defaultOgrePort)
	}
	if terminalProxyPort != 8443 {
		t.Fatalf("terminalProxyPort must track ogre's internal/proxy.DefaultListenPort (8443), got %d", terminalProxyPort)
	}
}

// Choosing the proxy's port explicitly is allowed (a box already attached that
// way must keep working) but it costs the terminal, and the user has to be
// told before anything is written.
func TestAttachPlanWarnsWhenTheChosenPortEatsTheTerminal(t *testing.T) {
	var out bytes.Buffer
	printAttachPlan(&out, testHost(), attachPreflight{port: portFree}, "1.2.3.4", terminalProxyPort, "/workspace")

	text := out.String()
	if !strings.Contains(text, "no browser terminal") {
		t.Errorf("the plan must say the terminal is lost on this port:\n%s", text)
	}
	if !strings.Contains(text, "--ogre-port 8444") {
		t.Errorf("the plan must name the port that keeps both:\n%s", text)
	}
}

// The terminal is reached on its own port. Attach is the only moment the user
// is being told which ports to open, so it has to be named there rather than
// discovered later as a dead tab.
func TestAttachPlanNamesTheTerminalPort(t *testing.T) {
	var out bytes.Buffer
	printAttachPlan(&out, testHost(), attachPreflight{port: portFree}, "1.2.3.4", defaultOgrePort, "/workspace")

	if !strings.Contains(out.String(), "TCP 8443 must also be reachable") {
		t.Errorf("the plan must name the terminal's own port:\n%s", out.String())
	}
}

// An unavailable terminal with no reason is UNKNOWN: an orchestrator that
// predates the field answers exactly that way, and inventing a cause for it is
// worse than saying we do not have one.
func TestAttachReportsAnUnexplainedTerminalAsUnknown(t *testing.T) {
	var out bytes.Buffer
	printTerminalVerdict(&out, &api.ActivateExternalResult{TerminalAvailable: false}, defaultOgrePort)

	if !strings.Contains(out.String(), "reported no cause") {
		t.Errorf("an absent reason must render as unknown:\n%s", out.String())
	}
}

// Each machine-readable cause gets its own actionable line. The whole point of
// the closed enum is that the user is told what to DO.
func TestAttachRendersEachTerminalReasonDistinctly(t *testing.T) {
	cases := map[string]string{
		"AGENT_PORT_CONFLICT": "--ogre-port 8444",
		"PROXY_UNREACHABLE":   "reachable from the internet",
		"PROXY_START_FAILED":  ogreLogPath,
	}
	for reason, want := range cases {
		var out bytes.Buffer
		printTerminalVerdict(&out, &api.ActivateExternalResult{
			TerminalAvailable:         false,
			TerminalUnavailableReason: reason,
		}, terminalProxyPort)
		if !strings.Contains(out.String(), want) {
			t.Errorf("%s must tell the user %q:\n%s", reason, want, out.String())
		}
	}
}
