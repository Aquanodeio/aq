package api

import "net/url"

// Environment endpoints for the pod/environment/volume model. An Environment
// is everything OUTSIDE /workspace: base image, installed packages, startup
// script. It saves silently with its pod on every Stop and only becomes a
// visible, named object when Kept or Shared (`aq env keep`/`aq env share`).
// Numbered versions (v1, v2, ...) are minted ONLY by Keep or Share — there is
// no standalone "save environment" verb.

// EnvironmentPreview is the data returned by GET
// /setups/:id/environment/preview: everything a share/keep confirmation needs
// to show BEFORE anything leaves the box — no box required, since it's
// rendered from the version's already-stored includedRoots/excluded.
type EnvironmentPreview struct {
	IncludedRoots []string               `json:"includedRoots"`
	Excluded      []string               `json:"excluded"`
	StartupScript string                 `json:"startupScript"`
	SecretHits    []EnvironmentSecretHit `json:"secretHits"`
}

// EnvironmentSecretHit is one line of the startup script that looked like a
// credential (hf_, sk-, AKIA, a PEM header, ...). Any hit refuses the share
// server-side; this is surfaced so the CLI can show exactly where before the
// server says no.
type EnvironmentSecretHit struct {
	Line int    `json:"line"`
	Kind string `json:"kind"`
}

