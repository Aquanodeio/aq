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
	Status  string   `json:"status"` // pending | approved | denied | expired | consumed
	Token   string   `json:"token"`
	Scopes  []string `json:"scopes"`
	TeamID  string   `json:"teamId"`
	KeyName string   `json:"keyName"`
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

// do sends a prepared request (attaching auth + JSON content type) and decodes
// the orchestrator's `{success,data,error}` envelope into out.
func (c *Client) do(req *http.Request, out any) error {
	c.setAuth(req)

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
		return fmt.Errorf("unexpected response (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
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
