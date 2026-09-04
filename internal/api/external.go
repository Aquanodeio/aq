package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
)

// External-box endpoints: adopting a machine Aquanode never provisioned into the
// control plane, and letting it go again.
//
// Every route here is gated server-side by the EXTERNAL_BOXES_ENABLED kill
// switch. With the switch off they are 404, not 500, so a CLI hitting an
// orchestrator that has the feature turned off gets "no such route" rather than
// a stack trace — treat a 404 from any of these as "not enabled here".

// AdoptExternalRequest is the body of POST /deployments/external.
//
// Hardware fields are all optional and all nullable: this is a box we did not
// provision, so anything we did not observe stays absent rather than being
// guessed at or zeroed. A zero GPU count would read as "no GPUs", which is a
// different claim from "we did not look".
type AdoptExternalRequest struct {
	Name      string `json:"name"`
	Host      string `json:"host"`
	OgrePort  int    `json:"ogrePort"`
	GPU       string `json:"gpu,omitempty"`
	GPUCount  int    `json:"gpuCount,omitempty"`
	CPU       int    `json:"cpu,omitempty"`
	Memory    string `json:"memory,omitempty"`
	Storage   string `json:"storage,omitempty"`
	MountPath string `json:"mountPath,omitempty"`
}

// AdoptExternalResult is the response to POST /deployments/external.
type AdoptExternalResult struct {
	DeploymentID int    `json:"deploymentId"`
	InstallToken string `json:"installToken"`
	ExpiresAt    string `json:"expiresAt"`
	OgrePort     int    `json:"ogrePort"`
}

