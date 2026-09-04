package main

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// EVERY HELP TEXT MUST STATE THE PORT THAT IS ACTUALLY THE DEFAULT.
//
// The attach control port moved from 8443 to 8444 so that attach and ogre's own
// terminal proxy can both exist on one box: 8443 is the proxy's port. The two
// per-command flag helps derive their number from defaultOgrePort and followed
// the move; the top-level `aq help` block spelled it out in prose and did not,
// so `aq attach -h` and `aq host add -h` disagreed with each other and the
// top-level help told users to pick exactly the port that collides with the
// terminal proxy.
//
// A prose default is a second copy of a fact the constant already owns. This is
// the guard that keeps the copy honest: it reads the bytes the user sees, not
// the constant, because the constant was never the thing that was wrong.
var ogrePortDefaultPattern = regexp.MustCompile(`--ogre-port[^\n]*default:[^)\n]*?(\d{2,5})`)

func TestEveryHelpTextStatesTheRealOgrePortDefault(t *testing.T) {
	want := strconv.Itoa(defaultOgrePort)

	matches := ogrePortDefaultPattern.FindAllStringSubmatch(usageText, -1)
	if len(matches) == 0 {
		t.Fatalf("no --ogre-port default found in the top-level help; if the flag was renamed, "+
			"this guard must be renamed with it rather than left matching nothing. Help:\n%s", usageText)
	}
	for _, m := range matches {
		if m[1] != want {
			t.Errorf("top-level help states --ogre-port default %s, but defaultOgrePort is %s: %q",
				m[1], want, strings.TrimSpace(m[0]))
		}
	}
}

// The terminal proxy's port is the one number --ogre-port must never default
// to, which is the whole reason the move happened. Asserted separately from the
// text scan above so that a help block rewritten into some other wording still
// cannot land back on 8443.
func TestTheAttachDefaultIsNotTheTerminalProxyPort(t *testing.T) {
	if defaultOgrePort == terminalProxyPort {
		t.Fatalf("defaultOgrePort (%d) is ogre's terminal proxy port: attach would take the "+
			"terminal's listener and the box would have no browser terminal", defaultOgrePort)
	}
}
