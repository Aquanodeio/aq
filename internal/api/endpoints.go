package api

import (
	"net/url"
)

// Endpoint-and-calls endpoints backing `aq endpoint`, `aq call`, and `aq
// calls`. An endpoint is a stable, callable address in front of ONE setup
// version — creating one is handing out a GPU budget (MaxInstances +
// SpendCapCents), which is why the CLI requires both rather than defaulting
// either to unbounded.
//
// Unlike setups.go's snake_case DTOs, these routes speak camelCase on the
// wire — match the field names exactly (versionId, spendCapCents, ...), do
// not "normalize" them to snake_case.

// Endpoint mirrors one row of GET /endpoints.
type Endpoint struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	VersionID        int    `json:"versionId"`
	Status           string `json:"status"`
	SpentCents       int64  `json:"spentCents"`
	SpendCapCents    int64  `json:"spendCapCents"`
	RunningInstances int    `json:"runningInstances"`
	CallsThisPeriod  int    `json:"callsThisPeriod"`
}

// ListEndpoints returns every endpoint the caller owns.
func (c *Client) ListEndpoints() ([]Endpoint, error) {
	var out []Endpoint
	if err := c.getJSON("/endpoints", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateEndpointRequest is the body of POST /endpoints. MaxInstances and
// SpendCapCents are both always sent — the CLI never lets either be omitted
// (see endpointCreate's validation), so there is no unbounded-by-default
// path on the wire either.
//
// PinnedDeploymentID pins the endpoint to a box the customer already owns
// (attached via `aq host add` + `aq attach`) instead of hardware Aquanode
// rents. It carries `omitempty` deliberately: the zero value must never
// reach the wire as a present-but-empty key, only as an absent one, the
// server reads an absent key as "today's managed behaviour" and a present
// zero/negative one as a malformed pin. endpointCreate resolves this from a
// `--on <alias>` flag locally and refuses before ever building this request
// unless the alias names a genuinely attached deployment.
type CreateEndpointRequest struct {
	Name               string `json:"name"`
	VersionID          int    `json:"versionId"`
	MaxInstances       int    `json:"maxInstances"`
	SpendCapCents      int64  `json:"spendCapCents"`
	PinnedDeploymentID int    `json:"pinnedDeploymentId,omitempty"`
}

// CreateEndpoint makes a setup version callable, returning the created
// endpoint row.
func (c *Client) CreateEndpoint(req CreateEndpointRequest) (*Endpoint, error) {
	var out Endpoint
	if err := c.postJSON("/endpoints", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RepointEndpointRequest is the body of POST /endpoints/:id/repoint.
type RepointEndpointRequest struct {
	VersionID int `json:"versionId"`
}

// RepointEndpoint switches an endpoint to a different version — the same
// call rolls it forward or back, it just depends which VersionID is passed.
func (c *Client) RepointEndpoint(endpointID string, req RepointEndpointRequest) (*Endpoint, error) {
	var out Endpoint
	path := "/endpoints/" + url.PathEscape(endpointID) + "/repoint"
	if err := c.postJSON(path, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteEndpoint removes an endpoint — DELETE /endpoints/:id.
func (c *Client) DeleteEndpoint(endpointID string) error {
	path := "/endpoints/" + url.PathEscape(endpointID)
	return c.deleteJSON(path, nil)
}

// Call mirrors one row of GET /endpoints/:id/calls, and the object returned
// by GET /endpoints/:id/calls/:callId.
//
// Status is one of queued|running|succeeded|failed|unservable. "unservable"
// must never be conflated with "failed": it means Aquanode could not get the
// caller a box at all (capacity, budget, or provider failure) — the workload
// itself never ran — while "failed" means it ran and the workload errored.
// Reason carries the detail for both, but especially matters for
// unservable, which is otherwise indistinguishable from a normal decline.
type Call struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	Reason     string `json:"reason"`
	AcceptedAt string `json:"acceptedAt"`
	StartedAt  string `json:"startedAt"`
	FinishedAt string `json:"finishedAt"`
	Phase      string `json:"phase"`
}

// ListCalls returns an endpoint's recent calls — GET /endpoints/:id/calls.
func (c *Client) ListCalls(endpointID string) ([]Call, error) {
	var out []Call
	path := "/endpoints/" + url.PathEscape(endpointID) + "/calls"
	if err := c.getJSON(path, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateCallRequest is the body of POST /endpoints/:id/calls. Inputs is
// always a non-nil map (possibly empty) — the CLI sends `{"inputs":{}}`
// rather than omitting the field when the caller passes no --input file.
// Wait and WaitSeconds are optional; when set, the server will try to return
// a 200 with the full call object if it completes within the window.
type CreateCallRequest struct {
	Inputs      map[string]any `json:"inputs"`
	Wait        bool           `json:"wait,omitempty"`
	WaitSeconds int            `json:"waitSeconds,omitempty"`
}

// CreateCallResult is the data returned by POST /endpoints/:id/calls (202 async response).
type CreateCallResult struct {
	CallID     string `json:"callId"`
	Status     string `json:"status"`
	AcceptedAt string `json:"acceptedAt"`
}

// CreateCallResponse can be either a 202 CreateCallResult or a 200 Call object.
// It merges all possible fields; the presence of certain fields indicates which
// response type was returned (CallID → 202 async, ID → 200 completed).
type CreateCallResponse struct {
	// 202 async response fields
	CallID     string `json:"callId"`
	AcceptedAt string `json:"acceptedAt"`
	// 200 sync response fields (full Call object)
	ID         string `json:"id"`
	StartedAt  string `json:"startedAt"`
	FinishedAt string `json:"finishedAt"`
	OutputRef  string `json:"outputRef"`
	// Both responses include Status and Reason
	Status string `json:"status"`
	Reason string `json:"reason"`
	Phase  string `json:"phase"`
}

// IsAsync returns true if this is a 202 async response (still queued/running).
func (r *CreateCallResponse) IsAsync() bool {
	return r.CallID != "" && r.ID == ""
}

// CallID returns either the 202 callId or the 200 id, whichever is present.
func (r *CreateCallResponse) GetCallID() string {
	if r.CallID != "" {
		return r.CallID
	}
	return r.ID
}

// CreateCall makes a call against an endpoint. The response can be either
// 202 (async, still running) or 200 (sync, completed within the wait window).
// Use resp.IsAsync() to distinguish them, or resp.GetCallID() to get the id
// in either case.
func (c *Client) CreateCall(endpointID string, req CreateCallRequest) (*CreateCallResponse, error) {
	var out CreateCallResponse
	path := "/endpoints/" + url.PathEscape(endpointID) + "/calls"
	if err := c.postJSON(path, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetCall fetches one call by id — GET /endpoints/:id/calls/:callId.
func (c *Client) GetCall(endpointID, callID string) (*Call, error) {
	var out Call
	path := "/endpoints/" + url.PathEscape(endpointID) + "/calls/" + url.PathEscape(callID)
	if err := c.getJSON(path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
