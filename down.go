package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// downOptions configures runDown. down() fills in the real environment; tests
// inject a base URL and a buffer writer.
type downOptions struct {
	cred   *config.Credential
	target string // deployment id, name, or project id (resolved by runDown)
	out    io.Writer
	errOut io.Writer
}

// down parses the deployment target and wires the real environment into runDown.
//
// `aq down <deploymentId>` tears down an env brought up by `aq up` / `aq deploy`,
// stopping the rented GPU box and its billing.
func down(args []string) error {
	target, err := parseDeploymentTarget(args, "down")
	if err != nil {
		return err
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runDown(downOptions{
		cred:   cred,
		target: target,
		out:    os.Stdout,
		errOut: os.Stderr,
	})
}

// runDown requests termination of the deployment and reports the outcome.
func runDown(opts downOptions) error {
	if opts.out == nil {
		opts.out = os.Stdout
	}
	if opts.errOut == nil {
		opts.errOut = os.Stderr
	}

	client := newControlClient(opts.cred)

	deploymentID, err := resolveDeploymentID(client, opts.target, "down")
	if err != nil {
		return err
	}

	// Read the box's address before closing it — the ssh_config and known_hosts
	// entries are keyed by it, and the row stops reporting one after teardown.
	var host, port string
	if res, err := client.DeploymentStatus(deploymentID); err == nil {
		host, port, _ = sshEndpointFor(res.Deployment)
	}

	if _, err := client.CloseDeployment(deploymentID); err != nil {
		return fmt.Errorf("could not close deployment #%d: %w", deploymentID, err)
	}

	fmt.Fprintf(opts.out, "✓ Termination requested for deployment #%d — the box will stop shortly.\n", deploymentID)

	// Drop the box's alias explicitly rather than relying on the list: teardown
	// is asynchronous, so it still reports as live for a while yet.
	syncManagedConfigQuiet(client, opts.errOut, nil, deploymentID)
	if host != "" {
		if err := removeHost(host, port); err != nil {
			fmt.Fprintf(opts.errOut, "warning: could not prune the known_hosts entry for %s: %v\n", host, err)
		}
	}
	return nil
}

// newControlClient builds an authenticated API client from a stored credential,
// defaulting the base URL to the configured Aquanode API.
func newControlClient(cred *config.Credential) *api.Client {
	apiURL := cred.APIURL
	if apiURL == "" {
		apiURL = config.APIURL()
	}
	return api.NewAuthed(apiURL, cred.Token, cred.TeamID)
}

// parseInterspersed parses fs while allowing flags and positional arguments to
// appear in any order. The stdlib flag package stops at the first non-flag
// token, so `aq status 4242 --show-secrets` would otherwise leave the flag
// unparsed; this collects positionals and keeps parsing the remainder.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		rest = fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		rest = rest[1:]
	}
}

// parseDeploymentTarget reads a single positional `aq <verb> <id>` token. It is
// either the numeric deployment id shown by `aq up`/`aq deploy`/the console, or
// a project id — resolveDeploymentID tells the two apart.
func parseDeploymentTarget(args []string, verb string) (string, error) {
	if len(args) == 0 || args[0] == "" {
		return "", fmt.Errorf("a deployment id is required — usage: aq %s <deploymentId>", verb)
	}
	return args[0], nil
}

// resolveDeploymentID turns a positional token into a numeric deployment id:
//
//	""        → the team's single live deployment (`aq ssh` with nothing else up)
//	numeric   → the deployment id itself
//	name      → the --name given to `aq up`/`aq deploy`, matched case-insensitively
//	otherwise → a project id (the UUID in the console URL), resolved via the API
//
// So a user who pastes a project id gets the box they meant instead of a cryptic
// "expected a number" error (#209), and one who named their box can address it
// the way they named it (#422).
func resolveDeploymentID(client *api.Client, target, verb string) (int, error) {
	if target == "" {
		return resolveSoleLiveDeployment(client, verb)
	}

	if id, err := strconv.Atoi(target); err == nil {
		if id <= 0 {
			return 0, fmt.Errorf("invalid deployment id %q — expected a positive number", target)
		}
		return id, nil
	}

	// A project id is a UUID, which no realistic display name collides with, so
	// checking its shape first spares the UUID path a full history download.
	if looksLikeUUID(target) {
		if id, err := resolveProjectDeployment(client, target, verb); err == nil {
			return id, nil
		}
	}

	id, nameErr := resolveDeploymentByName(client, target)
	if nameErr == nil {
		return id, nil
	}
	var ambiguous *ambiguousNameError
	if errors.As(nameErr, &ambiguous) {
		return 0, ambiguous
	}

	return resolveProjectDeployment(client, target, verb)
}

