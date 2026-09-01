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
	cred     *config.Credential
	target   string // deployment id, name, or project id (resolved by runDown)
	snapshot bool   // save the deployment before terminating it
	setupID  string // the setup id (uuid) backing target, resolved when snapshot is true
	out      io.Writer
	errOut   io.Writer
}

// down parses flags/the deployment target and wires the real environment into
// downWithCheckpoint.
//
// `aq down <deploymentId>` tears down an env brought up by `aq up` / `aq deploy`,
// stopping the rented GPU box and its billing. `--save` saves it first —
// terminate is skipped entirely if that save fails, so the flag can never
// destroy an unsaved box. Named "--save", not "--snapshot": this flag names
// an ACTION (do a save before stopping), and the action is "Save" everywhere
// else in this rewrite — "snapshot" survives only as the noun for the saved
// artifact itself, e.g. `aq deploy --snapshot <id>` below, which names WHICH
// save to restore, not an action.
func down(args []string) error {
	fs := flag.NewFlagSet("down", flag.ContinueOnError)
	snap := fs.Bool("save", false, "save your setup before terminating")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}

	target, err := parseDeploymentTarget(positional, "down")
	if err != nil {
		return err
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	opts := downOptions{
		cred:     cred,
		target:   target,
		snapshot: *snap,
		out:      os.Stdout,
		errOut:   os.Stderr,
	}

	if opts.snapshot {
		// Resolve to the numeric deployment id up front so the printed restore
		// command (aq deploy --snapshot <id>) is always something `aq deploy`
		// actually accepts, not a --name or a project UUID.
		client := newControlClient(cred)
		id, err := resolveDeploymentID(client, opts.target, "down")
		if err != nil {
			return err
		}
		opts.target = strconv.Itoa(id)

		// The checkpoint save is a setup-scoped call (POST
		// /setups/:id/snapshot), not a deployment-scoped one — map the
		// resolved deployment to the setup whose lease it currently holds
		// rather than ever passing the deployment id where a setup id
		// belongs.
		setupID, err := setupIDForDeployment(client, id)
		if err != nil {
			return err
		}
		opts.setupID = setupID
	}

	return downWithCheckpoint(opts, runSnapshot, runDown)
}

// downWithCheckpoint sequences an optional checkpoint before termination. A
// failed checkpoint ABORTS the terminate: destroying a box whose save just
// failed is the one outcome --save exists to prevent. checkpoint and
// terminate are injected so this control flow is testable without a live box.
func downWithCheckpoint(
	opts downOptions,
	checkpoint func(snapshotOptions) (api.SetupVersion, error),
	terminate func(downOptions) error,
) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	if opts.snapshot {
		fmt.Fprintln(out, "Saving your setup before terminating…")
		res, err := checkpoint(snapshotOptions{
			cred:    opts.cred,
			setupID: opts.setupID,
			pathDir: "/workspace",
		})
		if err != nil {
			return fmt.Errorf("save failed, so the deployment was NOT terminated (it is still running): %w", err)
		}
		fmt.Fprintf(out, "✓ Saved %s v%d\n", res.Name, res.Version)
		defer func() {
			fmt.Fprintf(out, "\nPick up where you left off with:\n  aq deploy --snapshot %s\n", opts.target)
		}()
	} else {
		// Say plainly that nothing is kept, BEFORE the box is gone. A bare
		// `aq down` closes with close_reason USER_REQUEST, which the
		// orchestrator excludes from RESUMABLE_CLOSE_REASONS — the data is
		// unrecoverable, not merely awkward to reach.
		//
		// This is disclosure, not a changed default. --save stays false
		// because a save WRITES BYTES and bytes are billed, and storing
		// something on the user's behalf is the user's call to make (see
		// aquanode-backend orchestrator/src/configs/idle.config.ts). The
		// defect this fixes was the silence, not the default.
		fmt.Fprintln(out, "Terminating without saving: nothing on this box is kept, and it cannot be resumed.")
		fmt.Fprintf(out, "To save your setup before stopping the box, use: aq down --save %s\n", opts.target)
	}

	return terminate(opts)
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

	fmt.Fprintf(opts.out, "✓ Termination requested for deployment #%d. The box will stop shortly.\n", deploymentID)

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
	return api.NewAuthed(resolveAPIURL(cred), cred.Token, cred.TeamID)
}

// resolveAPIURL picks the API base URL for this run: AQ_API_URL if the
// operator set it, else the URL baked into the stored credential, else the
// built-in default.
//
// The env var MUST outrank the credential. `aq login` persists the URL it
// paired against, so a logged-in user — everyone — silently kept talking to
// production no matter what AQ_API_URL said. That made the documented local-dev
// override dead on arrival, and worse than dead: pointing it at a local stack
// LOOKED like it worked and quietly rented a real billable box in prod instead.
// An explicit environment override is the strongest signal of intent there is;
// nothing persisted should beat it.
func resolveAPIURL(cred *config.Credential) string {
	if v := config.APIURLOverride(); v != "" {
		return v
	}
	if cred != nil && cred.APIURL != "" {
		return cred.APIURL
	}
	return config.APIURL()
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
		return "", fmt.Errorf("a deployment id is required, usage: aq %s <deploymentId>", verb)
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
			return 0, fmt.Errorf("invalid deployment id %q, expected a positive number", target)
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
			return 0, fmt.Errorf("no active deployment found for %q; pass the name you gave `--name`, or the numeric deployment id (e.g. aq %s 4242) shown by `aq up`/`aq deploy` or the console", target, verb)
		}
		return 0, fmt.Errorf("could not resolve %q to a deployment: %w", target, err)
	}
	if dep.ID <= 0 {
		return 0, fmt.Errorf("no active deployment found for project %q; pass the numeric deployment id (e.g. aq %s 4242)", target, verb)
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
	fmt.Fprintf(&b, "%q matches %d deployments; pass one of these ids instead:", e.name, len(e.matches))
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
		return 0, errors.New("no live deployments; run `aq up` to start one")
	case 1:
		return live[0].ID, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d deployments are live: name one, e.g. `aq %s %d`:", len(live), verb, live[0].ID)
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
