package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// The general console runs provider calls on THIS machine with the user's own
// keys (spec 2026-11-10-general-console-connected-accounts-design.md, section
// 6). Two files live in the aq config dir, both 0600:
//
//	accounts.json       { "runpod": { "RUNPOD_API_KEY": "...", "label": "work" } }
//	                    exactly the shape `mjolnir local --accounts-file` reads:
//	                    credentials keyed by the env var names each provider's
//	                    NewClient reads, plus an optional "label".
//	accounts-meta.json  { "runpod": { "fingerprint": "sha256:...", "verifiedAt": "..." } }
//	                    aq's own bookkeeping, kept OUT of accounts.json so the
//	                    executor never sees a key it did not define.
//
// A key's value appears in these files and nowhere else: never in argv, never
// in the environment of a child, never in a printed line (fingerprints only).

const (
	accountsFileName     = "accounts.json"
	accountsMetaFileName = "accounts-meta.json"
	accountLabelKey      = "label"
)

// accountEntry is one provider's stored fields: env var name -> value, plus
// accountLabelKey.
type accountEntry map[string]string

type accountsFile map[string]accountEntry

type accountMeta struct {
	Fingerprint string `json:"fingerprint"`
	VerifiedAt  string `json:"verifiedAt"`
}

type accountsMetaFile map[string]accountMeta

func accountsPaths() (accounts, meta string, err error) {
	dir, err := config.Dir()
	if err != nil {
		return "", "", err
	}
	return filepath.Join(dir, accountsFileName), filepath.Join(dir, accountsMetaFileName), nil
}

// readJSONFile decodes path into out; a missing file leaves out untouched.
func readJSONFile(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

// writeSecretFile writes v as JSON to path with mode 0600 inside a 0700 dir,
// through a same-directory temp file and a rename so a crash never leaves a
// half-written key file. The mode is set on the temp file BEFORE any byte of
// content lands in it, so no window exists where the keys are readable.
func writeSecretFile(path string, v any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("tighten %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".aq-secret-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// keyFingerprint is the spec's fingerprint: sha256 of the key, 16 hex chars.
func keyFingerprint(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "sha256:" + hex.EncodeToString(sum[:])[:16]
}

// fingerprintOf fingerprints the provider's first secret field (the key
// itself), in the server's declared order.
func fingerprintOf(p api.ConnectorProvider, entry accountEntry) string {
	for _, f := range p.Fields {
		if f.Secret && entry[f.Name] != "" {
			return keyFingerprint(entry[f.Name])
		}
	}
	for _, f := range p.Fields {
		if entry[f.Name] != "" {
			return keyFingerprint(entry[f.Name])
		}
	}
	return ""
}

// accountsCmd dispatches `aq accounts add|ls|rm`.
func accountsCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: aq accounts add <provider> [--label <name>] | ls | rm <provider>")
	}
	switch args[0] {
	case "add":
		return accountsAdd(args[1:])
	case "ls", "list":
		return accountsLs(os.Stdout)
	case "rm", "remove":
		return accountsRm(args[1:], os.Stdout)
	default:
		return fmt.Errorf("unknown accounts subcommand %q (want add, ls or rm)", args[0])
	}
}

// accountsAddDeps are the seams a test replaces: the server's provider list,
// the prompt, and the verification run.
type accountsAddDeps struct {
	providers func() ([]api.ConnectorProvider, error)
	prompt    func(label string, secret bool) (string, error)
	verify    func(provider, accountsPath string) (line string, err error)
	now       func() time.Time
	out       io.Writer
}

func accountsAdd(args []string) error {
	fs := flag.NewFlagSet("accounts add", flag.ContinueOnError)
	label := fs.String("label", "", "a name for this account, shown by `aq accounts ls` and `aq connect`")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("usage: aq accounts add <provider> [--label <name>]")
	}
	cred, err := config.Load()
	if err != nil {
		return err
	}
	if cred == nil || cred.Token == "" {
		return errors.New("not logged in: run `aq login` first (the provider list comes from your Aquanode account)")
	}
	client := newControlClient(cred)
	deps := accountsAddDeps{
		providers: client.ConnectorProviders,
		prompt:    promptFromTerminal,
		verify: func(provider, accountsPath string) (string, error) {
			bin, err := ensureMjolnirLocal(client, os.Stderr)
			if err != nil {
				return "", err
			}
			return runVerify(bin, provider, accountsPath, resolveAPIURL(cred))
		},
		now: time.Now,
		out: os.Stdout,
	}
	return runAccountsAdd(deps, positional[0], *label)
}