// AdoptExternal creates the Deployment row for a box we did not rent. It bills
// nothing: the orchestrator sets pricePerSecond and providerCostPerSecond to
// zero, leaves leaseId null, and does not emit a billing START.
func (c *Client) AdoptExternal(req AdoptExternalRequest) (*AdoptExternalResult, error) {
	var out AdoptExternalResult
	if err := c.postJSON("/deployments/external", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ExternalInstallConfig is the response to POST
// /deployments/external/:id/install-config — the only place these credentials
// ever leave the orchestrator.
//
// The token that redeems it is single-use with a TTL, so aq must treat this as a
// one-shot: it is never re-fetchable, never cached to disk, and a failure after
// redeeming means starting the attach over rather than retrying the redemption.
type ExternalInstallConfig struct {
	OgreJWTSecret   string `json:"ogreJwtSecret"`
	OgreProxyPass   string `json:"ogreProxyPassword"`
	TLSCertPEM      string `json:"tlsCertPem"`
	TLSKeyPEM       string `json:"tlsKeyPem"`
	OgrePort        int    `json:"ogrePort"`
	OrchestratorURL string `json:"orchestratorUrl"`
	// The deployment the token named, echoed back by the orchestrator as a
	// NUMBER — the same JSON type `AdoptExternalResult.DeploymentID` above
	// carries, because both fields are the same `Deployment.id`. It was typed
	// `string` here and nothing read it, so `json.Unmarshal` failed on the very
	// first live redeem and attach could never complete. Typed and CHECKED now:
	// an unread echo is worth nothing, and a wrong one means we are about to
	// write another box's credentials onto this machine.
	DeploymentID int `json:"deploymentId"`
}

// RedeemExternalInstallConfig exchanges the single-use install token for the
// box's credentials.
//
// It authenticates with the install token as a Bearer header and NOTHING else —
// deliberately not the user's API key. The token is the whole authority for this
// one call, it is consumed on first success, and scoping the request to it means
// a leaked install token grants exactly one box's config rather than acting as
// the user.
func (c *Client) RedeemExternalInstallConfig(deploymentID int, installToken string) (*ExternalInstallConfig, error) {
	if installToken == "" {
		return nil, errors.New("no install token to redeem")
	}
	req, err := http.NewRequest(http.MethodPost,
		c.BaseURL+"/deployments/external/"+strconv.Itoa(deploymentID)+"/install-config",
		bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+installToken)
	setUserAgent(req)

	var out ExternalInstallConfig
	if err := c.doBearer(req, &out); err != nil {
		return nil, err
	}
	// The orchestrator already refuses a token that does not name the path's
	// deployment; this is the same check from the other end. It is cheap, and
	// the failure it catches — installing one box's credentials onto another —
	// is the worst outcome this call has.
	if out.DeploymentID != deploymentID {
		return nil, fmt.Errorf("install config is for deployment #%d, not #%d", out.DeploymentID, deploymentID)
	}
	return &out, nil
}

// doBearer is do() without setAuth — the install-config call must carry the
// install token and no session credential.
func (c *Client) doBearer(req *http.Request, out any) error {
	saved := *c
	saved.APIKey, saved.TeamID = "", ""
	return saved.do(req, out)
}

// ActivateExternalResult is the response to POST
// /deployments/external/:id/activate.
type ActivateExternalResult struct {
	Status         string `json:"status"`
	AgentLastSeen  string `json:"agentLastSeenAt"`
	Error          string `json:"error"`
	UnreachableWhy string `json:"reason"`
	// TerminalAvailable is whether the orchestrator confirmed the box's browser
	// terminal answers FROM ITS OWN INFRASTRUCTURE, not merely that the box
	// reported a listener to itself. Reported here so attach can say what the
	// user actually got instead of leaving them to find a dead Terminal tab.
	//
	// A POINTER because it is three-state. `nil` is "not determined yet": the
	// box configuration outlives the activate request (see Provisioning), and an
	// orchestrator predating that field omits it entirely. A plain bool would
	// render both of those as a confirmed dead terminal.
	TerminalAvailable *bool `json:"terminalAvailable"`
	// TerminalUnavailableReason is the orchestrator's machine-readable cause,
	// empty when the terminal IS available or when the orchestrator predates the
	// field. Empty is UNKNOWN, never "no reason".
	TerminalUnavailableReason string `json:"terminalUnavailableReason"`
	// ResticConfigured is whether the box holds the credential that makes a
	// snapshot taken on it readable later. Three-state for the same reason as
	// TerminalAvailable.
	ResticConfigured *bool `json:"resticConfigured"`
	// Provisioning is "complete" when every verdict above is a real answer, and
	// "in_progress" when the attach SUCCEEDED and the box configuration is still
	// running server-side. Empty from an orchestrator predating the field, which
	// is the old always-synchronous behaviour and reads as complete.
	//
	// The attach itself is done in both cases. This distinguishes "we know what
	// you got" from "you are attached and the optional extras are still being
	// set up", which is the distinction that used to be lost entirely.
	Provisioning string `json:"provisioning"`
}

// Attached reports whether the orchestrator says this box is attached, which is
// the only question `aq attach` succeeds or fails on. Deliberately independent
// of every verdict above: the terminal and the restic credential are optional
// capabilities configured AFTER the attachment, and neither one failing means
// the box is not attached.
func (r *ActivateExternalResult) Attached() bool {
	return r != nil && strings.EqualFold(r.Status, "active")
}

// StillProvisioning reports whether the box configuration outlived the request.
func (r *ActivateExternalResult) StillProvisioning() bool {
	return r != nil && r.Provisioning == "in_progress"
}

// IsTimeout reports whether an error is this CLI's own HTTP client giving up,
// as opposed to the server answering with a refusal.
//
// THE DISTINCTION IS THE WHOLE POINT, and collapsing it is how `aq` reported a
// FAILED attach on an attach that had completed. A timeout says nothing about
// what the server did: the request may have been served in full a moment later.
// It is never evidence of failure, only evidence that we stopped listening, so
// a caller that hits one must go and ASK what happened rather than announce an
// outcome it did not observe.
func IsTimeout(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// ActivateExternal asks the orchestrator to probe the box and, only if a real
// round-trip succeeds, flip it ACTIVE.
//
// The probe is deliberately server-side: what has to be true is that OUR
// infrastructure can reach the box, and the CLI proving it can reach it from the
// user's laptop answers a different question. A failure returns
// ExternalUnreachableError carrying the orchestrator's own reason string, which
// callers surface verbatim — a paraphrase of a dial error is worse than useless
// to whoever has to open the firewall.
func (c *Client) ActivateExternal(deploymentID int) (*ActivateExternalResult, error) {
	var out ActivateExternalResult
	err := c.postJSON("/deployments/external/"+strconv.Itoa(deploymentID)+"/activate", map[string]any{}, &out)
	if err != nil {
		if reason, ok := unreachableReason(err); ok {
			return nil, &ExternalUnreachableError{Reason: reason}
		}
		return nil, err
	}
	return &out, nil
}

// ExternalUnreachableError is the orchestrator refusing to activate a box it
// could not reach. Its Reason is the server's own words, never a rewrite.
type ExternalUnreachableError struct {
	Reason string
}

func (e *ExternalUnreachableError) Error() string {
	if e.Reason == "" {
		return "the orchestrator could not reach the box, and gave no reason"
	}
	return e.Reason
}

// unreachableReason digs the probe's reason out of an error envelope. It reads
// the structured `reason` when there is one and falls back to the message —
// never to a generic string of our own, since the reason is the entire value of
// a failed probe.
func unreachableReason(err error) (string, bool) {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return "", false
	}
	if apiErr.Status != http.StatusConflict {
		return "", false
	}
	var body struct {
		Error  string `json:"error"`
		Reason string `json:"reason"`
	}
	if len(apiErr.Data) > 0 {
		if jsonErr := json.Unmarshal(apiErr.Data, &body); jsonErr == nil && body.Reason != "" {
			return body.Reason, true
		}
	}
	return apiErr.Message, true
}

// ReleaseResult is the response to POST /deployments/:id/release.
type ReleaseResult struct {
	Released bool `json:"released"`
	// BoxCleared is THREE-STATE and is a *bool for exactly that reason: true we
	// cleared every credential we pushed to the box, false the box refused,
	// nil we could not look (or the orchestrator predates the field). The last
	// two both mean live credentials may remain on the customer's machine, and
	// a bool would make them indistinguishable from a clean hand-back.
	BoxCleared *bool `json:"boxCleared"`
	// BoxClearReason is the orchestrator's own words for why, present whenever
	// BoxCleared is not true.
	BoxClearReason string `json:"boxClearReason"`
}

// ReleaseExternal clears the state Aquanode pushed to the box, revokes its
// credentials and drops its row.
//
// It never reaches a provider adapter and nothing is torn down. The machine
// keeps running exactly as it was — it is on a lease we do not hold and have no
// business ending. This is why the verb is `release` and not `terminate`.
//
// It DOES contact the box, over ogre's own API, and reports three-state whether
// that succeeded. The daemon itself is stopped by `aq release` over ssh, which
// is the half no HTTP call can do: ogre exposes no shutdown endpoint on
// purpose.
func (c *Client) ReleaseExternal(deploymentID int) (*ReleaseResult, error) {
	var out ReleaseResult
	if err := c.postJSON("/deployments/"+strconv.Itoa(deploymentID)+"/release", map[string]any{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
