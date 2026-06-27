package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHTTPAppReady covers the #234 readiness probe: the app counts as live only
// when it answers with a non-gateway status; a connection error or a gateway 5xx
// (proxy up, backend app not bound yet) counts as not-ready.
func TestHTTPAppReady(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   bool
	}{
		{"200 OK", http.StatusOK, true},
		{"401 auth challenge (app is serving)", http.StatusUnauthorized, true},
		{"403 forbidden", http.StatusForbidden, true},
		{"500 app error still means bound+serving", http.StatusInternalServerError, true},
		{"502 bad gateway — app not bound yet", http.StatusBadGateway, false},
		{"503 service unavailable", http.StatusServiceUnavailable, false},
		{"504 gateway timeout", http.StatusGatewayTimeout, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()
			if got := httpAppReady(srv.URL); got != tc.want {
				t.Errorf("httpAppReady(%d) = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}

// TestHTTPAppReadyConnectionRefused: a URL with nothing listening (the box's app
// port not bound yet) must read as not-ready, not as live.
func TestHTTPAppReadyConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now → connection refused

	if httpAppReady(url) {
		t.Errorf("expected not-ready when the connection is refused, got ready")
	}
}
