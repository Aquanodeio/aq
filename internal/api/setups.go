package api

import (
	"net/url"
	"strconv"
)

// Setup-lineage endpoints backing `aq snapshot`, `aq share`, `aq autosave`,
// and `aq setups`. A "setup" is its own object, distinct from the deployment
// that may currently hold its compute lease (Deployment, in control.go) —
// never pass a deployment id where a setup id belongs, or vice versa.
//
// Setup ids are UUID strings (`model Setup { id String @id
// @default(uuid()) ... }`), NOT the small integer ids deployments use.
// Every setup-id parameter here is a string for exactly that reason, and
// every path is built by string concatenation (url-escaped) rather than
// strconv.Itoa.
//
// Every field tag here is snake_case, per this orchestrator's DTO convention
// (toBackupDTO, toSnapshotVersionDTO, ...) — there is no camelCase transform
// on these routes.

// CreateSetupSnapshotRequest is the body of POST /setups/:id/snapshot. Name
// only matters on a setup's first save — it names the lineage every later
// save lands in. Callers past the first save must leave it empty: resending
// a name once a lineage already exists is not a rename, it's just ignored
// noise the CLI has no reason to send.
//
// WorkspaceDir is the directory captured. It keeps the wire name the
// ogre-facing path already uses end to end — `aq snapshot`'s --path flag
// maps onto it, but the flag rename is CLI-surface only, not a second name
// for the same value on the wire.
type CreateSetupSnapshotRequest struct {
	Name         string `json:"name,omitempty"`
	WorkspaceDir string `json:"workspace_dir"`
}

// SetupVersion mirrors one row of the setup_versions table, as returned by
// POST /setups/:id/snapshot (the newly created version), GET
// /setups/versions?name=..., and nested as Setup.LatestVersion.
//
// SetupID is a string for the same reason Setup.ID is — see the package doc.
// The version row's own ID is left an int: unlike Setup, nothing in the
// ticket's Prisma excerpt confirms its type, and control.go already shows
// this schema mixing int autoincrement ids (Deployment) with uuid ids
// (Setup) rather than using one convention everywhere. If a live server
// returns a non-numeric version id, ShareSetupVersion's request will surface
// that as a clear decode/404 error rather than a silent wrong-row share —
// but it hasn't been confirmed either way.
type SetupVersion struct {
	ID              int    `json:"id"`
	Name            string `json:"name"`
	Version         int    `json:"version"`
	Label           string `json:"label"`
	Description     string `json:"description"`
	Visibility      string `json:"visibility"`
	Path            string `json:"path"`
	SizeBytes       int64  `json:"size_bytes"`
	CreatedAt       string `json:"created_at"`
	Provenance      string `json:"provenance"`
	Pinned          bool   `json:"pinned"`
	SetupID         string `json:"setup_id"`
	DeploymentCount int    `json:"deployment_count"`
}

