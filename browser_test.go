package main

import (
	"testing"
)

// TestValidateBrowserURL covers the #209 guard: only https:// (or http:// to a
// loopback/configured-API host for local dev) may be handed to the OS opener.
func TestValidateBrowserURL(t *testing.T) {
	t.Setenv("AQ_API_URL", "https://server.aquanode.io/api/v1")

	allowed := []string{
		"https://console.aquanode.io/devices?code=ABCD",
		"https://server.aquanode.io/api/v1/x",
		"http://localhost:3000/devices",
		"http://127.0.0.1:8080/devices",
	}
	for _, u := range allowed {
		if err := validateBrowserURL(u); err != nil {
			t.Errorf("expected %q to be allowed, got: %v", u, err)
		}
	}

	rejected := []string{
		"http://evil.example.com/devices", // plain http to an arbitrary host
		"file:///etc/passwd",              // non-web scheme
		"javascript:alert(1)",             // script scheme
		"ftp://example.com/x",
		"-n", // a flag-looking token
		"",   // empty
		"not a url with spaces",
	}
	for _, u := range rejected {
		if err := validateBrowserURL(u); err == nil {
			t.Errorf("expected %q to be refused, but it was allowed", u)
		}
	}
}

// TestValidateBrowserURLHonorsConfiguredHost checks the local-dev escape hatch:
// an http:// verification URI on the configured API host is allowed.
func TestValidateBrowserURLHonorsConfiguredHost(t *testing.T) {
	t.Setenv("AQ_API_URL", "http://dev.box.internal:8080/api/v1")
	if err := validateBrowserURL("http://dev.box.internal:8080/devices?code=X"); err != nil {
		t.Errorf("expected configured http host to be allowed, got: %v", err)
	}
	if err := validateBrowserURL("http://other.host/devices"); err == nil {
		t.Errorf("expected non-configured http host to be refused")
	}
}
