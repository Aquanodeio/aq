package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGatewayErrorIsConcise is the #209 fix: a gateway 5xx served as an HTML
// page (a proxy in front of the orchestrator) yields a short, actionable message
// rather than the full page.
func TestGatewayErrorIsConcise(t *testing.T) {
	htmlPage := "<html><head><title>502 Bad Gateway</title></head><body><h1>502 Bad Gateway</h1>" +
		strings.Repeat("<p>nginx is sad and this paragraph is long.</p>", 50) + "</body></html>"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(htmlPage))
	}))
	defer srv.Close()

	err := New(srv.URL).getJSON("/anything", nil)
	if err == nil {
		t.Fatal("expected an error for a 502 HTML page")
	}
	msg := err.Error()
	if !strings.Contains(msg, "502") || !strings.Contains(strings.ToLower(msg), "retry") {
		t.Errorf("expected a concise 502/retry message; got: %q", msg)
	}
	if strings.Contains(msg, "<html>") || strings.Contains(msg, "<p>") {
		t.Errorf("error should not echo raw HTML; got: %q", msg)
	}
}

// TestUnexpectedBodyIsTruncated checks that a non-JSON, non-gateway body is
// flattened to one line and truncated to a snippet (#209).
func TestUnexpectedBodyIsTruncated(t *testing.T) {
	body := strings.Repeat("x", 5000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("line1\nline2\n" + body))
	}))
	defer srv.Close()

	err := New(srv.URL).getJSON("/anything", nil)
	if err == nil {
		t.Fatal("expected an error for a non-JSON 500 body")
	}
	msg := err.Error()
	if len(msg) > 300 {
		t.Errorf("expected a truncated snippet, got %d chars: %q", len(msg), msg)
	}
	if !strings.Contains(msg, "HTTP 500") {
		t.Errorf("expected the status in the message; got: %q", msg)
	}
	if strings.Contains(msg, "\n") {
		t.Errorf("expected a single-line snippet; got: %q", msg)
	}
}
