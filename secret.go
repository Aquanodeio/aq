package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// secret dispatches `aq secret <sub>`, the team secrets store (#1004): env
// vars and private-registry credentials a Job's Runs can reference by NAME at
// dispatch, instead of a real credential living in plaintext in a Job's
// `image.env` or a script that built one. A value is only ever written here,
// never read back: `set`/`list`/`rm`/`rotate` cover the whole vocabulary,
// there is no `aq secret get`.
func secret(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: aq secret <set|list|rm|rotate> ...")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "set":
		return secretSet(rest)
	case "list":
		return secretList(rest)
	case "rm":
		return secretRemove(rest)
	case "rotate":
		return secretRotate(rest)
	default:
		return fmt.Errorf("aq secret: unknown subcommand %q, expected one of set, list, rm, rotate", sub)
	}
}

// envSecretNameRE mirrors the orchestrator's own check (secrets.service.ts's
// ENV_KEY_RE): a `type: "env"` secret's name doubles as the env var key a
// Job's Run sees at dispatch, so it must already be a legal identifier.
// Checked here too, not just left to the server's 400, so a bad name fails
// before a network round trip rather than after one.
var envSecretNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// requireTeamID returns the stored credential's team id, or an error naming
// the fix. Every other aq command relies on the x-team-id HEADER alone (the
// orchestrator infers the team from it); the secrets routes are the first to
// also need the id IN THE PATH (`/secrets/teams/:teamId`), which is why this
// check exists only here: an empty TeamID has never before been a command's
// own problem to report.
func requireTeamID(cred *config.Credential) (string, error) {
	if cred == nil || strings.TrimSpace(cred.TeamID) == "" {
		return "", errors.New("no team associated with this login; run `aq login` again")
	}
	return cred.TeamID, nil
}