func runAccountsAdd(d accountsAddDeps, provider, label string) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "aws" {
		return errors.New("aws is not supported in local mode")
	}
	list, err := d.providers()
	if err != nil {
		return fmt.Errorf("could not fetch the provider list from Aquanode: %w", err)
	}
	var spec *api.ConnectorProvider
	names := make([]string, 0, len(list))
	for i := range list {
		names = append(names, list[i].Provider)
		if list[i].Provider == provider {
			spec = &list[i]
		}
	}
	if spec == nil {
		sort.Strings(names)
		return fmt.Errorf("%q is not a provider the local connector supports (supported: %s)", provider, strings.Join(names, ", "))
	}
	if len(spec.Fields) == 0 {
		return fmt.Errorf("the server listed no credential fields for %s", provider)
	}

	entry := accountEntry{}
	for _, f := range spec.Fields {
		name := f.Label
		if name == "" {
			name = f.Name
		}
		v, err := d.prompt(name, f.Secret)
		if err != nil {
			return err
		}
		v = strings.TrimSpace(v)
		if v == "" {
			return fmt.Errorf("%s is required", name)
		}
		entry[f.Name] = v
	}
	if label != "" {
		entry[accountLabelKey] = label
	}

	accPath, metaPath, err := accountsPaths()
	if err != nil {
		return err
	}
	accounts := accountsFile{}
	if err := readJSONFile(accPath, &accounts); err != nil {
		return err
	}
	meta := accountsMetaFile{}
	if err := readJSONFile(metaPath, &meta); err != nil {
		return err
	}
	prevEntry, hadPrev := accounts[provider]
	accounts[provider] = entry
	if err := writeSecretFile(accPath, accounts); err != nil {
		return fmt.Errorf("write %s: %w", accPath, err)
	}

	line, verr := d.verify(provider, accPath)
	if verr != nil {
		// A key the provider rejected must not stay stored: `aq connect` would
		// register it and the console would list an account that cannot deploy.
		if hadPrev {
			accounts[provider] = prevEntry
		} else {
			delete(accounts, provider)
		}
		if rerr := writeSecretFile(accPath, accounts); rerr != nil {
			return fmt.Errorf("verification failed (%v) and restoring %s failed too: %w", verr, accPath, rerr)
		}
		return fmt.Errorf("%s was not saved, verification failed: %w", provider, verr)
	}
	fmt.Fprintln(d.out, strings.TrimSpace(line))
	meta[provider] = accountMeta{Fingerprint: fingerprintOf(*spec, entry), VerifiedAt: d.now().UTC().Format(time.RFC3339)}
	if err := writeSecretFile(metaPath, meta); err != nil {
		return fmt.Errorf("write %s: %w", metaPath, err)
	}
	return nil
}

// runVerify runs `mjolnir local --verify <provider>` against the accounts file
// and returns its one output line. The key travels in the FILE, never argv.
func runVerify(bin, provider, accountsPath, apiURL string) (string, error) {
	cmd := exec.Command(bin, "local", "--accounts-file", accountsPath, "--api-url", apiURL, "--verify", provider)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stdout.String() + "\n" + stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(msg)
	}
	return stdout.String(), nil
}

func accountsLs(out io.Writer) error {
	accPath, metaPath, err := accountsPaths()
	if err != nil {
		return err
	}
	accounts := accountsFile{}
	if err := readJSONFile(accPath, &accounts); err != nil {
		return err
	}
	meta := accountsMetaFile{}
	if err := readJSONFile(metaPath, &meta); err != nil {
		return err
	}
	if len(accounts) == 0 {
		fmt.Fprintln(out, "No accounts stored. Add one with `aq accounts add <provider>`.")
		return nil
	}
	names := make([]string, 0, len(accounts))
	for n := range accounts {
		names = append(names, n)
	}
	sort.Strings(names)
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PROVIDER\tLABEL\tFINGERPRINT\tVERIFIED")
	for _, n := range names {
		m := meta[n]
		fp, ver := m.Fingerprint, m.VerifiedAt
		if fp == "" {
			fp = "-"
		}
		if ver == "" {
			ver = "never"
		}
		lab := accounts[n][accountLabelKey]
		if lab == "" {
			lab = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", n, lab, fp, ver)
	}
	return tw.Flush()
}

func accountsRm(args []string, out io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: aq accounts rm <provider>")
	}
	provider := strings.ToLower(args[0])
	accPath, metaPath, err := accountsPaths()
	if err != nil {
		return err
	}
	accounts := accountsFile{}
	if err := readJSONFile(accPath, &accounts); err != nil {
		return err
	}
	if _, ok := accounts[provider]; !ok {
		return fmt.Errorf("no stored account for %s", provider)
	}
	delete(accounts, provider)
	if err := writeSecretFile(accPath, accounts); err != nil {
		return err
	}
	meta := accountsMetaFile{}
	if err := readJSONFile(metaPath, &meta); err != nil {
		return err
	}
	delete(meta, provider)
	if err := writeSecretFile(metaPath, meta); err != nil {
		return err
	}
	fmt.Fprintf(out, "Removed %s. A running `aq connect` keeps its old session until restarted.\n", provider)
	return nil
}

// promptFromTerminal reads one value from stdin. A secret is read with the
// terminal's echo off, by shelling out to stty (aq takes no dependencies, see
// CLAUDE.md line 3); stdin that is not a terminal (a pipe, a file) is read as
// one plain line, which is what a script feeding a key expects.
func promptFromTerminal(label string, secret bool) (string, error) {
	fmt.Fprintf(os.Stderr, "%s: ", label)
	restore := func() {}
	if secret && isTerminal(os.Stdin) {
		off := exec.Command("stty", "-echo")
		off.Stdin = os.Stdin
		if err := off.Run(); err != nil {
			return "", fmt.Errorf("could not turn terminal echo off, refusing to read a key with it on: %w", err)
		}
		restore = func() {
			on := exec.Command("stty", "echo")
			on.Stdin = os.Stdin
			_ = on.Run()
			fmt.Fprintln(os.Stderr)
		}
	}
	defer restore()
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && (err != io.EOF || line == "") {
		return "", fmt.Errorf("read %s: %w", label, err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	if devnull, derr := os.Stat(os.DevNull); derr == nil && os.SameFile(info, devnull) {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
