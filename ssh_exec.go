package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// execSSH replaces the aq process with ssh.
//
// syscall.Exec beats running ssh as a child: the tty is ssh's directly (no pty
// shim, no terminal-resize breakage), Ctrl-C / Ctrl-Z / SIGWINCH reach ssh with
// the semantics the user expects, and ssh's exit status is the process's exit
// status with no translation layer. It has no Windows equivalent, which is a
// non-issue — .goreleaser.yml builds linux and darwin only.
//
// On success this never returns; everything below the call is the error path.
func execSSH(args []string) error {
	bin, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh is not installed or not on PATH: %w", err)
	}
	if err := syscall.Exec(bin, append([]string{"ssh"}, args...), os.Environ()); err != nil {
		return fmt.Errorf("could not run ssh: %w", err)
	}
	return nil
}
