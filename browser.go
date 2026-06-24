package main

import (
	"os/exec"
	"runtime"
)

// openBrowser best-effort opens a URL in the user's default browser. A failure
// is non-fatal — the login flow still prints the URL + code for manual use.
func openBrowser(url string) error {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
		args = []string{url}
	case "windows":
		cmd = "rundll32"
		args = []string{"url.dll,FileProtocolHandler", url}
	default: // linux, bsd, ...
		cmd = "xdg-open"
		args = []string{url}
	}
	return exec.Command(cmd, args...).Start()
}
