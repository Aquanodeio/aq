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
type CreateEndpointRequest struct {
	Name          string `json:"name"`
	VersionID     int    `json:"versionId"`
	MaxInstances  int    `json:"maxInstances"`
	SpendCapCents int64  `json:"spendCapCents"`
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
type CreateCallRequest struct {
	Inputs map[string]any `json:"inputs"`
}

// CreateCallResult is the data returned by POST /endpoints/:id/calls.
type CreateCallResult struct {
	CallID     string `json:"callId"`
	Status     string `json:"status"`
	AcceptedAt string `json:"acceptedAt"`
}

// CreateCall makes a call against an endpoint.
func (c *Client) CreateCall(endpointID string, req CreateCallRequest) (*CreateCallResult, error) {
	var out CreateCallResult
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
