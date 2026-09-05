package main

import (
	"fmt"
	"io"

	"github.com/Aquanodeio/aq/internal/api"
)

// The login user for a managed box is a per-deployment fact the platform now
// records (`deployments.ssh_login_user`, from the provider adapter's own
// capability declaration in mjolnir). aq reads it rather than assuming.
//
// It is THREE-state. `loginUserFor` returns `known == false` for three
// different situations that all mean the same thing to the user — no login
// user was recorded for this box:
//
//   - the deployment predates the column,
//   - its provider has not established which user its key injection lands on,
//   - the backend is too old to send the field.
//
// None of those is a licence to answer "root". Root is only ever a last-resort
// fallback, and when it is used it is announced: see `warnUnknownLoginUser`.

// loginUserFor returns the login user recorded for a deployment, and whether
// one was recorded at all.
func loginUserFor(dep api.Deployment) (user string, known bool) {
	if dep.SSHLoginUser == "" {
		return "", false
	}
	return dep.SSHLoginUser, true
}

// warnUnknownLoginUser prints the named "login user unknown" message for a
// deployment with no recorded login user, and says nothing at all when one is
// recorded.
//
// It writes to errOut, so `aq ssh --print` and any piped stdout stay clean.
//
// It is a warning rather than a refusal on purpose. `aq` still falls back to
// root, because root is right on every Docker-pool provider and on every box
// already running when this shipped, and silently dropping the `User` line
// would make ssh try the LOCAL account name, which is right nowhere. The bug
// being fixed is not that root is used, it is that root was used SILENTLY: a
// user whose connection is refused could not tell from anything aq printed
// that a different login user was the answer.
func warnUnknownLoginUser(errOut io.Writer, dep api.Deployment) {
	if errOut == nil {
		return
	}
	if _, known := loginUserFor(dep); known {
		return
	}
	fmt.Fprintf(errOut, "warning: login user unknown for deployment #%d — "+
		"no login user was recorded for this box, so aq is assuming %q.\n", dep.ID, sshUser)
	fmt.Fprintf(errOut, "         If the connection is refused (some providers put your key on "+
		"`ubuntu` instead), retry with `aq ssh -user <name> %s`.\n", sshTarget(dep))
}

// loginUserNote is the one-line form of the same fact for `aq up`'s post-create
// output, where a two-line stderr warning would crowd out the box's details.
// Empty when the login user is known, so the caller prints nothing.
func loginUserNote(dep api.Deployment) string {
	if _, known := loginUserFor(dep); known {
		return ""
	}
	return fmt.Sprintf("login user unknown, assuming %q — if refused, `aq ssh -user <name> %s`",
		sshUser, sshTarget(dep))
}