// GetEnvironmentPreview fetches the outgoing-paths preview for setupID's
// current (working) environment — GET /setups/:id/environment/preview.
func (c *Client) GetEnvironmentPreview(setupID string) (*EnvironmentPreview, error) {
	var out EnvironmentPreview
	path := "/setups/" + url.PathEscape(setupID) + "/environment/preview"
	if err := c.getJSON(path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// EnvironmentVersion mirrors one row returned by GET
// /setups/:id/environment/versions and GET /environments/:id/versions.
type EnvironmentVersion struct {
	ID            string   `json:"id"`
	Version       int      `json:"version"`
	CreatedAt     string   `json:"createdAt"`
	IncludedRoots []string `json:"includedRoots"`
	Excluded      []string `json:"excluded"`
}

// ListSetupEnvironmentVersions returns every version of setupID's own
// environment lineage — GET /setups/:id/environment/versions. The owner sees
// the full history.
func (c *Client) ListSetupEnvironmentVersions(setupID string) ([]EnvironmentVersion, error) {
	var out []EnvironmentVersion
	path := "/setups/" + url.PathEscape(setupID) + "/environment/versions"
	if err := c.getJSON(path, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListEnvironmentVersions returns every version of a named (Kept or Shared)
// Environment — GET /environments/:id/versions. An owner sees every version;
// a recipient sees only the one version shared with them.
func (c *Client) ListEnvironmentVersions(environmentID string) ([]EnvironmentVersion, error) {
	var out []EnvironmentVersion
	path := "/environments/" + url.PathEscape(environmentID) + "/versions"
	if err := c.getJSON(path, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// KeepEnvironmentRequest is the body of POST /setups/:id/environment/keep.
type KeepEnvironmentRequest struct {
	Name string `json:"name"`
}

// KeepEnvironmentResult is the data returned by POST
// /setups/:id/environment/keep.
type KeepEnvironmentResult struct {
	EnvironmentID string `json:"environmentId"`
}

// KeepSetupEnvironment names setupID's current environment so it lists under
// Yours in the New pod picker and survives pod deletion. It mints nothing by
// itself — Share is the only thing that mints a version, and Keep on a pod
// whose environment already has one just files the existing lineage under a
// name.
func (c *Client) KeepSetupEnvironment(setupID string, name string) (*KeepEnvironmentResult, error) {
	var out KeepEnvironmentResult
	path := "/setups/" + url.PathEscape(setupID) + "/environment/keep"
	if err := c.postJSON(path, KeepEnvironmentRequest{Name: name}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ShareEnvironmentRequest is the body of POST
// /setups/:id/environment/share. VersionID is optional (absent = the
// lineage's latest version; a fresh capture runs first if the pod is
// Running).
type ShareEnvironmentRequest struct {
	VersionID string `json:"versionId,omitempty"`
}

// ShareResult is the data returned by POST /setups/:id/environment/share and
// POST /environments/:id/share. State is "preparing" until the server-side
// publish job (a restic copy into a fresh per-version repo) finishes;
// GET /shares/:shareId polls it to "ready" or "failed" — never assume ready
// from this response alone.
type ShareResult struct {
	ShareID  string `json:"shareId"`
	ShareURL string `json:"shareUrl"`
	State    string `json:"state"`
}

// ShareSetupEnvironment shares one version of setupID's own environment
// (POST /setups/:id/environment/share). Absent versionID shares the latest;
// on a Running pod the server captures fresh first.
func (c *Client) ShareSetupEnvironment(setupID string, versionID string) (*ShareResult, error) {
	var out ShareResult
	path := "/setups/" + url.PathEscape(setupID) + "/environment/share"
	if err := c.postJSON(path, ShareEnvironmentRequest{VersionID: versionID}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ShareEnvironmentByIDRequest is the body of POST /environments/:id/share.
// Unlike the pod-scoped share above, versionId is REQUIRED here: a named
// Environment has no single "current pod" to default the latest from.
type ShareEnvironmentByIDRequest struct {
	VersionID string `json:"versionId"`
}

// ShareEnvironmentByID shares one version of a named (Kept or Shared)
// Environment directly, without going through the pod that made it — POST
// /environments/:id/share.
func (c *Client) ShareEnvironmentByID(environmentID string, versionID string) (*ShareResult, error) {
	var out ShareResult
	path := "/environments/" + url.PathEscape(environmentID) + "/share"
	if err := c.postJSON(path, ShareEnvironmentByIDRequest{VersionID: versionID}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ShareEnvironmentRef is the {id,name} pair GET /shares/:shareId nests as
// environment: the environment the link names, present on every successful
// response regardless of state (a link whose environment is gone 404s
// instead, environment.service.ts's openShare).
type ShareEnvironmentRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ShareStatus is the data returned by GET /shares/:shareId (confirmed
// against environment.service.ts's openShare, aquanode-backend#803,
// 2026-09-26). State is three-state ("preparing"|"ready"|"failed"); Error is
// non-nil only once State is "failed". Environment is always present on a
// successful response regardless of State: openShare 404s the whole call
// (EnvironmentNotFoundError) rather than ever returning it null or absent.
//
// VersionID and Version are BOTH nullable, and null is not tied to a
// specific State the way it might look: they are null while a Running pod's
// share is still capturing (or that capture failed, so State is
// "preparing"/"failed" with no version minted yet), but can already be
// non-nil even while State is still "preparing", once the version exists
// and only its publish-to-a-shareable-copy job is still running. Never
// assume one implies the other; read both independently.
type ShareStatus struct {
	State       string              `json:"state"`
	Error       *string             `json:"error"`
	Environment ShareEnvironmentRef `json:"environment"`
	VersionID   *string             `json:"versionId"`
	Version     *int                `json:"version"`
}

// GetShareStatus polls a share's publish-job status.
func (c *Client) GetShareStatus(shareID string) (*ShareStatus, error) {
	var out ShareStatus
	path := "/shares/" + url.PathEscape(shareID)
	if err := c.getJSON(path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RevokeShare revokes a share link, DELETE /shares/:shareId. Deletes the
// published repo once no link to it remains.
func (c *Client) RevokeShare(shareID string) error {
	path := "/shares/" + url.PathEscape(shareID)
	return c.deleteJSON(path, nil)
}

// EnvironmentVersionRef is the {id,version,createdAt} triple GET
// /environments nests as an item's latestVersion (confirmed against
// EnvironmentListItem, orchestrator/src/services/environments/
// environment.service.ts, w3-backend 2026-09-26). ID here is the version's
// own row id, an EnvironmentVersion.ID, not the environment's id: this is
// what a caller creating a pod from this environment must send as
// POST /setups' environmentVersionId.
type EnvironmentVersionRef struct {
	ID        string `json:"id"`
	Version   int    `json:"version"`
	CreatedAt string `json:"createdAt"`
}

// EnvironmentSummary is one row of GET /environments' builtin/yours/shared
// arrays: enough to pick from in "aq env ls"/"aq pods create --env" and
// resolve a name to an id. Kind is "builtin"|"kept"|"shared" on THIS
// listing specifically (never "working": an unnamed working environment
// isn't a listable object until Kept or Shared, unlike Setup.environment.kind
// which does include "working"). LatestVersion is nil only for a
// genuinely version-less environment; nothing the picker offers should be
// version-less in practice, but a caller must still check rather than
// assume.
type EnvironmentSummary struct {
	ID            string                 `json:"id"`
	Name          string                 `json:"name"`
	Kind          string                 `json:"kind"`
	LatestVersion *EnvironmentVersionRef `json:"latestVersion"`
}

// EnvironmentsResult is the data returned by GET /environments: the three
// groups the New pod picker shows (Built-in, Yours, Shared with me).
type EnvironmentsResult struct {
	Builtin []EnvironmentSummary `json:"builtin"`
	Yours   []EnvironmentSummary `json:"yours"`
	Shared  []EnvironmentSummary `json:"shared"`
}

// ListEnvironments fetches every environment the caller can pick from — GET
// /environments.
func (c *Client) ListEnvironments() (*EnvironmentsResult, error) {
	var out EnvironmentsResult
	if err := c.getJSON("/environments", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteEnvironment deletes a Kept or Shared environment — DELETE
// /environments/:id. Breaks nothing running; existing share links to it stop
// working.
func (c *Client) DeleteEnvironment(environmentID string) error {
	path := "/environments/" + url.PathEscape(environmentID)
	return c.deleteJSON(path, nil)
}
