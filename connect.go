package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// `aq connect` runs the local executor: mjolnir in `local` mode, a child of
// this process, holding the user's provider keys and doing every provider call
// the orchestrator relays to it. aq itself registers nothing and relays
// nothing; it installs the executor, hands it the stored credential through a
// 0600 file, streams what it logs, and ends it on Ctrl-C.

const (
	mjolnirBinName = "mjolnir"
	// The archive member is `mjolnir`; `mjolnir-local` is the release target's
	// name, accepted so a build that kept it still installs.
	mjolnirAltMember     = "mjolnir-local"
	connectorSessionFile = "connector-session.json"
	connectStopGrace     = 10 * time.Second
)

// mjolnirLocalDownloadError mirrors ogreDownloadURLError for the executor: a
// 404 is the platform refusal (exit 12, permanent), anything else is blamed on
// the party that failed.
func mjolnirLocalDownloadError(err error) error {
	var apiErr *api.APIError
	if errors.As(err, &apiErr) {
		if apiErr.Status == http.StatusNotFound {
			return &exitError{
				code: exitNoOgreBuildForPlatform,
				err: fmt.Errorf("no local executor build is published for %s/%s: %s\n"+
					"    `aq connect` runs it on THIS machine. Run it from a machine Aquanode builds it for",
					runtime.GOOS, runtime.GOARCH, apiErr.Message),
			}
		}
		return fmt.Errorf("Aquanode could not provide a local executor download: %s", apiErr.Message)
	}
	return fmt.Errorf("could not reach Aquanode to get the local executor download URL; check your network connection: %w", err)
}

// ensureMjolnirLocal installs or updates the executor at
// <bin dir>/mjolnir and returns its path. It never trusts a `mjolnir` found on
// PATH: that name is also the hosted provider server, which is not this binary.
//
// The archive is installed to a staging name, EXECUTED (`mjolnir version`), and
// only then renamed into place, so a tarball whose member does not run never
// replaces a working executor and is never left where connect would use it.
// A marker file beside the binary records the archive sha256 it came from; an
// unchanged sha256 (and a binary that still runs) means no download.
func ensureMjolnirLocal(client *api.Client, out io.Writer) (string, error) {
	dir, err := ogreBinDir()
	if err != nil {
		return "", err
	}
	dest := filepath.Join(dir, mjolnirBinName)
	marker := dest + ".sha256"

	meta, err := client.MjolnirLocalDownloadURL(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", mjolnirLocalDownloadError(err)
	}
	if have, rerr := os.ReadFile(marker); rerr == nil && strings.EqualFold(strings.TrimSpace(string(have)), meta.SHA256) {
		if verifyBinaryRuns(dest, mjolnirBinName) == nil {
			return dest, nil
		}
	}

	fmt.Fprintln(out, "Fetching the local executor from Aquanode...")
	staging := dest + ".new"
	defer os.Remove(staging)
	if err := downloadAndInstall(meta.URL, meta.SHA256, meta.Asset, staging, mjolnirBinName, mjolnirAltMember); err != nil {
		return "", fmt.Errorf("could not fetch the local executor: %w", err)
	}
	if err := verifyBinaryRuns(staging, mjolnirBinName); err != nil {
		return "", err
	}
	if err := os.Rename(staging, dest); err != nil {
		return "", fmt.Errorf("install the local executor to %s: %w", dest, err)
	}
	if err := os.WriteFile(marker, []byte(meta.SHA256+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("record the executor version: %w", err)
	}
	fmt.Fprintf(out, "Installed the local executor %s to %s\n", meta.Version, dest)
	return dest, nil
}

// connectSession is the file handed to the executor: where to register and the
// credential to register with, taken from the stored `aq login`.
type connectSession struct {
	APIURL string `json:"apiUrl"`
	APIKey string `json:"apiKey"`
	TeamID string `json:"teamId,omitempty"`
}

type connectOptions struct {
	client       *api.Client
	cred         *config.Credential
	apiURL       string
	accountsPath string
	out          io.Writer
}

func connectCmd(args []string) error {
	if len(args) > 0 && args[0] != "--help" && args[0] != "-h" {
		return fmt.Errorf("aq connect takes no arguments (got %q)", args[0])
	}
	if len(args) > 0 {
		fmt.Fprintln(os.Stdout, connectHelp)
		return nil
	}
	cred, err := config.Load()
	if err != nil {
		return err
	}
	if cred == nil || cred.Token == "" {
		return errors.New("not logged in: run `aq login` first")
	}
	accPath, _, err := accountsPaths()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runConnect(ctx, connectOptions{
		client:       newControlClient(cred),
		cred:         cred,
		apiURL:       resolveAPIURL(cred),
		accountsPath: accPath,
		out:          os.Stdout,
	})
}

const connectHelp = `aq connect: run the local executor so the general console can use your own
provider accounts. Keep it running in a terminal while you use the console or
"aq up". Add accounts first with "aq accounts add <provider>". Ctrl-C ends the
session. Exit 12: no executor build for this OS/architecture.`

func runConnect(ctx context.Context, o connectOptions) error {
	accounts := accountsFile{}
	if err := readJSONFile(o.accountsPath, &accounts); err != nil {
		return err
	}
	if len(accounts) == 0 {
		return errors.New("no provider accounts stored: run `aq accounts add <provider>` first")
	}

	bin, err := ensureMjolnirLocal(o.client, o.out)
	if err != nil {
		return err
	}

	// The session file carries the aq credential. Create it 0600 BEFORE any byte
	// is written (os.WriteFile would honour a umask-widened mode on create) and
	// remove it when the child is gone.
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	sessionPath := filepath.Join(dir, connectorSessionFile)
	if err := writeSecretFile(sessionPath, connectSession{APIURL: o.apiURL, APIKey: o.cred.Token, TeamID: o.cred.TeamID}); err != nil {
		return fmt.Errorf("write the session file: %w", err)
	}
	defer os.Remove(sessionPath)

	cmd := exec.CommandContext(ctx, bin, "local",
		"--accounts-file", o.accountsPath,
		"--session-file", sessionPath,
		"--api-url", o.apiURL)
	// Ctrl-C ends the session: interrupt the child so it can DELETE its session,
	// and kill it only if it has not exited after the grace period.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = connectStopGrace

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the local executor: %w", err)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	stream := func(r io.Reader) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			mu.Lock()
			fmt.Fprintln(o.out, sc.Text())
			mu.Unlock()
		}
	}
	wg.Add(2)
	go stream(stdout)
	go stream(stderr)
	wg.Wait()

	werr := cmd.Wait()
	if ctx.Err() != nil {
		fmt.Fprintln(o.out, "Session ended.")
		return nil
	}
	if werr != nil {
		return fmt.Errorf("the local executor stopped: %w", werr)
	}
	return nil
}
