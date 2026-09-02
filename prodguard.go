package main

import (
	"fmt"
	"io"
	"net/url"
	"strings"
)

// This file is the one place `aq` decides "which host am I about to change
// something on, and am I allowed to". It exists because the CLI's defaults all
// point at production: AQ_API_URL is unset in almost every shell, the stored
// credential from `aq login` carries production's URL, and DefaultAPIURL is
// production too. Nothing about running the binary out of a scratch checkout,
// a container, or a script changes any of that — so a process that never meant
// to touch the real account reaches it by doing nothing at all, and the first
// evidence is a rented GPU on the bill.
//
// Two always-on rails, both keyed off the command name in main's dispatch:
//
//  1. every command that changes something announces the host it resolved, so
//     "which stack did that actually hit" is answerable from the output rather
//     than by re-deriving the precedence rules in resolveAPIURL; and
//  2. the commands that rent hardware refuse to do it against a non-local host
//     when nobody is at the keyboard, unless the caller says so explicitly.
//
// Rail 2 is deliberately scoped to non-interactive stdin. A person typing
// `aq up` has a terminal, sees the banner from rail 1, and is unaffected —
// guarding them would just train them to pass the override reflexively, which
// is how a guard stops being one. A script, a CI job or an agent has no
// terminal and no judgment, and is exactly the caller that inherits production
// by accident.

// billableCommands maps a verb to a short description of the money it can
// start spending. Membership here is the definition of "billable" for rail 2 —
// a new verb that rents hardware belongs in this map, and the test that pins
// it against main's dispatch will not tell you that, so this comment is the
// contract: if it can cause a box to be leased, it goes here.
//
// Only these three reach a lease. `up` and `deploy` do it unconditionally
// (api.Client.Up / api.Client.Deploy). `import` does it on the path that
// installs and runs the captured setup on a fresh box. Everything else in the
// dispatch either reads, edits metadata, or de-provisions — `down` and `pause`
// stop spend rather than start it, so guarding them would be backwards: it
// would make the safe action harder than the expensive one.
var billableCommands = map[string]string{
	"up":     "rent a GPU box",
	"deploy": "rent a GPU box to restore a save onto",
	"import": "launch the imported setup onto a rented box",
}

// nonMutatingCommands are the verbs that change nothing on the account, so
// rail 1 stays quiet for them. The set is an allowlist rather than a denylist
// on purpose: a verb added to main's dispatch and forgotten here announces its
// target host, which is noise at worst. The reverse default would let a new
// mutating verb ship silent, which is the failure this file exists to stop.
//
// `host` and `logout` are listed because they touch only local files — naming
// an API host in their output would claim a request that never happens.
var nonMutatingCommands = map[string]bool{
	"version": true, "--version": true, "-v": true,
	"help": true, "--help": true, "-h": true,
	"gpus":   true,
	"ls":     true,
	"status": true,
	"logs":   true,
	"setups": true,
	"whoami": true,
	// `calls` is GONE — it was a top-level verb and is now `aq job runs`.
	// `job` is deliberately NOT listed in its place: this allowlist keys on the
	// top-level verb, and `job` covers create/rm/run/cancel, which all mutate.
	// Listing it to keep `aq job runs` quiet would silence the announcement for
	// `aq job rm` too, and this file exists to stop exactly that. The cost is
	// one extra host announcement on a read; the alternative is a silent
	// destructive verb, which is not a trade worth making.
	"ssh":    true,
	"host":   true,
	"logout": true,
}

