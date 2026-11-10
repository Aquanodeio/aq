package api

import (
	"net/url"
)

// ConnectorField is one credential field the local executor reads for a
// provider. Name is the env var name mjolnir's NewClient for that provider
// reads today (and the key it is stored under in accounts.json); Secret says
// whether aq must prompt without echo.
type ConnectorField struct {
	Name   string `json:"name"`
	Label  string `json:"label,omitempty"`
	Secret bool   `json:"secret"`
}

// ConnectorProvider is one provider the orchestrator can relay to a local
// executor, with the exact fields it needs.
type ConnectorProvider struct {
	Provider    string           `json:"provider"`
	DisplayName string           `json:"displayName,omitempty"`
	Fields      []ConnectorField `json:"fields"`
}

// ConnectorProviders fetches GET /connector/providers: the list of providers
// and their credential fields, held once on the server so aq keeps no copy.
func (c *Client) ConnectorProviders() ([]ConnectorProvider, error) {
	var out struct {
		Providers []ConnectorProvider `json:"providers"`
	}
	if err := c.getJSON("/connector/providers", &out); err != nil {
		return nil, err
	}
	return out.Providers, nil
}

// MjolnirLocalDownloadURL fetches GET /artifacts/mjolnir-local/download-url:
// the same contract as the ogre route (the version arrives as `tag`) (a presigned goreleaser tar.gz, the
// ARCHIVE's sha256, 404 for a platform with no build).
func (c *Client) MjolnirLocalDownloadURL(goos, goarch string) (*OgreDownloadURLResult, error) {
	var out struct {
		OgreDownloadURLResult
		Tag string `json:"tag"`
	}
	path := "/artifacts/mjolnir-local/download-url?os=" + url.QueryEscape(goos) + "&arch=" + url.QueryEscape(goarch)
	if err := c.getJSON(path, &out); err != nil {
		return nil, err
	}
	res := out.OgreDownloadURLResult
	if res.Version == "" {
		res.Version = out.Tag
	}
	return &res, nil
}
