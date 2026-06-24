package main

import (
	"fmt"
	"net/url"
	"os/exec"
	"runtime"

	"github.com/Aquanodeio/aq/internal/config"
)

// openBrowser best-effort opens a URL in the user's default browser. A failure
// is non-fatal — the login flow still prints the URL + code for manual use.
//
// The URL is server-supplied (the device-login verification URI), so it is
// validated before being handed to the OS opener: passing an arbitrary string
// (e.g. `file://…`, a custom-scheme handler, or a `-`-leading flag) to
// `open`/`xdg-open` could launch something unintended from a compromised or
// MITM'd response. Only an https:// URL — or an http:// URL on the loopback host
// or the configured API host, for local dev — is opened (#209).
func openBrowser(rawURL string) error {
	if err := validateBrowserURL(rawURL); err != nil {
		return err
	}

	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
		args = []string{rawURL}
	case "windows":
		cmd = "rundll32"
		args = []string{"url.dll,FileProtocolHandler", rawURL}
	default: // linux, bsd, ...
		cmd = "xdg-open"
		args = []string{rawURL}
	}
	return exec.Command(cmd, args...).Start()
}

// validateBrowserURL accepts only a URL safe to open: any https:// URL, or an
// http:// URL whose host is the loopback interface or the configured API host
// (local/self-hosted dev). Everything else — non-web schemes, plain http to an
// arbitrary host, malformed input — is refused.
func validateBrowserURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("refusing to open malformed URL %q: %w", rawURL, err)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLocalDevHost(u.Hostname()) {
			return nil
		}
	}
	return fmt.Errorf("refusing to open non-https URL %q", rawURL)
}

// isLocalDevHost reports whether an http:// URL host is trusted for local dev:
// the loopback interface, or the host of the configured Aquanode API base
// (AQ_API_URL) so a self-hosted/dev backend on http still opens.
func isLocalDevHost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	if api, err := url.Parse(config.APIURL()); err == nil && api.Hostname() == host && host != "" {
		return true
	}
	return false
}
