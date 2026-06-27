package main

import (
	"io"
	"net/http"
	"time"
)

// appProbeTimeout bounds a single readiness probe to the app URL. It is short so
// a slow/hung app doesn't stall the poll loop — a non-answer just means "not yet"
// and the next poll retries.
const appProbeTimeout = 8 * time.Second

// httpAppReady reports whether the app behind probeURL is actually serving.
//
// A deployment reaching ACTIVE with a published service URL is NOT enough: the
// orchestrator surfaces the URL before the app inside the box has bound its port,
// so clicking it right away gives a connection error (#234). We GET the URL and
// treat it as live only when the app answers — any non-gateway HTTP status counts
// (e.g. ComfyUI's 401 auth challenge or a 200), because that proves the app bound
// its port and is serving. A connection error (port not bound yet) or a gateway
// 5xx (502/503/504 — the reverse proxy is up but the backend app isn't) means
// "not yet", so the caller keeps polling until the URL is genuinely reachable.
func httpAppReady(probeURL string) bool {
	client := &http.Client{Timeout: appProbeTimeout}
	resp, err := client.Get(probeURL)
	if err != nil {
		return false
	}
	// Drain a little so the connection can be reused, then close.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return false
	}
	return true
}