// flagPassed reports whether the named flag was explicitly given on the
// command line, as opposed to holding its unset default. It is the distinction
// readValueFromFlagOrStdin needs to decide "read stdin" from "the flag's
// value, however it looks", and secretRotate needs to tell one secret shape's
// flags from the other's.
func flagPassed(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// readValueFromFlagOrStdin returns a sensitive value from the given flag when
// `passed` says it was explicitly given, or reads all of stdin otherwise.
// This CLI has no existing convention for entering a secret (no masked
// prompt, no external terminal dependency, see the module's no-external-deps
// rule), so this is the smallest one that avoids the two obvious footguns: a
// flag value lands in shell history and any local process list, so piping it
// in (`aq secret set FOO --type env <<< "$X"`) is offered as the
// alternative, the same way `docker login --password-stdin` does. Only a
// stdin read gets its trailing newline trimmed; a flag value is used
// byte-for-byte, since the shell never adds one.
func readValueFromFlagOrStdin(passed bool, flagValue, flagName string, in io.Reader) (string, error) {
	if passed {
		if flagValue == "" {
			return "", fmt.Errorf("--%s must not be empty", flagName)
		}
		return flagValue, nil
	}
	data, err := io.ReadAll(in)
	if err != nil {
		return "", fmt.Errorf("could not read --%s from stdin: %w", flagName, err)
	}
	v := strings.TrimSuffix(string(data), "\n")
	v = strings.TrimSuffix(v, "\r")
	if v == "" {
		return "", fmt.Errorf("--%s is required (pass it as a flag, or pipe the value on stdin)", flagName)
	}
	return v, nil
}

// secretSetOptions configures runSecretSet. secretSet() fills in the real
// environment; tests run runSecretSet directly.
type secretSetOptions struct {
	cred       *config.Credential
	name       string
	secretType string // "env" | "registry"
	// env shape
	envValue string
	// registry shape
	server, username, token string
	out                     io.Writer
}

// secretSet parses `aq secret set <name> --type env|registry ...` and wires
// the real environment into runSecretSet.
func secretSet(args []string) error {
	fs := flag.NewFlagSet("secret set", flag.ContinueOnError)
	secretType := fs.String("type", "", `secret type: "env" or "registry" (required)`)
	value := fs.String("value", "", "the secret value (type=env); omit to read from stdin")
	server := fs.String("server", "", "registry host, e.g. ghcr.io (type=registry)")
	username := fs.String("username", "", "registry username (type=registry)")
	token := fs.String("token", "", "registry password or PAT (type=registry)")

	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 || positional[0] == "" {
		return errors.New("usage: aq secret set <name> --type env --value <value>\n" +
			"   or: aq secret set <name> --type registry --server <server> --username <user> --token <token>")
	}
	name := positional[0]

	opts := secretSetOptions{name: name, secretType: strings.TrimSpace(*secretType), out: os.Stdout}
	switch opts.secretType {
	case "env":
		if !envSecretNameRE.MatchString(name) {
			return fmt.Errorf("an env secret's name must be a valid env var identifier ([A-Za-z_][A-Za-z0-9_]*), got %q", name)
		}
		v, err := readValueFromFlagOrStdin(flagPassed(fs, "value"), *value, "value", os.Stdin)
		if err != nil {
			return err
		}
		opts.envValue = v
	case "registry":
		if strings.TrimSpace(*server) == "" || strings.TrimSpace(*username) == "" || strings.TrimSpace(*token) == "" {
			return errors.New("--server, --username and --token are all required for a registry secret")
		}
		opts.server, opts.username, opts.token = *server, *username, *token
	case "":
		return errors.New(`--type is required: "env" or "registry"`)
	default:
		return fmt.Errorf(`--type must be "env" or "registry", got %q`, *secretType)
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}
	opts.cred = cred

	return runSecretSet(opts)
}

// runSecretSet creates the secret and prints its id, never its value, which
// the response never carries.
func runSecretSet(opts secretSetOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	teamID, err := requireTeamID(opts.cred)
	if err != nil {
		return err
	}
	client := newControlClient(opts.cred)

	req := api.CreateSecretRequest{Name: opts.name, Type: opts.secretType}
	switch opts.secretType {
	case "env":
		req.Value = opts.envValue
	case "registry":
		req.Value = api.RegistryCredential{Server: opts.server, Username: opts.username, Token: opts.token}
	}

	sec, err := client.CreateSecret(teamID, req)
	if err != nil {
		return fmt.Errorf("could not create secret %q: %w", opts.name, err)
	}

	fmt.Fprintf(out, "✓ Created %s secret %q (id %s)\n", sec.Type, sec.Name, sec.ID)
	return nil
}

// secretListOptions configures runSecretList. secretList() fills in the real
// environment; tests run runSecretList directly.
type secretListOptions struct {
	cred *config.Credential
	out  io.Writer
}

// secretList parses `aq secret list` and wires the real environment into
// runSecretList.
func secretList(args []string) error {
	fs := flag.NewFlagSet("secret list", flag.ContinueOnError)
	if _, err := parseInterspersed(fs, args); err != nil {
		return err
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runSecretList(secretListOptions{cred: cred, out: os.Stdout})
}

// runSecretList fetches and renders the team's secrets: names and metadata
// only, the value is never in this response for any secret, ever.
func runSecretList(opts secretListOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	teamID, err := requireTeamID(opts.cred)
	if err != nil {
		return err
	}
	client := newControlClient(opts.cred)

	list, err := client.ListSecrets(teamID)
	if err != nil {
		return fmt.Errorf("could not list secrets: %w", err)
	}

	printSecrets(out, list)
	return nil
}

// printSecrets renders the secret list as a simple aligned table, matching
// the style printRuns/printDeployments already use elsewhere in this CLI.
func printSecrets(out io.Writer, list []api.Secret) {
	if len(list) == 0 {
		fmt.Fprintln(out, "No secrets yet. Run `aq secret set <name> --type env --value <value>` to create one.")
		return
	}

	fmt.Fprintf(out, "%-36s  %-24s  %-9s  %-24s  %s\n", "ID", "NAME", "TYPE", "CREATED", "ROTATED")
	for _, s := range list {
		rotated := "never"
		if s.RotatedAt != nil && strings.TrimSpace(*s.RotatedAt) != "" {
			rotated = *s.RotatedAt
		}
		fmt.Fprintf(out, "%-36s  %-24s  %-9s  %-24s  %s\n",
			s.ID, truncate(s.Name, 24), s.Type, orDash(s.CreatedAt), rotated)
	}
}

// secretRemoveOptions configures runSecretRemove. secretRemove() fills in the
// real environment; tests run runSecretRemove directly.
type secretRemoveOptions struct {
	cred   *config.Credential
	target string // secret name or id
	out    io.Writer
}

// secretRemove parses `aq secret rm <name-or-id>` and wires the real
// environment into runSecretRemove.
func secretRemove(args []string) error {
	fs := flag.NewFlagSet("secret rm", flag.ContinueOnError)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 || positional[0] == "" {
		return errors.New("usage: aq secret rm <name-or-id>")
	}
	target := positional[0]

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runSecretRemove(secretRemoveOptions{cred: cred, target: target, out: os.Stdout})
}

// runSecretRemove resolves the name-or-id, same pattern host.go/job.go use
// elsewhere in this CLI, then deletes it. A Job still naming it in `secrets`
// or `image.registrySecret` starts failing that reference at its next
// dispatch; deleting here does not touch any Job row.
func runSecretRemove(opts secretRemoveOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	teamID, err := requireTeamID(opts.cred)
	if err != nil {
		return err
	}
	client := newControlClient(opts.cred)

	sec, err := resolveSecret(client, teamID, opts.target)
	if err != nil {
		return err
	}

	if err := client.DeleteSecret(teamID, sec.ID); err != nil {
		return fmt.Errorf("could not remove secret %q: %w", sec.Name, err)
	}

	fmt.Fprintf(out, "✓ Removed secret %q\n", sec.Name)
	return nil
}

// secretRotateOptions configures runSecretRotate. secretRotate() fills in the
// real environment; tests run runSecretRotate directly.
type secretRotateOptions struct {
	cred   *config.Credential
	target string // secret name or id
	// Which of these were passed is read from the flag set at parse time
	// (secretRotate), since runSecretRotate has no flag.FlagSet of its own:
	// it only learns the secret's TYPE after resolving it, network-side.
	valuePassed             bool
	value                   string
	server, username, token string
	in                      io.Reader
	out                     io.Writer
}

// secretRotate parses `aq secret rotate <name-or-id> ...` and wires the real
// environment into runSecretRotate. Mirrors secretSet's input flags, but does
// not take --type: a rotation keeps the secret's existing type, it can only
// replace the value stored under it.
func secretRotate(args []string) error {
	fs := flag.NewFlagSet("secret rotate", flag.ContinueOnError)
	value := fs.String("value", "", "new value for an env secret; omit to read from stdin")
	server := fs.String("server", "", "new registry host for a registry secret")
	username := fs.String("username", "", "new registry username for a registry secret")
	token := fs.String("token", "", "new registry password or PAT for a registry secret")

	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 || positional[0] == "" {
		return errors.New("usage: aq secret rotate <name-or-id> --value <value>\n" +
			"   or: aq secret rotate <name-or-id> --server <server> --username <user> --token <token>")
	}
	target := positional[0]

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runSecretRotate(secretRotateOptions{
		cred:        cred,
		target:      target,
		valuePassed: flagPassed(fs, "value"),
		value:       *value,
		server:      *server,
		username:    *username,
		token:       *token,
		in:          os.Stdin,
		out:         os.Stdout,
	})
}

// runSecretRotate resolves the secret first: its stored TYPE decides which
// shape the new value must take, so a registry secret rotated with --value,
// or an env one rotated with --server/--username/--token, is refused by name
// rather than sent as a guess at whichever shape happened to be filled in.
func runSecretRotate(opts secretRotateOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}
	in := opts.in
	if in == nil {
		in = os.Stdin
	}

	teamID, err := requireTeamID(opts.cred)
	if err != nil {
		return err
	}
	client := newControlClient(opts.cred)

	sec, err := resolveSecret(client, teamID, opts.target)
	if err != nil {
		return err
	}

	registryFlagsPassed := opts.server != "" || opts.username != "" || opts.token != ""

	req := api.RotateSecretRequest{}
	switch sec.Type {
	case "env":
		if registryFlagsPassed {
			return fmt.Errorf("secret %q is type env; use --value, not --server/--username/--token", sec.Name)
		}
		value, err := readValueFromFlagOrStdin(opts.valuePassed, opts.value, "value", in)
		if err != nil {
			return err
		}
		req.Value = value
	case "registry":
		if opts.valuePassed {
			return fmt.Errorf("secret %q is type registry; use --server/--username/--token, not --value", sec.Name)
		}
		if strings.TrimSpace(opts.server) == "" || strings.TrimSpace(opts.username) == "" || strings.TrimSpace(opts.token) == "" {
			return fmt.Errorf("--server, --username and --token are all required to rotate registry secret %q", sec.Name)
		}
		req.Value = api.RegistryCredential{Server: opts.server, Username: opts.username, Token: opts.token}
	default:
		return fmt.Errorf("secret %q has an unrecognized type %q; upgrade aq or use the console", sec.Name, sec.Type)
	}

	updated, err := client.RotateSecret(teamID, sec.ID, req)
	if err != nil {
		return fmt.Errorf("could not rotate secret %q: %w", sec.Name, err)
	}

	fmt.Fprintf(out, "✓ Rotated %s secret %q (id %s)\n", updated.Type, updated.Name, updated.ID)
	return nil
}
