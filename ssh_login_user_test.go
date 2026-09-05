package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Aquanodeio/aq/internal/api"
)

// A managed hyperstack box refuses root outright: cloud-init's disable_root
// behaviour puts the platform's key on `ubuntu` and leaves root answering with
// a forced-command banner. Four fresh boxes (deployments 3513, 3519, 3521,
// 3528) failed that way while aq wrote `User root` for every one of them.
//
// The generated stanza must carry whatever login user the platform recorded
// for the box, not an assumed root.
func TestEntriesForCarriesTheRecordedLoginUser(t *testing.T) {
	dep := api.Deployment{
		ID:           3528,
		Name:         "box",
		AppURL:       "http://203.0.113.9:22",
		SSHLoginUser: "ubuntu",
	}

	entries := entriesFor(dep, "/k", "/kh")
	if len(entries) == 0 {
		t.Fatal("expected stanzas")
	}
	for _, e := range entries {
		if e.User != "ubuntu" {
			t.Errorf("%s: User = %q, want %q", e.Alias, e.User, "ubuntu")
		}
	}

	// The rendered file is what ssh actually reads — assert there, not only on
	// the struct, since renderStanzas is where the root fallback lives.
	got := renderManagedConfig(entries)
	if !strings.Contains(got, "User ubuntu") {
		t.Errorf("rendered config must carry `User ubuntu`; got:\n%s", got)
	}
	if strings.Contains(got, "User root") {
		t.Errorf("rendered config must not fall back to root when a login user was recorded; got:\n%s", got)
	}
}

// No recorded login user is UNKNOWN, not evidence for root. aq still renders
// root — it is correct on every Docker-pool provider and on every box already
// running when this shipped, and omitting the User line would make ssh try the
// LOCAL account name, which is right nowhere — but it must never do so
// silently. `warnUnknownLoginUser` is the "silently" half of the fix.
func TestUnknownLoginUserFallsBackToRootAndSaysSo(t *testing.T) {
	dep := api.Deployment{ID: 4001, Name: "box", AppURL: "http://203.0.113.9:22"}

	if user, known := loginUserFor(dep); known || user != "" {
		t.Fatalf("loginUserFor with no recorded user = (%q, %v), want (\"\", false)", user, known)
	}

	got := renderManagedConfig(entriesFor(dep, "/k", "/kh"))
	if !strings.Contains(got, "User root") {
		t.Errorf("unknown login user should still render the root fallback; got:\n%s", got)
	}

	var errOut bytes.Buffer
	warnUnknownLoginUser(&errOut, dep)
	msg := errOut.String()
	// The ticket's own words: a named "login user unknown" message, not a
	// generic warning the user has to interpret.
	for _, want := range []string{"login user unknown", "#4001", "root", "-user"} {
		if !strings.Contains(msg, want) {
			t.Errorf("warning must mention %q; got:\n%s", want, msg)
		}
	}
	// It must name a command the user can actually run to recover, addressing
	// the box by the same handle `aq up` told them to use.
	if !strings.Contains(msg, "aq ssh -user <name> box") {
		t.Errorf("warning must name the recovery command for this box; got:\n%s", msg)
	}
}

// The mirror: a box whose login user IS recorded gets no warning at all.
// Printing one on every connection would train the user to ignore it, which
// would put us back where we started on the boxes that need it.
func TestKnownLoginUserWarnsNothing(t *testing.T) {
	dep := api.Deployment{ID: 3528, Name: "box", AppURL: "http://203.0.113.9:22", SSHLoginUser: "ubuntu"}

	var errOut bytes.Buffer
	warnUnknownLoginUser(&errOut, dep)
	if errOut.Len() != 0 {
		t.Errorf("a recorded login user must produce no warning; got:\n%s", errOut.String())
	}
	if note := loginUserNote(dep); note != "" {
		t.Errorf("a recorded login user must produce no `aq up` note; got %q", note)
	}
}

// `aq up` prints `SSH: aq ssh <name>`, which is a promise the command works on
// the first try. When the login user is unknown that promise is unbacked, so
// the same block has to say so — the ticket's complaint was that nothing in
// aq up's output hinted at it.
func TestPrintConnectionNotesAnUnknownLoginUser(t *testing.T) {
	var out bytes.Buffer
	printConnection(&out, api.Deployment{ID: 4001, Name: "box", AppURL: "http://203.0.113.9:22"})
	if !strings.Contains(out.String(), "login user unknown") {
		t.Errorf("aq up output must note an unknown login user; got:\n%s", out.String())
	}

	out.Reset()
	printConnection(&out, api.Deployment{ID: 3528, Name: "box", AppURL: "http://203.0.113.9:22", SSHLoginUser: "ubuntu"})
	if strings.Contains(out.String(), "login user unknown") {
		t.Errorf("aq up output must stay quiet when the login user is recorded; got:\n%s", out.String())
	}
}

// The field has to survive the wire, not just the struct. A backend that does
// not send `ssh_login_user` at all decodes to the empty string, which is the
// same UNKNOWN as an explicit empty one — both are handled, neither is root.
func TestDeploymentDecodesSSHLoginUserFromTheWire(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"recorded", `{"id":3528,"ssh_login_user":"ubuntu"}`, "ubuntu"},
		{"declared empty", `{"id":3528,"ssh_login_user":""}`, ""},
		{"field absent (older backend)", `{"id":3528}`, ""},
		{"null", `{"id":3528,"ssh_login_user":null}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var dep api.Deployment
			if err := decodeDeploymentJSON(c.body, &dep); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if dep.SSHLoginUser != c.want {
				t.Errorf("SSHLoginUser = %q, want %q", dep.SSHLoginUser, c.want)
			}
		})
	}
}

func decodeDeploymentJSON(body string, dep *api.Deployment) error {
	return json.Unmarshal([]byte(body), dep)
}
