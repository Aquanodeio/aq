package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// jobPullOptions configures doJobPull. jobPull() fills in the real
// environment; tests run doJobPull directly against a stub client/HTTP.
type jobPullOptions struct {
	cred   *config.Credential
	target string // job id or name
	runID  string // "" means resolve the latest run that reached a box
	dest   string // "" means the default ./<job>-<runId>/
	out    io.Writer
	errOut io.Writer
	// httpGet fetches a minted download URL's body. Tests inject a stub that
	// never touches the network; jobPull leaves it nil so doJobPull defaults
	// to a real http.Get.
	httpGet func(url string) (*http.Response, error)
}

// jobPullFlags is every flag `aq job pull` accepts, registered in one place
// so the top-level `aq --help` job: section can be checked against the real
// flag set (see job_help_test.go's TestTopLevelHelpDocumentsEveryJobFlag,
// the guard that exists because this exact kind of drift shipped before).
type jobPullFlags struct {
	run *string
}

func registerJobPullFlags(fs *flag.FlagSet) *jobPullFlags {
	f := &jobPullFlags{}
	f.run = fs.String("run", "", "run id to pull from (default: the latest run that reached a box)")
	return f
}

// jobPull parses `aq job pull <job> [--run <runId>] [dest]` and wires the
// real environment into doJobPull.
func jobPull(args []string) error {
	fs := flag.NewFlagSet("job pull", flag.ContinueOnError)
	f := registerJobPullFlags(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 || positional[0] == "" {
		return errors.New("usage: aq job pull <job> [--run <runId>] [dest]")
	}
	if len(positional) > 2 {
		return fmt.Errorf("aq job pull takes at most a job and a destination directory, got %s", strings.Join(positional, " "))
	}
	target := positional[0]
	dest := ""
	if len(positional) == 2 {
		dest = positional[1]
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return doJobPull(jobPullOptions{
		cred:   cred,
		target: target,
		runID:  strings.TrimSpace(*f.run),
		dest:   dest,
		out:    os.Stdout,
		errOut: os.Stderr,
	})
}

// doJobPull fetches a run's landed artifacts (log object + every declared
// output) into a local directory, sequentially, resumable by re-run.
func doJobPull(opts jobPullOptions) error {
	out, errOut := opts.out, opts.errOut
	if out == nil {
		out = os.Stdout
	}
	if errOut == nil {
		errOut = os.Stderr
	}
	httpGet := opts.httpGet
	if httpGet == nil {
		httpGet = http.Get
	}

	client := newControlClient(opts.cred)
	jobID, err := resolveJobID(client, opts.target)
	if err != nil {
		return err
	}

	runID := opts.runID
	if runID == "" {
		runID, err = latestRunWithABox(client, jobID)
		if err != nil {
			return err
		}
	}

	list, err := client.ListRunArtifacts(jobID, runID)
	if err != nil {
		return fmt.Errorf("could not list run %s's artifacts: %w", runID, err)
	}

	switch list.Source {
	case "no_attempt_yet":
		return fmt.Errorf("run %s has never had a box, so it has no artifacts to pull", runID)
	case "no_box":
		return fmt.Errorf("run %s's attempt has no box attached, so it has no artifacts to pull", runID)
	case "unreachable":
		// COULD NOT LOOK. This must never read as "no outputs" -- that is a
		// different, positive claim this call never established.
		return fmt.Errorf("could not reach the box to list run %s's artifacts right now; try again in a moment", runID)
	case "ok":
		// fall through
	default:
		return fmt.Errorf("run %s's artifacts came back with an unrecognized source %q", runID, list.Source)
	}

	if len(list.Artifacts) == 0 {
		fmt.Fprintf(out, "Run %s completed with no output files.\n", runID)
		return nil
	}

	dest := opts.dest
	if dest == "" {
		name := opts.target
		if job, err := findJob(client, jobID); err == nil && job.Name != "" {
			name = job.Name
		}
		dest = fmt.Sprintf("./%s-%s", name, runID)
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return fmt.Errorf("create destination %s: %w", dest, err)
	}

	var pulled, skipped, failed int
	for _, a := range list.Artifacts {
		destPath, err := safeArtifactPath(dest, a.Key)
		if err != nil {
			fmt.Fprintf(errOut, "  fail  %s: %v\n", a.Key, err)
			failed++
			continue
		}

		if info, statErr := os.Stat(destPath); statErr == nil && !info.IsDir() && info.Size() == a.SizeBytes {
			fmt.Fprintf(errOut, "  skip  %s (%s, already downloaded)\n", a.Key, formatBytes(a.SizeBytes))
			skipped++
			continue
		}

		dl, err := client.DownloadRunArtifactURL(jobID, runID, a.Key)
		if err != nil {
			fmt.Fprintf(errOut, "  fail  %s: could not mint a download URL: %v\n", a.Key, err)
			failed++
			continue
		}

		if err := downloadArtifactTo(httpGet, dl.URL, destPath); err != nil {
			fmt.Fprintf(errOut, "  fail  %s: %v\n", a.Key, err)
			failed++
			continue
		}
		fmt.Fprintf(out, "  %s  %s\n", a.Key, formatBytes(a.SizeBytes))
		pulled++
	}

	if list.Truncated {
		fmt.Fprintln(errOut, "  note: the server truncated this listing; re-run `aq job pull` to fetch what did not fit")
	}

	fmt.Fprintf(out, "✓ Pulled %d file(s) to %s/", pulled, dest)
	if skipped > 0 {
		fmt.Fprintf(out, " (%d already there)", skipped)
	}
	fmt.Fprintln(out)

	if failed > 0 {
		return fmt.Errorf("%d of %d file(s) failed to download; re-run `aq job pull` to retry them", failed, len(list.Artifacts))
	}
	return nil
}

// latestRunWithABox picks the default run for `aq job pull` when --run is
// not given: the most recent run whose attempt actually ran on a box
// (succeeded or failed), read off the existing runs listing -- never
// "queued"/"running" (not terminal yet, nothing has landed) and never
// "unservable" (never got a box at all, so there is structurally nothing to
// list; asking mjolnir about it would only ever answer no_box). ListRuns is
// already ordered newest-first (acceptedAt desc, orchestrator
// jobs.controller.ts:464), so the first match is the latest.
func latestRunWithABox(client *api.Client, jobID string) (string, error) {
	runs, err := client.ListRuns(jobID)
	if err != nil {
		return "", fmt.Errorf("could not list runs: %w", err)
	}
	for _, r := range runs {
		if r.Status == "succeeded" || r.Status == "failed" {
			return r.ID, nil
		}
	}
	return "", errors.New("no completed run yet to pull artifacts from; pass --run <runId> to pick one explicitly, or check `aq job runs`")
}

// safeArtifactPath joins dest with a server-supplied artifact key, refusing
// a key that would escape dest (a leading "../", an absolute path). The key
// is untrusted input from the wire; nothing here should ever write outside
// the directory the caller named.
func safeArtifactPath(dest, key string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(key))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) {
		return "", fmt.Errorf("refusing an unsafe artifact key %q", key)
	}
	return filepath.Join(dest, clean), nil
}

// downloadArtifactTo streams one artifact to destPath via a temp file in the
// same directory, renamed into place only once the whole body has landed --
// the same atomic-write shape import.go's ogre install uses (downloadAndInstall),
// so a killed or interrupted pull never leaves a file whose size the next
// run's resume check (doJobPull's os.Stat above) could mistake for a
// complete download.
func downloadArtifactTo(httpGet func(string) (*http.Response, error), url, destPath string) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(destPath), err)
	}

	resp, err := httpGet(url)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	tmp, err := os.CreateTemp(filepath.Dir(destPath), ".aq-pull-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once successfully renamed into place

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return fmt.Errorf("download failed: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		return fmt.Errorf("install %s: %w", destPath, err)
	}
	return nil
}
