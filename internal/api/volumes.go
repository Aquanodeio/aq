package api

import "net/url"

// Volume endpoints for the pod/environment/volume model. A Volume is
// /workspace: code, checkpoints, datasets. It has automatic history (one
// VolumePoint per Stop) and no manual save button: Duplicate, Restore a
// point, and Delete are the only mutating verbs here (`aq volume`); Attach
// happens implicitly by naming the volume on a pod's Start, not a standalone
// call this client exposes.

// VolumePoint is one row of a volume's history, as nested under Volume's own
// `points` field. Provenance is three-state-ish on the wire ("stop" |
// "idle_stop"), never invented client-side; Label is nil except on a point
// migrated from a named legacy SnapshotVersion (D10).
type VolumePoint struct {
	ID         string  `json:"id"`
	CreatedAt  string  `json:"createdAt"`
	Provenance string  `json:"provenance"`
	Label      *string `json:"label"`
}

// Volume mirrors one row of GET /volumes, GET /volumes/:id (confirmed
// against orchestrator/src/services/volumes/volume.service.ts's VolumeDTO,
// w3-backend 2026-09-26). Points is only populated on the single-volume
// fetch; a list-all response may leave it empty, which callers must render
// as "no history yet", never as an error.
//
// SizeBytes and HeadSavedAt are both nullable (never a bare number/string on
// the wire): SizeBytes is null before storage metering has ever measured
// this volume's prefix, and HeadSavedAt is null before its first save ever
// lands. Neither must ever decode to a bare zero value that reads as a real
// answer ("0 B held" / some real timestamp) when the true state is "not
// measured yet" / "never saved yet".
//
// AttachedPodID is nil for an unattached volume (Duplicate's target, or one
// explicitly detached), never render a nil as "attached to nothing in
// particular" vs. an empty string, the two read identically to a user but
// only one of them is a real distinction the wire makes. AttachedPodID
// reflects the STRUCTURAL relationship (this volume is pod X's volume)
// regardless of whether pod X is currently running: Delete refuses on
// AttachedPodID alone, but Restore a point refuses only when Running is
// also true (a stopped pod's volume can still be restored). Never conflate
// the two: "attached" and "running" are different gates on different verbs.
type Volume struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	SizeBytes       *int64  `json:"sizeBytes"`
	MountPath       string  `json:"mountPath"`
	AttachedPodID   *string `json:"attachedPodId"`
	AttachedPodName *string `json:"attachedPodName"`
	Running         bool    `json:"running"`
	HeadSavedAt     *string `json:"headSavedAt"`
	// SaveState is three-state on the wire ("saved"|"failing"|"unknown"),
	// same rule as SetupVolumeSummary: never collapse "unknown" into
	// "saved".
	SaveState     string        `json:"saveState"`
	LastSaveError *string       `json:"lastSaveError"`
	CreatedAt     string        `json:"createdAt"`
	Points        []VolumePoint `json:"points"`
}

// ListVolumes returns every volume the caller owns, GET /volumes.
func (c *Client) ListVolumes() ([]Volume, error) {
	var out []Volume
	if err := c.getJSON("/volumes", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetVolume fetches one volume's detail, including its full point history,
// GET /volumes/:id.
func (c *Client) GetVolume(volumeID string) (*Volume, error) {
	var out Volume
	path := "/volumes/" + url.PathEscape(volumeID)
	if err := c.getJSON(path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DuplicateVolumeRequest is the body of POST /volumes/:id/duplicate.
type DuplicateVolumeRequest struct {
	Name string `json:"name"`
}

// DuplicateVolume forks a volume into a brand new one at its latest point,
// an honest fork, writes never merge back. No server-side bucket copy is
// implied by this call's shape; it may take a moment to become attachable.
func (c *Client) DuplicateVolume(volumeID string, name string) (*Volume, error) {
	var out Volume
	path := "/volumes/" + url.PathEscape(volumeID) + "/duplicate"
	if err := c.postJSON(path, DuplicateVolumeRequest{Name: name}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RestoreVolumePointRequest is the body of POST /volumes/:id/restore.
type RestoreVolumePointRequest struct {
	PointID string `json:"pointId"`
}

// RestoreVolumePoint sets a volume's head to an earlier point, POST
// /volumes/:id/restore. Refused (409) while the volume is attached to a
// running pod (Running, not merely AttachedPodID != nil: a stopped pod's
// volume can still be restored); the server never interprets "restore" as
// "newest by time".
func (c *Client) RestoreVolumePoint(volumeID string, pointID string) (*Volume, error) {
	var out Volume
	path := "/volumes/" + url.PathEscape(volumeID) + "/restore"
	if err := c.postJSON(path, RestoreVolumePointRequest{PointID: pointID}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteVolume deletes a volume and its whole history, DELETE /volumes/:id.
// Refused (409) while attached to any pod (AttachedPodID != nil), running
// or not, unlike RestoreVolumePoint.
func (c *Client) DeleteVolume(volumeID string) error {
	path := "/volumes/" + url.PathEscape(volumeID)
	return c.deleteJSON(path, nil)
}
