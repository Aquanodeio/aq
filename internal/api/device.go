// Package api is a thin client for the Aquanode control API. Today it covers the
// device-login (pairing) endpoints used by `aq login`.
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to the Aquanode API base (e.g. https://server.aquanode.io/api/v1).
//
// The device-login endpoints are public; the funnel endpoints (`up`, status,
// ssh-keys) require auth. When APIKey/TeamID are set they are sent as the
// `x-api-key` / `x-team-id` headers the orchestrator expects.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	APIKey  string // x-api-key (the aq_sk_… token from `aq login`)
	TeamID  string // x-team-id (required by team-scoped routes like /deployments)
}

// New returns a Client for the given base URL with a sane default timeout.
func New(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// NewAuthed returns a Client that sends the API key + team id on every request.
func NewAuthed(baseURL, apiKey, teamID string) *Client {
	c := New(baseURL)
	c.APIKey = apiKey
	c.TeamID = teamID
	return c
}

// Version is the aq build version. `main` overwrites it at startup from its own
// ldflags-injected `version`, so this package can label its requests without
// importing main.
var Version = "0.0.0-dev"

// setUserAgent identifies this caller to the orchestrator. Server-side product
// analytics uses it to attribute an event to the CLI rather than to a generic
// scripted API call — the API key alone is indistinguishable from `curl`. Keep
// the `aq/` prefix: the orchestrator matches on it (`surfaceFromRequest`).
func setUserAgent(req *http.Request) {
	req.Header.Set("User-Agent", "aq/"+Version)
}

// setAuth attaches the auth headers when the client is authenticated. Sending
// them on the public device endpoints is harmless (they are ignored there).
func (c *Client) setAuth(req *http.Request) {
	if c.APIKey != "" {
		req.Header.Set("x-api-key", c.APIKey)
	}
	if c.TeamID != "" {
		req.Header.Set("x-team-id", c.TeamID)
	}
}

// envelope mirrors the orchestrator's ResponseHandler shape.
type envelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   string          `json:"error"`
}

// DeviceStart is the result of POST /api-keys/device/start.
type DeviceStart struct {
	DeviceCode              string   `json:"deviceCode"`
	UserCode                string   `json:"userCode"`
	Scopes                  []string `json:"scopes"`
	VerificationURI         string   `json:"verificationUri"`
	VerificationURIComplete string   `json:"verificationUriComplete"`
	Interval                int      `json:"interval"`
	ExpiresIn               int      `json:"expiresIn"`
}

// DevicePoll is the result of POST /api-keys/device/token.
type DevicePoll struct {
	Status string `json:"status"` // pending | approved | denied | expired | consumed | slow_down
	// Interval, when > 0, is the cadence (seconds) the server wants the CLI to
	// poll at from now on (RFC 8628 §3.5). The CLI only ever slows down to it.
	Interval int      `json:"interval"`
	Token    string   `json:"token"`
	Scopes   []string `json:"scopes"`
	TeamID   string   `json:"teamId"`
	KeyName  string   `json:"keyName"`
}

func (c *Client) postJSON(path string, body any, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.BaseURL+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, out)
}

func (c *Client) getJSON(path string, out any) error {
	req, err := http.NewRequest(http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	return c.do(req, out)
}

func (c *Client) putJSON(path string, body any, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPut, c.BaseURL+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, out)
}

func (c *Client) patchJSON(path string, body any, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPatch, c.BaseURL+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, out)
}

func (c *Client) deleteJSON(path string, out any) error {
	req, err := http.NewRequest(http.MethodDelete, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	return c.do(req, out)
}

// do sends a prepared request (attaching auth + JSON content type) and decodes
// the orchestrator's `{success,data,error}` envelope into out.
func (c *Client) do(req *http.Request, out any) error {
	c.setAuth(req)
	setUserAgent(req)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nonJSONError(resp.StatusCode, raw)
	}
	if !env.Success {
		msg := env.Error
		if msg == "" {
			msg = fmt.Sprintf("request failed (HTTP %d)", resp.StatusCode)
		}
		return &APIError{Status: resp.StatusCode, Message: msg, Data: env.Data}
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("parse response data: %w", err)
		}
	}
	return nil
}

// maxBodySnippet bounds how much of a non-JSON error body is echoed to the user,
// so a full HTML error page (a proxy/load-balancer 5xx in front of the
// orchestrator) is reduced to a readable snippet instead of flooding the terminal.
const maxBodySnippet = 200

// nonJSONError renders the error for a response whose body isn't the JSON
// envelope. The common case is a gateway 5xx (502/503/504) served as an HTML
// page by a proxy in front of the orchestrator: those get a short, actionable
// message instead of the raw page. Anything else is truncated to a snippet so a
// large/HTML body doesn't swamp the CLI output (#209).
func nonJSONError(status int, raw []byte) error {
	switch status {
	case http.StatusBadGateway:
		return fmt.Errorf("the Aquanode server is temporarily unreachable (HTTP 502 Bad Gateway), please retry in a moment")
	case http.StatusServiceUnavailable:
		return fmt.Errorf("the Aquanode server is temporarily unavailable (HTTP 503 Service Unavailable), please retry in a moment")
	case http.StatusGatewayTimeout:
		return fmt.Errorf("the Aquanode server took too long to respond (HTTP 504 Gateway Timeout), please retry in a moment")
	}
	return fmt.Errorf("unexpected response (HTTP %d): %s", status, snippet(raw))
}

// snippet collapses a response body to a single bounded line: it joins
// whitespace-separated fields (so a multi-line HTML page becomes one line) and
// truncates to maxBodySnippet runes, appending an ellipsis when it cut content.
func snippet(raw []byte) string {
	flat := strings.Join(strings.Fields(string(raw)), " ")
	if flat == "" {
		return "(empty body)"
	}
	r := []rune(flat)
	if len(r) <= maxBodySnippet {
		return flat
	}
	return string(r[:maxBodySnippet]) + "…"
}

// APIError carries a non-success orchestrator response.
type APIError struct {
	Status  int
	Message string
	Data    json.RawMessage
}

func (e *APIError) Error() string { return e.Message }

// StartDevice begins a pairing.
func (c *Client) StartDevice(clientName string, scopes []string) (*DeviceStart, error) {
	body := map[string]any{}
	if clientName != "" {
		body["clientName"] = clientName
	}
	if len(scopes) > 0 {
		body["scopes"] = scopes
	}
	var out DeviceStart
	if err := c.postJSON("/api-keys/device/start", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PollDevice polls a pairing by its device code.
func (c *Client) PollDevice(deviceCode string) (*DevicePoll, error) {
	var out DevicePoll
	if err := c.postJSON("/api-keys/device/token", map[string]string{"deviceCode": deviceCode}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