// isLocalTarget reports whether an API base URL points at a stack running on
// this machine.
//
// Default-deny: anything this cannot positively identify as loopback counts as
// remote, including a URL that fails to parse. The question rail 2 asks is
// "may I spend money here", and the safe answer to "I could not tell" is no.
// That also means a typo'd or unfamiliar host is guarded exactly like
// production, which is the behaviour you want — the accident this file guards
// is never "I meant a different remote host", it is "I did not realise I was
// pointed at one at all".
func isLocalTarget(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	switch host {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	// A host under .localhost is reserved for loopback by RFC 6761, and
	// per-worktree stacks are a natural place to use one.
	return strings.HasSuffix(host, ".localhost")
}

// announceTarget implements rail 1: print the resolved API base for any
// command that changes something. Writes to errOut so it never contaminates
// stdout, which callers parse.
func announceTarget(cmd, apiURL string, errOut io.Writer) {
	if nonMutatingCommands[cmd] {
		return
	}
	fmt.Fprintf(errOut, "aq: api %s\n", apiURL)
}

// guardBillable implements rail 2. It returns a non-nil error — which main
// turns into a non-zero exit before the command runs at all — when a billable
// verb is about to rent hardware on a non-local host from a process with no
// terminal and no explicit opt-in.
//
// Every input is a parameter rather than read from the environment inside so
// the decision table is testable without a terminal, a credential file or a
// live host.
func guardBillable(cmd, apiURL string, args []string, allowProd, interactive bool) error {
	what, billable := billableCommands[cmd]
	if !billable {
		return nil
	}
	// Asking a command what it does must never be the thing that gets refused.
	// `aq up --help` rents nothing, and a guard that answers a question about
	// usage with a wall of refusal is how people learn to route around it.
	if wantsHelp(args) {
		return nil
	}
	if isLocalTarget(apiURL) {
		return nil
	}
	if allowProd || interactive {
		return nil
	}
	return fmt.Errorf(`refusing to %s: `+"`aq %s`"+` targets %s, which is not a local
stack, and nothing here looks like a person who could stop it.

Nothing about this shell asked for that host: an unset AQ_API_URL, or the URL
stored by `+"`aq login`"+`, is enough to reach it, so a script or an automated tool
lands on the real account by default and the first sign is a real bill.

To do it anyway:      re-run with --prod, or set AQ_ALLOW_PROD=1
To use a local stack: set AQ_API_URL=http://localhost:<port>/api/v1`,
		what, cmd, apiURL)
}

// hasHumanOversight reports whether someone could plausibly notice and object
// to what is about to be charged.
//
// stdin being a terminal is the main signal, but it is not sufficient on its
// own: an automated tool launched from an interactive shell inherits that
// terminal, so it looks like a person while behaving like a script — which is
// precisely the caller that rented a box nobody asked for. So a harness that
// announces itself in the environment counts as unattended regardless of what
// stdin looks like. CI is the near-universal convention; the rest name the
// agent runners common enough to be worth recognising by name.
//
// Every one of these is a positive signal of automation. There is no signal
// that positively proves a human, so this stays a best-effort narrowing of the
// exemption rather than the thing the guard rests on — the refusal it gates is
// a one-flag override, not a wall.
func hasHumanOversight(getenv func(string) string, interactiveStdin bool) bool {
	for _, k := range []string{"CI", "CONTINUOUS_INTEGRATION", "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT"} {
		if getenv(k) != "" {
			return false
		}
	}
	return interactiveStdin
}

// stripProdFlag removes the global --prod opt-in from a billable command's
// arguments and reports whether it was present, so the command's own flag set
// never sees a flag it does not define.
//
// It stops at a literal `--`. `aq run` and friends forward everything past that
// separator to a command running on the box, and eating a `--prod` out of a
// user's remote invocation would be a silent corruption of their argv. Callers
// only apply this to billable verbs, so the blast radius is three commands, but
// the separator rule is what makes it safe to widen later.
func stripProdFlag(args []string) ([]string, bool) {
	out := make([]string, 0, len(args))
	found := false
	for i, a := range args {
		if a == "--" {
			out = append(out, args[i:]...)
			break
		}
		if a == "--prod" || a == "-prod" || a == "--prod=true" || a == "-prod=true" {
			found = true
			continue
		}
		out = append(out, a)
	}
	return out, found
}

// wantsHelp reports whether args ask the command to describe itself rather than
// run. Stops at `--`, for the same reason stripProdFlag does: a `--help` past
// the separator belongs to a command running on the box.
func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "-h" || a == "--help" || a == "-help" {
			return true
		}
	}
	return false
}