// CreateSetupSnapshot saves a setup's current state into its named lineage,
// returning the newly created version row. The very first call for a setup
// creates the lineage (from req.Name, or the setup's own name if that's
// empty too); every call after silently reuses it and increments Version.
func (c *Client) CreateSetupSnapshot(setupID string, req CreateSetupSnapshotRequest) (*SetupVersion, error) {
	var out SetupVersion
	path := "/setups/" + url.PathEscape(setupID) + "/snapshot"
	if err := c.postJSON(path, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListSetupVersions returns every version row named `name` — GET
// /setups/versions?name=<name>. The name alone is NOT unique to one setup
// (two different setups' lineages can share a chosen name), so a caller that
// needs one particular setup's version must additionally filter the result
// on SetupID. This is `aq share`'s only path from a (setup, version-number)
// pair the user typed to the version ROW id the share route actually needs —
// version and id are different counters (see ShareSetupVersion), and this is
// the one lookup that resolves one to the other instead of guessing.
func (c *Client) ListSetupVersions(name string) ([]SetupVersion, error) {
	var out []SetupVersion
	path := "/setups/versions?name=" + url.QueryEscape(name)
	if err := c.getJSON(path, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetSetupVersion fetches one version row by its own global row id — GET
// /setups/versions/:id. `aq endpoint point` uses this to learn which
// lineage (setup id + name) an endpoint's CURRENT version belongs to: the
// repoint API and the `<version>` a user types are both scoped to a version
// NUMBER within one lineage, but an Endpoint only carries its current
// VersionID, not the owning setup — this is the lookup that recovers it,
// the same way ListSetupVersions recovers a row id from a (setup,
// version-number) pair.
func (c *Client) GetSetupVersion(versionRowID int) (*SetupVersion, error) {
	var out SetupVersion
	path := "/setups/versions/" + strconv.Itoa(versionRowID)
	if err := c.getJSON(path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ShareSetupVersionResult is the data returned by POST
// /setups/versions/:id/share.
type ShareSetupVersionResult struct {
	URL string `json:"url"`
}

// ShareSetupVersion mints a link for ONE immutable version of a setup's save
// lineage. versionRowID is the setup_versions table's own id — NOT the
// per-lineage version NUMBER (v1, v2, v3, ...) a user types; those are
// different counters and must never be confused (see ListSetupVersions,
// which resolves one to the other). The link always addresses that exact
// version, never the lineage's current head — sharing v3 keeps pointing at
// v3's bytes even after v4, v5, ... exist.
func (c *Client) ShareSetupVersion(versionRowID int) (*ShareSetupVersionResult, error) {
	var out ShareSetupVersionResult
	path := "/setups/versions/" + strconv.Itoa(versionRowID) + "/share"
	if err := c.postJSON(path, struct{}{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetupAutosaveRequest is the body of PUT /setups/:id/autosave.
type SetupAutosaveRequest struct {
	Enabled bool `json:"enabled"`
}

// SetSetupAutosave turns a setup's autosave on or off, returning the updated
// Setup row. Autosave keeps one always-current copy — it is not a history
// and not undo, so the CLI prints that warning (plus the per-GiB storage
// rate) itself before ever calling this when turning it on.
func (c *Client) SetSetupAutosave(setupID string, enabled bool) (*Setup, error) {
	var out Setup
	path := "/setups/" + url.PathEscape(setupID) + "/autosave"
	if err := c.putJSON(path, SetupAutosaveRequest{Enabled: enabled}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Setup mirrors one row of GET /setups: what the caller owns, independent of
// whether the underlying compute is currently rented.
//
// ID is a UUID string (`model Setup { id String @id @default(uuid()) ...
// }`) — never the numeric Deployment.ID from control.go, even though a
// running setup has one associated via LeaseDeploymentID. There is also no
// boolean "running" field on the wire — Running derives it from
// LeaseDeploymentID, which is only non-nil while a live deployment holds the
// lease.
type Setup struct {
	ID                string        `json:"id"`
	Name              string        `json:"name"`
	Status            string        `json:"status"`
	MountPath         string        `json:"mount_path"`
	AutosaveEnabled   bool          `json:"autosave_enabled"`
	SizeBytes         int64         `json:"size_bytes"`
	LastSyncAt        string        `json:"last_sync_at"`
	LeaseDeploymentID *int          `json:"lease_deployment_id"`
	CreatedAt         string        `json:"created_at"`
	UpdatedAt         string        `json:"updated_at"`
	LatestVersion     *SetupVersion `json:"latest_version"`
}

// Running reports whether a deployment currently holds this setup's lease.
func (s Setup) Running() bool {
	return s.LeaseDeploymentID != nil
}

// ListSetups returns every setup the caller owns.
func (c *Client) ListSetups() ([]Setup, error) {
	var out []Setup
	if err := c.getJSON("/setups", &out); err != nil {
		return nil, err
	}
	return out, nil
}
