package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A 5xx from our own API is an ANSWER, not a transport failure. The whole
// point of the split (#967: `aq import` told every user to check their network
// while the orchestrator was the thing that was misconfigured).
func TestIsTransportFailureOnServerRefusal(t *testing.T) {
	cases := []struct {
		name string
		body string
		code int
	}{
		{name: "503 JSON envelope", body: `{"success":false,"error":"service unavailable"}`, code: http.StatusServiceUnavailable},
		{name: "503 HTML gateway page", body: `<html><body>503</body></html>`, code: http.StatusServiceUnavailable},
		{name: "500 JSON envelope", body: `{"success":false,"error":"boom"}`, code: http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			err := New(srv.URL).getJSON("/whatever", nil)
			if err == nil {
				t.Fatal("expected an error from a 5xx response")
			}
			if IsTransportFailure(err) {
				t.Fatalf("a %d answer must not be classified as the user's network: %v", tc.code, err)
			}
		})
	}
}

// The genuine case: nothing answered at all.
func TestIsTransportFailureOnUnreachableServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening on that port any more

	err := New(url).getJSON("/whatever", nil)
	if err == nil {
		t.Fatal("expected a dial error against a closed port")
	}
	if !IsTransportFailure(err) {
		t.Fatalf("a dial failure must be classified as a transport failure: %v", err)
	}
}

func TestIsTransportFailureOnUnrelatedError(t *testing.T) {
	// A parse/logic error is neither side of the wire; it must not be dressed
	// up as a network problem.
	if IsTransportFailure(errors.New("parse response data: unexpected end of JSON input")) {
		t.Fatal("a non-network, non-API error must not be reported as a transport failure")
	}
}
