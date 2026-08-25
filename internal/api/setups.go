package api

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/Aquanodeio/aq/internal/config"
)

// Setup-lineage endpoints backing `aq save`, `aq share`, `aq fork`,
// `aq autosave`, `aq autopause`, `aq force-detach`, `aq sync-now`,
// `aq edit-version`, and `aq setups`. A "setup" is its own object, distinct
// from the deployment that may currently hold its compute lease (Deployment,
// in control.go) — never pass a deployment id where a setup id belongs, or
// vice versa.
//
// Setup ids are UUID strings (`model Setup { id String @id
// @default(uuid()) ... }`), NOT the small integer ids deployments use.
// Every setup-id parameter here is a string for exactly that reason, and
// every path is built by string concatenation (url-escaped) rather than
// strconv.Itoa.
//
// Tags here are NOT one convention — verified per field against the actual
// serializer/schema for its own endpoint, never inferred from a neighbour:
//   - SetupVersion and its request/response bodies (POST .../snapshot, GET
//     .../versions, PATCH .../versions/:id) go through toSnapshotVersionDTO /
//     the zod schemas in setups.schemas.ts, which genuinely are snake_case.
//   - The `Setup` struct below (GET /setups, GET /setups/:id-shaped routes)
//     is serialized by serializeSetup in setups.controller.ts, which
//     hand-writes a plain camelCase object literal instead of going through
//     one of those DTO helpers — there is genuinely no case-transform
//     middleware anywhere in the orchestrator (checked server.ts + everything
//     under src/middleware). So its fields are tagged camelCase to match,
//     the one exception being SizeBytes, whose wire type is also not a plain
//     number — see its doc comment below.

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
// POST /setups/:id/snapshot (the newly created version) and GET
// /setups/versions[?name=...]. There is no "latest version" field nested on
// Setup itself — see ListAllSetupVersions for how `aq setups`/`aq share`
// recover a setup's latest/named version instead.
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

