package api

import (
	"net/url"
	"strconv"
)

// Setup-lineage endpoints backing `aq pods`, `aq job point`, and (for the
// still-live SnapshotVersion rows Jobs read, D11 of the pod/environment/
// volume plan) `aq job create`. A "setup" is its own object, distinct from
// the deployment that may currently hold its compute lease (Deployment, in
// control.go), never pass a deployment id where a setup id belongs, or vice
// versa. Everything else the "Setup-lineage" name once described (`aq save`,
// `aq share`, `aq fork`, `aq autopause`, `aq force-detach`, `aq edit-version`
// on a managed pod) is gone under this model: Start/Stop/Move own the
// lifecycle, and Environment/Volume own history and sharing (see
// pods_lifecycle.go, environments.go, volumes.go).
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

// SetupVersion mirrors one row of the setup_versions table, as returned by
// GET /setups/versions[?name=...]. Pods stop WRITING this table under the
// pod/environment/volume model (D11: `aq save` and its route are gone, saves
// mint EnvironmentVersion/VolumePoint rows now), it survives read-only,
// because Jobs still reference a SnapshotVersion as a source
// (`aq job create <pod> <version>`, job.go) and existing rows must stay
// resolvable. There is no "latest version" field nested on Setup itself,
// see ListAllSetupVersions for how `aq pods`/`aq job create`/`aq job point`
// recover a setup's latest/named version instead.
//
// SetupID is a string for the same reason Setup.ID is, see the package doc.
// The version row's own ID is left an int: unlike Setup, nothing in the
// ticket's Prisma excerpt confirms its type, and control.go already shows
// this schema mixing int autoincrement ids (Deployment) with uuid ids
// (Setup) rather than using one convention everywhere. If a live server
// returns a non-numeric version id, GetSetupVersion's request will surface
// that as a clear decode/404 error rather than a silent wrong-row resolve,
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

// ListSetupVersions returns every version row named `name`, GET
// /setups/versions?name=<name>. The name alone is NOT unique to one setup
// (two different setups' lineages can share a chosen name), so a caller that
// needs one particular setup's version must additionally filter the result
// on SetupID. This is `aq job point`'s path from a (setup, version-number)
// pair to the version ROW id a repoint request actually needs: version and
// id are different counters (see GetSetupVersion), and this is the one
// lookup that resolves one to the other instead of guessing.
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
// /setups/versions/:id. `aq job point` uses this to learn which
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

// SetupEnvironmentSummary mirrors the `environment` object the
// pod/environment/volume plan's wire contract (section 2) nests on GET
// /setups and GET /setups/:id. It is always present (never null) — every pod
// has a working environment even before anything is ever Kept or Shared out
// of it. Version is nullable on the wire (int|null): the working environment
// has no minted EnvironmentVersion until the pod's environment is Kept or
// Shared for the first time.
type SetupEnvironmentSummary struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version *int   `json:"version"`
	Kind    string `json:"kind"`
}

// SetupVolumeSummary mirrors the `volume` object nested on GET /setups, GET
// /setups/:id. SaveState is three-state on the wire
// ("saved"|"failing"|"unknown") and must never collapse "unknown" (the agent
// could not be reached) into "saved" — see the workspace's three-state
// signal rule. LastSaveError is nil except while SaveState is "failing".
type SetupVolumeSummary struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	SizeBytes     int64   `json:"sizeBytes"`
	HeadSavedAt   string  `json:"headSavedAt"`
	SaveState     string  `json:"saveState"`
	LastSaveError *string `json:"lastSaveError"`
}

// SetupRestoreProgress mirrors the `restore` object nested on GET /setups,
// GET /setups/:id while a Start is restoring the pod's environment/volume
// onto its box. Nil once the pod reaches Running (`ready_at`) or if it was
// never restoring.
type SetupRestoreProgress struct {
	Phase      string `json:"phase"`
	BytesDone  int64  `json:"bytesDone"`
	BytesTotal int64  `json:"bytesTotal"`
}

// Setup mirrors one row of GET /setups: what the caller owns, independent of
// whether the underlying compute is currently rented.
//
// ID is a UUID string (`model Setup { id String @id @default(uuid()) ...
// }`) — never the numeric Deployment.ID from control.go, even though a
// running setup has one associated via AttachedDeploymentID. There is also
// no boolean "running" field on the wire — Running derives it from
// AttachedDeploymentID, which is only non-nil while a live deployment is
// attached. There is likewise no "latest version" field nested here at all,
// see ListAllSetupVersions.
//
// This struct mirrors the confirmed PodDTO shape (w3-backend,
// pod-serializer.ts, 2026-09-26), not the pre-pod/environment/volume
// serializeSetup shape the doc comment above used to describe: that
// serializer sent `mountPath`, `sizeBytes` and `lastSyncAt` on every row,
// none of which PodDTO sends any more, and reading them here would silently
// decode to a zero value that looks like a real (empty) answer rather than
// an absent field.
//
// Every tag below is camelCase, matching PodDTO field for field.
type Setup struct {
	ID          string                  `json:"id"`
	Name        string                  `json:"name"`
	Status      string                  `json:"status"`
	Environment SetupEnvironmentSummary `json:"environment"`
	// Volume is nil for a pod running with no volume attached (D4 of the
	// pod/environment/volume plan: a bare pod is allowed, and the New pod
	// flow warns about it). Never render a nil Volume as an empty one.
	Volume *SetupVolumeSummary `json:"volume"`
	// Restore is non-nil only while Start is restoring this pod's
	// environment/volume onto its box.
	Restore *SetupRestoreProgress `json:"restore"`
	// AutostopEnabled is three-state on the wire: nil = never explicitly
	// chosen (the pod follows the platform default), non-nil = explicitly
	// set true/false. NEVER collapse nil into false when rendering this —
	// see setups.controller.ts's comment on why (it's the whole point of the
	// column). Renamed from AutopauseEnabled/autopauseEnabled per the
	// pod/environment/volume plan's vocabulary sweep (PUT
	// /setups/:id/autostop replaces PUT /setups/:id/autopause; no alias).
	AutostopEnabled *bool `json:"autostopEnabled"`
	// DeploymentID is populated on POST /setups and POST /setups/:id/start's
	// response (w3-backend, 2026-09-26): the freshly-rented deployment id, so
	// the caller can poll `aq status <id>` while the box restores. Absent on
	// every other response that decodes a Setup (Stop/Move return the pod
	// with no such field), nil there, never a stale or guessed id.
	DeploymentID *int `json:"deploymentId,omitempty"`
	// AttachedDeploymentID is the deployment currently running this pod, nil
	// once Stopped or once that deployment closes. Replaces the pre-model
	// LeaseDeploymentID (the lease moved to Volume and is no longer
	// serialized on the pod itself, w3-backend 2026-09-26): Running() derives
	// from this field alone.
	AttachedDeploymentID *int `json:"attachedDeploymentId"`
	// Stopping is true while a Stop, or the first half of a Move, is in
	// flight: the pod is neither cleanly Running nor cleanly Stopped, and
	// collapsing it into either would misreport an in-progress release as
	// one of its two settled states.
	Stopping  bool   `json:"stopping"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

// Running reports whether a deployment currently holds this setup's lease.
func (s Setup) Running() bool {
	return s.AttachedDeploymentID != nil
}

// ListSetups returns every setup the caller owns.
func (c *Client) ListSetups() ([]Setup, error) {
	var out []Setup
	if err := c.getJSON("/setups", &out); err != nil {
		return nil, err
	}
	return out, nil
}