// resolveProjectDeployment resolves a project id to its current deployment.
func resolveProjectDeployment(client *api.Client, target, verb string) (int, error) {
	dep, err := client.GetProjectDeployment(target)
	if err != nil {
		var apiErr *api.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return 0, fmt.Errorf("no active deployment found for %q — pass the name you gave `--name`, or the numeric deployment id (e.g. aq %s 4242) shown by `aq up`/`aq deploy` or the console", target, verb)
		}
		return 0, fmt.Errorf("could not resolve %q to a deployment: %w", target, err)
	}
	if dep.ID <= 0 {
		return 0, fmt.Errorf("no active deployment found for project %q — pass the numeric deployment id (e.g. aq %s 4242)", target, verb)
	}
	return dep.ID, nil
}

// ambiguousNameError reports a name matching more than one live deployment.
type ambiguousNameError struct {
	name    string
	matches []api.Deployment
}

func (e *ambiguousNameError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%q matches %d deployments — pass one of these ids instead:", e.name, len(e.matches))
	for _, d := range e.matches {
		fmt.Fprintf(&b, "\n  %d  %s  (%s)", d.ID, d.Name, d.Status)
	}
	return b.String()
}

// resolveDeploymentByName matches a display name against the team's live
// deployments, case-insensitively.
//
// Deployment names carry no server-side uniqueness constraint, so a name
// matching several boxes is an error listing the candidates rather than a
// silent pick — silently picking would let `aq down staging` terminate the
// wrong box.
func resolveDeploymentByName(client *api.Client, name string) (int, error) {
	live, err := liveDeployments(client)
	if err != nil {
		return 0, err
	}
	var matches []api.Deployment
	for _, d := range live {
		if strings.EqualFold(strings.TrimSpace(d.Name), name) {
			matches = append(matches, d)
		}
	}
	switch len(matches) {
	case 0:
		return 0, fmt.Errorf("no live deployment named %q", name)
	case 1:
		return matches[0].ID, nil
	default:
		return 0, &ambiguousNameError{name: name, matches: matches}
	}
}

// resolveSoleLiveDeployment picks the team's only live deployment, so a bare
// `aq ssh` just connects when there is nothing to disambiguate.
func resolveSoleLiveDeployment(client *api.Client, verb string) (int, error) {
	live, err := liveDeployments(client)
	if err != nil {
		return 0, err
	}
	switch len(live) {
	case 0:
		return 0, errors.New("no live deployments — run `aq up` to start one")
	case 1:
		return live[0].ID, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d deployments are live — name one, e.g. `aq %s %d`:", len(live), verb, live[0].ID)
	for _, d := range live {
		name := strings.TrimSpace(d.Name)
		if name == "" {
			name = "(unnamed)"
		}
		fmt.Fprintf(&b, "\n  %d  %s  (%s)", d.ID, name, d.Status)
	}
	return 0, errors.New(b.String())
}

// liveDeployments returns the team's non-terminal deployments. GET /deployments
// applies no filters and returns the team's whole history, so the filtering is
// necessarily client-side.
func liveDeployments(client *api.Client) ([]api.Deployment, error) {
	deps, err := client.ListDeployments()
	if err != nil {
		return nil, fmt.Errorf("could not list deployments: %w", err)
	}
	var live []api.Deployment
	for _, d := range deps {
		if d.ID > 0 && !isClosedStatus(d.Status) {
			live = append(live, d)
		}
	}
	return live, nil
}

// looksLikeUUID reports whether s has the 8-4-4-4-12 hex shape of a project id.
func looksLikeUUID(s string) bool {
	groups := strings.Split(s, "-")
	if len(groups) != 5 {
		return false
	}
	for i, want := range []int{8, 4, 4, 4, 12} {
		if len(groups[i]) != want {
			return false
		}
		for _, r := range groups[i] {
			if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
				return false
			}
		}
	}
	return true
}
