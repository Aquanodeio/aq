package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
)

// Volume endpoints for the pod/environment/volume model. A Volume is
// /workspace: code, checkpoints, datasets. It has automatic history (one
// VolumePoint per Stop) and no manual save button: Duplicate, Restore a
// point, and Delete are the only mutating verbs here (`aq volume`); Attach
// happens implicitly by naming the volume on a pod's Start, not a standalone
// call this client exposes.
//
// Any number of pods may REFERENCE a volume; at most one RUNNING pod writes
// it at a time (single writer, enforced at Start before anything is
// rented). A stopped pod blocks nothing: its volume can be picked by
// another pod, rolled back, or deleted. "In use" therefore means exactly
// one thing (InUseBy below): a pod's live box has it right now, never merely
// that some pod references it.

// VolumeHolder is the pod whose live box has a volume right now (`inUseBy`).
// PodID and PodName are BOTH nullable, and nil together for the same reason:
// a volume's write lease can outlive the pod row that claimed it, so a
// non-nil VolumeHolder with a nil PodName means "some pod holds it, but no
// pod row could be found for it", never "attached to nothing in
// particular". Check the *VolumeHolder pointer itself for "not in use", not
// these fields.
type VolumeHolder struct {
	PodID   *string `json:"podId"`
	PodName *string `json:"podName"`
}

// VolumeUser is one pod that references a volume (`usedBy`, GET
// /volumes/:id only), running or not: any number of pods may reference one,
// and State is what tells them apart from the single pod actually writing
// it (VolumeHolder above).
type VolumeUser struct {
	PodID   string `json:"podId"`
	PodName string `json:"podName"`
	State   string `json:"state"` // "running" | "stopped"
}

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
// against orchestrator/src/services/volumes/volume.service.ts's VolumeDTO/
// VolumeDetailDTO, w3-backend PR #811, 2026-09-28). UsedBy and Points are
// only populated on the single-volume fetch; a list-all response leaves
// both empty, which callers must render as "no history yet" / nothing to
// list, never as an error.
//
// SizeBytes and HeadSavedAt are both nullable (never a bare number/string on
// the wire): SizeBytes is null before storage metering has ever measured
// this volume's prefix, and HeadSavedAt is null before its first save ever
// lands. Neither must ever decode to a bare zero value that reads as a real
// answer ("0 B held" / some real timestamp) when the true state is "not
// measured yet" / "never saved yet".
//
// InUseBy replaces the pre-model AttachedPodID/AttachedPodName/Running:
// under the one-pod-per-volume model those three fields conflated
// "references this volume" with "is writing it right now"; under this model
// many pods may reference a volume; delete, never alias. InUseBy alone
// answers "is writing it right now" (nil = no); Delete refuses on it alone.
// UsedBy (detail only) lists every REFERENCING pod, running or not.
type Volume struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	SizeBytes   *int64  `json:"sizeBytes"`
	HeadSavedAt *string `json:"headSavedAt"`
	// SaveState is three-state on the wire ("saved"|"failing"|"never_saved"
	// -- "never_saved" renamed from "unknown" in #811, same field, same
	// meaning: nothing has been saved yet, not a warning). Never collapse
	// "never_saved" into "saved"; same rule as SetupVolumeSummary.
	SaveState     string        `json:"saveState"`
	LastSaveError *string       `json:"lastSaveError"`
	MountPath     string        `json:"mountPath"`
	InUseBy       *VolumeHolder `json:"inUseBy"`
	// Copying is true while this volume's bytes are still landing
	// server-side (a Duplicate's target, or a share link's files copying
	// in), bounded to a few hours. Never attach/restore/rely on its data
	// until this is false.
	Copying   bool   `json:"copying"`
	CreatedAt string `json:"createdAt"`
	// UsedBy is only populated by GetVolume (GET /volumes/:id); empty on a
	// ListVolumes row, same rule as Points.
	UsedBy []VolumeUser  `json:"usedBy"`
	Points []VolumePoint `json:"points"`
}

// ListVolumes returns every volume the caller owns, GET /volumes.
func (c *Client) ListVolumes() ([]Volume, error) {
	var out []Volume
	if err := c.getJSON("/volumes", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetVolume fetches one volume's detail, including who references it and
// its full point history, GET /volumes/:id.
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
// an honest fork, writes never merge back. The copy lands server-side
// (Copying is true on the result until it does); it may take a moment to
// become attachable.
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
// /volumes/:id/restore. Refused (409) while a pod's live box has the volume
// (InUseBy, not merely "some pod references it": a stopped pod's volume can
// still be restored); the server never interprets "restore" as "newest by
// time".
func (c *Client) RestoreVolumePoint(volumeID string, pointID string) (*Volume, error) {
	var out Volume
	path := "/volumes/" + url.PathEscape(volumeID) + "/restore"
	if err := c.postJSON(path, RestoreVolumePointRequest{PointID: pointID}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DetachedPod is one pod DeleteVolume detached because it referenced the
// deleted volume while STOPPED. A running pod's volume is never detached
// this way; DELETE refuses outright instead (VolumeInUseError).
type DetachedPod struct {
	PodID   string `json:"podId"`
	PodName string `json:"podName"`
}

// DeleteVolumeResult is the data of a successful DELETE /volumes/:id.
type DeleteVolumeResult struct {
	ID           string        `json:"id"`
	DetachedPods []DetachedPod `json:"detachedPods"`
}

// VolumeInUseError is DELETE /volumes/:id's 409 while a pod's live box has
// the volume (single writer). InUseBy names the holder in the same shape
// Volume.InUseBy carries; never blind-retry this delete, stop that pod
// first (or wait for it to stop).
type VolumeInUseError struct {
	Message string
	InUseBy *VolumeHolder
}

func (e *VolumeInUseError) Error() string { return e.Message }

// DeleteVolume deletes a volume and its whole history, DELETE /volumes/:id.
// Refused as a *VolumeInUseError while a pod's live box has it; otherwise
// every STOPPED pod that referenced it is detached (its config and history
// untouched, just pointed at no volume) and returned in DetachedPods.
func (c *Client) DeleteVolume(volumeID string) (*DeleteVolumeResult, error) {
	var out DeleteVolumeResult
	path := "/volumes/" + url.PathEscape(volumeID)
	if err := c.deleteJSON(path, &out); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict && len(apiErr.Data) > 0 {
			var body struct {
				InUseBy *VolumeHolder `json:"inUseBy"`
			}
			if jsonErr := json.Unmarshal(apiErr.Data, &body); jsonErr == nil && body.InUseBy != nil {
				return nil, &VolumeInUseError{Message: apiErr.Message, InUseBy: body.InUseBy}
			}
		}
		return nil, err
	}
	return &out, nil
}