// ListAllSetupVersions returns every version row the caller can see across
// every one of their setups — GET /setups/versions with no `name` filter.
// The orchestrator's listSnapshotVersions only takes the legacy-lineage
// merge path when `name` is unset (listSnapshotVersions in
// setups.controller.ts), so this also carries any legacy/external
// (backup_id-owned, no SetupID) rows mixed in; callers matching on SetupID
// filter those out for free.
//
// This is the only way to recover a setup's latest saved version — the
// `Setup` row itself (GET /setups) carries no nested "latest version" field
// on the wire (see the Setup doc comment), so `aq setups`' VERSION column
// and `aq share`'s (setup, version-number) resolution both derive it from
// this list instead of trusting anything nested.
func (c *Client) ListAllSetupVersions() ([]SetupVersion, error) {
	var out []SetupVersion
	if err := c.getJSON("/setups/versions", &out); err != nil {
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

// InstallPreviewStartupScript mirrors InstallPreviewDTO's nested
// startupScript — willRun only, NEVER the script's content.
type InstallPreviewStartupScript struct {
	WillRun bool    `json:"willRun"`
	Source  *string `json:"source"`
}

// InstallPreviewHardware mirrors InstallPreviewDTO's suggestedHardware — the
// version author's own box, offered as a SUGGESTION only, never enforced.
type InstallPreviewHardware struct {
	GPU      *string `json:"gpu"`
	GPUCount *int    `json:"gpuCount"`
	CPU      *int    `json:"cpu"`
	Memory   *string `json:"memory"`
	Storage  *string `json:"storage"`
}

// InstallPreviewVRAM mirrors InstallPreviewDTO's peakVram. Three-state on the
// wire — nil (not a zeroed struct) when the author's last save never
// observed a peak.
type InstallPreviewVRAM struct {
	PeakMB  int  `json:"peakMb"`
	TotalMB *int `json:"totalMb"`
	Pegged  bool `json:"pegged"`
}

// InstallPreviewResult mirrors InstallPreviewDTO (snapshot-version.service.ts)
// exactly — a hand-written object literal, camelCase like the Setup DTO, not
// run through the snake_case DTO helpers SetupVersion's own routes use. It is
// everything a prospective installer needs to see BEFORE renting hardware:
// no startup-script CONTENT, no secrets, no ancestry — every finding here is
// a warning, never a gate.
type InstallPreviewResult struct {
	ID                int                         `json:"id"`
	Name              string                      `json:"name"`
	Version           int                         `json:"version"`
	Provenance        string                      `json:"provenance"`
	HasRecipe         bool                        `json:"hasRecipe"`
	Template          *string                     `json:"template"`
	Image             *string                     `json:"image"`
	Ports             []int                       `json:"ports"`
	HasAppURL         bool                        `json:"hasAppUrl"`
	HasSecureURL      bool                        `json:"hasSecureUrl"`
	StartupScript     InstallPreviewStartupScript `json:"startupScript"`
	SuggestedHardware *InstallPreviewHardware     `json:"suggestedHardware"`
	PeakVRAM          *InstallPreviewVRAM         `json:"peakVram"`
	Warnings          []string                    `json:"warnings"`
}

// GetSetupVersionInstallPreview fetches GET /setups/versions/:id/install-preview.
// gpuModel/gpuCount/vramMB are the caller's PROPOSED target hardware — optional
// (pass "" / 0 / 0 to omit); when given, the server compares them against the
// recipe's own observation and returns any mismatch in Warnings.
func (c *Client) GetSetupVersionInstallPreview(versionRowID int, gpuModel string, gpuCount, vramMB int) (*InstallPreviewResult, error) {
	path := "/setups/versions/" + strconv.Itoa(versionRowID) + "/install-preview"
	q := url.Values{}
	if gpuModel != "" {
		q.Set("gpu", gpuModel)
	}
	if gpuCount > 0 {
		q.Set("gpu_count", strconv.Itoa(gpuCount))
	}
	if vramMB > 0 {
		q.Set("vram_mb", strconv.Itoa(vramMB))
	}
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	var out InstallPreviewResult
	if err := c.getJSON(path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// InstallSetupVersionRequest is the body of POST /setups/versions/:id/install
// ("provision FROM the recipe"). The caller supplies only what is genuinely
// theirs — their SSH key and their hardware choice; everything describing the
// workload (image/template/ports/startup script) comes from the recipe.
type InstallSetupVersionRequest struct {
	SSHKeyID   string  `json:"ssh_key_id"`
	Name       string  `json:"name,omitempty"`
	GPUModel   string  `json:"gpu_model,omitempty"`
	MaxPrice   float64 `json:"max_price,omitempty"`
	Provider   string  `json:"provider,omitempty"`
	ShareToken string  `json:"share_token,omitempty"`
}

// InstallSetupVersionResult is the data returned by POST
// /setups/versions/:id/install: a fresh deployment shaped by the recipe. This
// does NOT restore the version's bytes yet — poll the deployment until
// active, then call RunSetupVersion against it.
type InstallSetupVersionResult struct {
	DeploymentID int    `json:"deployment_id"`
	ProjectID    string `json:"project_id"`
}

// InstallSetupVersion provisions a new deployment shaped by versionRowID's
// recipe. See InstallSetupVersionResult's doc comment for the required
// poll-then-run follow-up.
func (c *Client) InstallSetupVersion(versionRowID int, req InstallSetupVersionRequest) (*InstallSetupVersionResult, error) {
	var out InstallSetupVersionResult
	path := "/setups/versions/" + strconv.Itoa(versionRowID) + "/install"
	if err := c.postJSON(path, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RunSetupVersionRequest is the body of POST /setups/versions/:id/run —
// install a version onto hardware the caller already rented (their own, via
// InstallSetupVersion above, or any other deployment they own).
type RunSetupVersionRequest struct {
	TargetDeploymentID int    `json:"target_deployment_id"`
	ShareToken         string `json:"share_token,omitempty"`
}

// RunSetupVersionCompatibility mirrors the run response's nested
// compatibility object — non-blocking findings only. A mismatch never
// refuses the run; it only ever lands here.
type RunSetupVersionCompatibility struct {
	Warnings []string `json:"warnings"`
}

// RunSetupVersionResult is the data returned by POST
// /setups/versions/:id/run.
type RunSetupVersionResult struct {
	Message       string                       `json:"message"`
	Compatibility RunSetupVersionCompatibility `json:"compatibility"`
}

// RunSetupVersion restores versionRowID's bytes onto targetDeploymentID —
// the second half of the install → poll → run sequence for launching an
// imported (or any other) setup version onto fresh hardware.
func (c *Client) RunSetupVersion(versionRowID int, req RunSetupVersionRequest) (*RunSetupVersionResult, error) {
	var out RunSetupVersionResult
	path := "/setups/versions/" + strconv.Itoa(versionRowID) + "/run"
	if err := c.postJSON(path, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ShareSetupVersionResult is the data returned by POST
// /setups/versions/:id/share — a bare share TOKEN plus the optional name/
// expiry the caller passed. The orchestrator's `createVersionShare`
// (snapshot-version.service.ts) never returns a URL — the console builds the
// public link itself, client-side, as `<console origin>/launch/<token>` (see
// its AccessPopover.tsx). URL below is filled in here the same way, so every
// caller of this method still gets a ready-to-paste link.
type ShareSetupVersionResult struct {
	Token     string  `json:"token"`
	Name      *string `json:"name"`
	ExpiresAt *string `json:"expires_at"`
	// URL is never present on the wire — see the type doc above. Populated
	// by ShareSetupVersion from Token before returning.
	URL string `json:"-"`
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
	out.URL = config.ConsoleURL() + "/launch/" + out.Token
	return &out, nil
}

// ForkSetupRequest is the body of POST /setups/fork — a live share TOKEN
// (from ShareSetupVersion/`aq share`, or one pulled out of a pasted
// /launch/<token> link) and an optional display name for the new setup.
// Same shape as adoptSetupSchema for the same reason (setups.schemas.ts).
type ForkSetupRequest struct {
	Token string `json:"token"`
	Name  string `json:"name,omitempty"`
}

// ForkSetup turns a live share token into a brand new Setup the caller owns,
// filed under their own team — the consuming half of `aq share`'s link.
// Forking your own team's own version is refused server-side (pointless
// empty copy of something you can already read/save/run directly).
func (c *Client) ForkSetup(req ForkSetupRequest) (*Setup, error) {
	var out Setup
	if err := c.postJSON("/setups/fork", req, &out); err != nil {
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

// SetupAutopauseRequest is the body of PUT /setups/:id/autopause.
type SetupAutopauseRequest struct {
	Enabled bool `json:"enabled"`
}

// SetSetupAutopause sets a setup's per-setup autopause PREFERENCE explicitly
// (on or off), returning the updated Setup row.
//
// This is NOT the same mechanism as `aq idle`: idle policy is a
// PER-DEPLOYMENT threshold config (warn/stop-after minutes, GPU idle %) that
// always outranks whatever this sets (see idlePolicyFor in the
// orchestrator's idle.config.ts, which layers Setup.autopauseEnabled in
// underneath it). Autopause carries no thresholds of its own — it only says
// "stop this setup's box when it goes idle, using the platform's default
// thresholds." There is also no verb to clear it back to "unset" — a setup
// that never calls this route simply follows the platform default
// (DEFAULT_IDLE_POLICY.autoStopEnabled, currently off).
func (c *Client) SetSetupAutopause(setupID string, enabled bool) (*Setup, error) {
	var out Setup
	path := "/setups/" + url.PathEscape(setupID) + "/autopause"
	if err := c.putJSON(path, SetupAutopauseRequest{Enabled: enabled}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetupForceDetachResult is the data returned by POST
// /setups/:id/force-detach.
type SetupForceDetachResult struct {
	WasSyncing bool `json:"wasSyncing"`
}

// ForceDetachSetup breaks a setup's lease even mid-sync, discarding any work
// written since the last COMPLETED sync. acknowledgeDataLoss:true is sent
// unconditionally — the orchestrator refuses the call without it
// (forceDetachSetupSchema), and callers of this method (aq's `force-detach`
// command) are expected to have gotten the user's explicit --yes first;
// there is no silent/partial form of this call.
func (c *Client) ForceDetachSetup(setupID string) (*SetupForceDetachResult, error) {
	var out SetupForceDetachResult
	path := "/setups/" + url.PathEscape(setupID) + "/force-detach"
	body := map[string]bool{"acknowledgeDataLoss": true}
	if err := c.postJSON(path, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetupSyncResult is the data returned by POST /setups/:id/sync. Unlike the
// Setup DTO above, this really is ogre's own snake_case wire shape
// (SetupSyncResponse in the orchestrator's types/setup.types.ts), passed
// through unmodified — not the hand-written serializeSetup object literal.
type SetupSyncResult struct {
	SnapshotID      string `json:"snapshot_id"`
	FilesNew        int    `json:"files_new,omitempty"`
	FilesChanged    int    `json:"files_changed,omitempty"`
	DataAddedPacked int64  `json:"data_added_packed,omitempty"`
}

// SyncSetupNow forces a sync tick right now, outside the setup's own
// scheduled interval. The setup must currently be attached to a running
// deployment — the orchestrator 400s otherwise with a message naming that.
func (c *Client) SyncSetupNow(setupID string) (*SetupSyncResult, error) {
	var out SetupSyncResult
	path := "/setups/" + url.PathEscape(setupID) + "/sync"
	if err := c.postJSON(path, struct{}{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateSnapshotVersionRequest is the body of PATCH /setups/versions/:id
// (updateSnapshotVersionSchema, `.strict()` — only these three fields are
// accepted). Label/Description are nil-omitted pointers so an unset flag
// never overwrites the existing value; there is currently no way to send an
// explicit `null` to CLEAR one back to empty (the schema allows it, but
// distinguishing "not touched" from "clear to null" needs more than a plain
// omitempty pointer, since both render as an absent key — deliberately left
// unsupported rather than guessed at).
// Visibility is a plain non-nullable enum string when set.
type UpdateSnapshotVersionRequest struct {
	Label       *string `json:"label,omitempty"`
	Description *string `json:"description,omitempty"`
	Visibility  string  `json:"visibility,omitempty"`
}

// UpdateSnapshotVersion edits a saved version's label, description, and/or
// visibility (private/team/public) — the same three fields the console's
// version settings sheet edits.
func (c *Client) UpdateSnapshotVersion(versionRowID int, req UpdateSnapshotVersionRequest) (*SetupVersion, error) {
	var out SetupVersion
	path := "/setups/versions/" + strconv.Itoa(versionRowID)
	if err := c.patchJSON(path, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// setupSizeBytes decodes GET /setups' `sizeBytes` field. serializeSetup
// sends it as a decimal STRING (`ws.sizeBytes.toString()`), or JSON `null`
// for a setup with no measured size yet — never a bare number: BigInt
// doesn't survive JSON.stringify, per that function's own comment, so it
// stringifies explicitly rather than relying on a global replacer. A plain
// int64 field would hard-fail decoding every setup row once the tag below is
// fixed to the real wire key.
type setupSizeBytes int64

func (n *setupSizeBytes) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	*n = setupSizeBytes(v)
	return nil
}

// Setup mirrors one row of GET /setups: what the caller owns, independent of
// whether the underlying compute is currently rented.
//
// ID is a UUID string (`model Setup { id String @id @default(uuid()) ...
// }`) — never the numeric Deployment.ID from control.go, even though a
// running setup has one associated via LeaseDeploymentID. There is also no
// boolean "running" field on the wire — Running derives it from
// LeaseDeploymentID, which is only non-nil while a live deployment holds the
// lease. There is likewise no "latest version" field nested here at all —
// see ListAllSetupVersions.
//
// Every tag below is camelCase, matching serializeSetup's hand-written
// object literal (setups.controller.ts) — see the package doc comment at
// the top of this file for why this struct's convention differs from
// SetupVersion's.
type Setup struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Status          string `json:"status"`
	MountPath       string `json:"mountPath"`
	AutosaveEnabled bool   `json:"autosaveEnabled"`
	// AutopauseEnabled is three-state on the wire: nil = never explicitly
	// chosen (the setup follows the platform default), non-nil = explicitly
	// set true/false. NEVER collapse nil into false when rendering this —
	// see setups.controller.ts's comment on why (it's the whole point of the
	// column).
	AutopauseEnabled  *bool          `json:"autopauseEnabled"`
	SizeBytes         setupSizeBytes `json:"sizeBytes"`
	LastSyncAt        string         `json:"lastSyncAt"`
	LeaseDeploymentID *int           `json:"leaseDeploymentId"`
	CreatedAt         string         `json:"createdAt"`
	UpdatedAt         string         `json:"updatedAt"`
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
