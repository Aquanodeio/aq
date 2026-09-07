package api

import "net/url"

// Team-scoped secrets store backing `aq secret` (ticket #1004): env vars and
// private-registry credentials a Job's Runs can reference by NAME at dispatch,
// so a real credential never has to live in plaintext in a Job's `image.env`
// or in a script that built one.
//
// Every route here is team-scoped by an EXPLICIT :teamId path segment, unlike
// every other resource in this package, which infers the team from the
// x-team-id header alone (see NewAuthed). That header is still sent on these
// requests too, but the orchestrator's admin-role check reads teamId from the
// URL. requireTeamID (in the main package) is the one place that decides
// what to put there.
//
// The wire never returns a value, not even the one just written by a create
// or a rotate: Secret below has no field for it, deliberately.

// RegistryCredential is the `value` shape for a `type: "registry"` secret:
// POST /secrets/teams/:teamId's body when Type is "registry", and the shape
// RotateSecretRequest.Value takes for rotating one.
type RegistryCredential struct {
	Server   string `json:"server"`
	Username string `json:"username"`
	Token    string `json:"token"`
}

// Secret mirrors one row returned by the secrets store: the `data` of
// create/rotate, and one entry of list's `data.secrets`. RotatedAt is a
// pointer because "never rotated" is a real, common state (every secret
// starts there), not the zero value of a string.
type Secret struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Type      string  `json:"type"` // "env" | "registry"
	CreatedAt string  `json:"createdAt"`
	UpdatedAt string  `json:"updatedAt"`
	RotatedAt *string `json:"rotatedAt"`
}

// CreateSecretRequest is the body of POST /secrets/teams/:teamId. Value holds
// either a plain string (Type == "env") or a RegistryCredential (Type ==
// "registry"); json.Marshal serializes whichever one the caller put there,
// so the two shapes never both need a field on this struct.
type CreateSecretRequest struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value any    `json:"value"`
}

// CreateSecret stores a new team secret. The response never carries the value
// back, not even this once, only the id/name/type/timestamps.
func (c *Client) CreateSecret(teamID string, req CreateSecretRequest) (*Secret, error) {
	var out Secret
	path := "/secrets/teams/" + url.PathEscape(teamID)
	if err := c.postJSON(path, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// listSecretsResult is GET /secrets/teams/:teamId's `data` envelope: a named
// `secrets` array, not a bare list, so the endpoint can grow siblings later
// without breaking this decode.
type listSecretsResult struct {
	Secrets []Secret `json:"secrets"`
}

// ListSecrets returns every secret on the team, names and metadata only.
// Any team member may call this: it is what `aq job create --secret`
// resolves a name against, not an admin-only operation.
func (c *Client) ListSecrets(teamID string) ([]Secret, error) {
	var out listSecretsResult
	path := "/secrets/teams/" + url.PathEscape(teamID)
	if err := c.getJSON(path, &out); err != nil {
		return nil, err
	}
	return out.Secrets, nil
}

// RotateSecretRequest is the body of POST /secrets/teams/:teamId/:secretId/rotate.
// Unlike CreateSecretRequest it carries no Type: the secret's type never
// changes at rotation, only its stored value, so the caller resends only
// Value shaped for whatever type the secret already is.
type RotateSecretRequest struct {
	Value any `json:"value"`
}

// RotateSecret replaces a secret's stored value in place, keeping its id and
// name. Returns the updated summary with RotatedAt now set.
func (c *Client) RotateSecret(teamID, secretID string, req RotateSecretRequest) (*Secret, error) {
	var out Secret
	path := "/secrets/teams/" + url.PathEscape(teamID) + "/" + url.PathEscape(secretID) + "/rotate"
	if err := c.postJSON(path, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteSecret removes a team secret. Any Job still naming it in `secrets`
// or `image.registrySecret` starts failing that reference at its next
// dispatch: the orchestrator does not cascade-clean Job rows on delete.
func (c *Client) DeleteSecret(teamID, secretID string) error {
	path := "/secrets/teams/" + url.PathEscape(teamID) + "/" + url.PathEscape(secretID)
	return c.deleteJSON(path, nil)
}
