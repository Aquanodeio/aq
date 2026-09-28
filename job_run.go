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
	"time"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// `aq job run` is the ONLY way to make a batch job now (the jobs-are-jobs
// contract): POST /jobs creates the job AND
// its one Run in one call, dispatched immediately, no separate "now start
// it" step. `aq job create` + `aq job run <job>` (create-then-call) is
// GONE, not renamed -- a job is 1:1 with its Run from here on, so there is
// nothing left for a second call to do.

// jobRunOptions configures runJobRun. jobRun() fills in the real
// environment; tests run runJobRun directly.
type jobRunOptions struct {
	cred *config.Credential
	// Exactly one source: (setupTarget, version) for a version-source run,
	// or image for an image-source one. jobRun refuses before this is ever
	// built if both or neither were given, so runJobRun branches on
	// image == "" and trusts the XOR already holds.
	setupTarget string // setup id (uuid) or name
	version     int    // the per-lineage version NUMBER to run
	image       string // --image ref; "" means version-source
	// registrySecret names a `type: registry` team secret to pull a private
	// --image with (`aq secret set --type registry`). Only meaningful with
	// image set; jobRun refuses it locally otherwise.
	registrySecret string
	// argv is the "command" entrypoint's argv, taken verbatim from everything
	// after a bare `--` (splitRemoteCommand): nil means "derive it from the
	// version's recipe" on a version-source run (job.service.ts
	// deriveDefaultEntrypoint), and is REQUIRED on an image-source run,
	// which has no recipe to derive from.
	argv []string
	// outputPath pairs with argv; ignored when argv is nil.
	outputPath string
	// gpuModels/anyGPU/gpuOrder/diskGB/gpuCount are the image-source job's
	// hardware constraint. Never applied to a version-source run: the
	// backend seeds that job's hardware from its recipe instead
	// (job.service.ts seedHardwareFromRecipe), and the CLI does not override
	// it -- jobRun refuses a non-default --gpus locally for that source (and
	// for a --on pinned job, which rents no hardware at all) before this is
	// ever built.
	gpuModels []string
	anyGPU    bool
	// gpuOrder is "" (cheapest, the default, never written to the wire),
	// "ordered", or "cheapest" (written as "" too, matching what an absent
	// key already means).
	gpuOrder string
	// gpuCount is hardware.gpuCount: one of 1, 2, 4, 8 (validated by jobRun
	// via validateJobGPUCount before this ever runs), default 1.
	gpuCount int
	diskGB   int
	name     string // job name (defaults to the source's own name)
	// onAlias is the `--on <alias>` value as typed, kept only for output,
	// pinnedDeploymentID is what actually goes on the wire.
	onAlias string
	// pinnedDeploymentID pins the job to a box the customer already
	// attached, instead of hardware Aquanode rents. Zero means the ordinary
	// managed path, never send it as a bare "0" or a negative number; the
	// wire key must be absent unless this is a real attached deployment id.
	pinnedDeploymentID int
	// secrets names `type: "env"` team secrets (`aq secret set --type env`)
	// this job's Run needs injected at dispatch. nil/empty means none;
	// CreateBatchJobRequest.Secrets carries `omitempty` for exactly that.
	secrets []string
	// checkpointPaths/checkpointExclude map to api.Checkpoint{Paths,Exclude}.
	// Applies to EITHER source, since the server's checkpointRequired check
	// (job.service.ts:578) runs unconditionally before any source branch.
	// Optional at this CLI, deliberately: neither is locally required, so
	// giving neither sends no checkpoint key at all and the server's own
	// refusal names what is missing.
	checkpointPaths   []string
	checkpointExclude []string
	// installRequirements opts argv into the shared requirements.txt wrapper
	// (training-jobs DX DELTA section 2.6): copy whatever landed in ogre's
	// fixed /inputs dir into /workspace, pip install -q -r requirements.txt,
	// then exec the user's own command. Only meaningful with argv given,
	// and refused locally otherwise.
	installRequirements bool
	// detach prints the created job's id and returns immediately, instead
	// of streaming its log to completion. Mirrors `docker run -d`.
	detach bool
	out    io.Writer
	errOut io.Writer
	// sleep/maxWaitPolls are test hooks for the poll loop that waits for the
	// run to reach a terminal status after its log stream ends; production
	// callers leave both zero (time.Sleep, unbounded).
	sleep        func(time.Duration)
	maxWaitPolls int
}

// jobRunFlags is every flag `aq job run` accepts, registered in one place so
// the top-level `aq --help` block can be checked against the real flag set
// rather than against a second copy someone remembered to update. The help
// block drifted before: it listed three of fourteen flags, so an
// image-source job was undocumented at the only place a user looks first.
type jobRunFlags struct {
	name                *string
	on                  *string
	image               *string
	registrySecret      *string
	gpuModels           *stringList
	anyGPU              *bool
	gpuOrder            *string
	gpus                *int
	diskGB              *int
	outputPath          *string
	secrets             *stringList
	checkpointPaths     *stringList
	checkpointExclude   *stringList
	installRequirements *bool
	detach              *bool
	// port exists ONLY to be refused. `aq job run` has no HTTP-serving
	// shape at all (a command entrypoint has no server to publish a port
	// for -- entrypoint.ts's own parser refuses `port` on a command kind by
	// name, see internal/api/jobs.go's Entrypoint doc), so registering the
	// flag and refusing it explicitly gives a typo'd `--port` a named fix
	// ("use `aq endpoint create`") instead of Go's generic "flag provided
	// but not defined" error, which points nowhere.
	port *int
}

func registerJobRunFlags(fs *flag.FlagSet) *jobRunFlags {
	f := &jobRunFlags{
		gpuModels:         &stringList{},
		secrets:           &stringList{},
		checkpointPaths:   &stringList{},
		checkpointExclude: &stringList{},
	}
	f.name = fs.String("name", "", "job name (default: the source's own name)")
	f.on = fs.String("on", "", "run this job on a host you already attached with `aq attach`, instead of renting hardware")
	f.image = fs.String("image", "", "a public or private image ref (source, instead of the <pod> <version> positionals)")
	f.registrySecret = fs.String("registry-secret", "", "name of a `type: registry` team secret (`aq secret set --type registry`) to pull a private --image with")
	fs.Var(f.gpuModels, "gpu-model", "exact marketplace GPU model name (see `aq gpus`) an --image job may run on (repeatable; required for --image unless --any-gpu)")
	f.anyGPU = fs.Bool("any-gpu", false, "explicit opt-in: let an --image job run on any GPU model the market currently offers, instead of naming one")
	f.gpuOrder = fs.String("gpu-order", "", "with two or more --gpu-model, prefer them in the order given (\"ordered\") or cheapest-first (\"cheapest\", the default)")
	f.gpus = fs.Int("gpus", defaultJobGPUCount, "how many GPUs the job's box should have: one of 1, 2, 4 or 8 (default: 1); only applies to an --image job")
	f.diskGB = fs.Int("disk-gb", 100, "disk size in GB for an --image job")
	f.outputPath = fs.String("output-path", "/outputs", "absolute path inside the box the command writes results into (used whenever a command is given after `--`)")
	fs.Var(f.secrets, "secret", "name of a `type: env` team secret (`aq secret set --type env`) to inject into this job's Run (repeatable)")
	fs.Var(f.checkpointPaths, "checkpoint-path", "path ogre snapshots so a reclaimed or price-hopped run can resume (repeatable); optional, but the server refuses a job with none named")
	fs.Var(f.checkpointExclude, "checkpoint-exclude", "path excluded from the checkpoint snapshot, e.g. a venv or cache dir (repeatable)")
	f.installRequirements = fs.Bool("install-requirements", false, "wrap the command (after --) to install a declared requirements.txt before running it: copies /inputs into /workspace, pip installs -q -r requirements.txt, then execs the command (shared wire contract with the console's same toggle)")
	f.detach = fs.Bool("detach", false, "print the created job's id and exit immediately, instead of streaming its log until it finishes")
	f.port = fs.Int("port", 0, "refused: a job with a port is an endpoint, use `aq endpoint create --port` instead")
	return f
}

// defaultJobGPUCount is what --gpus defaults to and the only value a
// version-source or --on pinned run may carry: neither has a wire path for
// a Job's hardware.gpuCount (see jobRunOptions.gpuCount), so anything else
// on those paths is refused locally by name rather than silently ignored.
const defaultJobGPUCount = 1

// jobGPUCounts is the closed set a Job's hardware.gpuCount accepts on the
// wire (orchestrator hardware.ts: z.union of the four literals). This is
// deliberately NOT aq up/deploy's free-form --gpus (any 1..maxGPUCount):
// job-placement.ts matches gpuCount EXACTLY against a node's own GPU count,
// never `>=` (the fix for #1157's pricing bug, which is why single-GPU was
// forced in the first place), so a value outside this closed set could never
// place on a real node and the CLI refuses it before the request is ever
// sent.
var jobGPUCounts = [...]int{1, 2, 4, 8}

// validateJobGPUCount rejects a --gpus value the API would 400 anyway,
// naming the allowed set the same way the orchestrator's own schema error
// does.
func validateJobGPUCount(n int) error {
	for _, v := range jobGPUCounts {
		if n == v {
			return nil
		}
	}
	return fmt.Errorf("--gpus must be 1, 2, 4 or 8, got %d", n)
}

// jobRun parses `aq job run <setup> <version> ... -- <argv...>` (a
// version-source run) or `aq job run --image <ref> ... -- <argv...>`
// (an image-source one) and wires the real environment into runJobRun.
func jobRun(args []string) error {
	// Everything after a bare `--` is the entrypoint's argv, verbatim. No
	// quoting gymnastics, and never re-parsed as our own flags (which a plain
	// loop over fs.Parse would do, breaking on a user argv token that itself
	// looks like a flag, e.g. `-- python train.py --epochs 3`).
	head, argv := splitRemoteCommand(args)

	fs := flag.NewFlagSet("job run", flag.ContinueOnError)
	f := registerJobRunFlags(fs)
	name, on, image, registrySecret := f.name, f.on, f.image, f.registrySecret
	anyGPU, gpuOrder, diskGB, outputPath := f.anyGPU, f.gpuOrder, f.diskGB, f.outputPath

	positional, err := parseInterspersed(fs, head)
	if err != nil {
		return err
	}
	// Named refusal, checked before anything else: a job with a port is an
	// endpoint, and the fix is a different command, not a different flag.
	if *f.port != 0 {
		return fmt.Errorf("aq job run has no --port flag: a job that serves HTTP is an endpoint, create it with `aq endpoint create --image <ref> --port %d ...`", *f.port)
	}
	// Repeatable flags are read only after Parse has filled them in.
	gpuModels, secrets := *f.gpuModels, *f.secrets
	checkpointPaths, checkpointExclude := *f.checkpointPaths, *f.checkpointExclude
	if err := validateJobGPUCount(*f.gpus); err != nil {
		return err
	}

	imageRef := strings.TrimSpace(*image)
	registrySecretName := strings.TrimSpace(*registrySecret)
	hasPositionalSource := len(positional) > 0

	// SOURCE XOR, refused locally by name before any request is built:
	// mirrors job.service.ts's own refusal (createJob:618-632), just earlier.
	if imageRef != "" && hasPositionalSource {
		return errors.New("aq job run takes exactly one source: --image, or <pod> <version> positionals, never both")
	}
	if registrySecretName != "" && imageRef == "" {
		return errors.New("--registry-secret only applies to an --image job")
	}
	if imageRef == "" && !hasPositionalSource {
		return errors.New("usage: aq job run <pod> <version> [...] -- <argv...>  OR  aq job run --image <ref> --gpu-model <name> [...] -- <argv...>")
	}

	onAlias := strings.TrimSpace(*on)
	var pinnedDeploymentID int
	if onAlias != "" {
		if imageRef != "" {
			// Mirrors job.service.ts's own PinnedBoxError: a pinned box
			// already carries a saved version, so it has nothing an
			// image-source job could match against.
			return errors.New("--on pins a box that already carries a saved version, so it cannot serve an --image job")
		}
		h, err := lookupHost(onAlias)
		if err != nil {
			return err
		}
		if !h.Attached() {
			return fmt.Errorf("host %q is not attached: run `aq attach %s` first, then retry `aq job run ... --on %s`", onAlias, onAlias, onAlias)
		}
		if h.DeploymentID <= 0 {
			// Cannot happen given h.Attached() above (it requires a nonzero
			// DeploymentID), but the wire must never see a non-positive pin
			// under any circumstance, so this is asserted explicitly rather
			// than trusted.
			return fmt.Errorf("host %q has no valid attached deployment id: run `aq attach %s` again", onAlias, onAlias)
		}
		pinnedDeploymentID = h.DeploymentID
	}

	// --gpus only has a wire path on an --image job (runJobRun's image
	// branch writes hardware.gpuCount from it). A version-source run never
	// overrides recipe hardware -- job.service.ts's seedHardwareFromRecipe
	// runs instead -- and a --on pinned job rents no hardware at all, so
	// neither has anything for the flag to apply to. A default (unset)
	// --gpus is a silent no-op on both, since it already matches what
	// happens without the flag; only a non-default value is refused, named
	// by the reason specific to each.
	if imageRef == "" && *f.gpus != defaultJobGPUCount {
		if onAlias != "" {
			return fmt.Errorf("--gpus cannot be combined with --on: a pinned job runs on the host you already attached with `aq attach`, so no hardware is rented for --gpus to apply to (got --gpus %d)", *f.gpus)
		}
		return fmt.Errorf("--gpus only applies to an --image job: a version-source job's hardware is seeded from its recipe, not overridden by the CLI (got --gpus %d)", *f.gpus)
	}

	var setupTarget string
	var version int
	if imageRef == "" {
		if len(positional) < 2 || positional[0] == "" || positional[1] == "" {
			return errors.New("usage: aq job run <pod> <version> [--name <n>] [--on <alias>] -- <argv...>")
		}
		setupTarget = positional[0]
		version, err = strconv.Atoi(positional[1])
		if err != nil || version <= 0 {
			return fmt.Errorf("invalid version %q; pass the version number shown by `aq save` or `aq pods` (e.g. 3 for v3)", positional[1])
		}
	} else {
		// Entrypoint is REQUIRED for an image job, never invented: the
		// backend refuses a missing one by name (job.service.ts:680-684,
		// "there is no recipe to derive one from") because a null entrypoint
		// fails later at dispatch, on a box the owner is already paying for.
		if len(argv) == 0 {
			return errors.New("an image-source job must state its entrypoint: pass the command to run after `--`, e.g. `aq job run --image ... -- python train.py`")
		}
		if len(gpuModels) == 0 && !*anyGPU {
			return errors.New("--gpu-model is required for an image-source job (see `aq gpus` for exact names), or pass --any-gpu to allow any model the market currently offers")
		}
		if len(gpuModels) > 0 && *anyGPU {
			return errors.New("--gpu-model and --any-gpu are mutually exclusive: name the models you want, or allow any of them, not both")
		}
		if *gpuOrder != "" && *gpuOrder != "ordered" && *gpuOrder != "cheapest" {
			return fmt.Errorf("--gpu-order must be \"ordered\" or \"cheapest\", got %q", *gpuOrder)
		}
		if *diskGB < 10 || *diskGB > 10_000 {
			return fmt.Errorf("--disk-gb must be between 10 and 10000, got %d", *diskGB)
		}
	}

	if *f.installRequirements && len(argv) == 0 {
		return errors.New("--install-requirements has no command to wrap: pass one after `--`, e.g. `aq job run ... --install-requirements -- python train.py`")
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runJobRun(jobRunOptions{
		cred:                cred,
		setupTarget:         setupTarget,
		version:             version,
		image:               imageRef,
		registrySecret:      registrySecretName,
		argv:                argv,
		outputPath:          *outputPath,
		gpuModels:           []string(gpuModels),
		anyGPU:              *anyGPU,
		gpuOrder:            *gpuOrder,
		gpuCount:            *f.gpus,
		diskGB:              *diskGB,
		name:                *name,
		onAlias:             onAlias,
		pinnedDeploymentID:  pinnedDeploymentID,
		secrets:             []string(secrets),
		checkpointPaths:     []string(checkpointPaths),
		checkpointExclude:   []string(checkpointExclude),
		installRequirements: *f.installRequirements,
		detach:              *f.detach,
		out:                 os.Stdout,
		errOut:              os.Stderr,
	})
}

// imageDerivedName mirrors console/app/jobs/new/page.tsx's own `derivedName`
// for an image source: the last path segment of the ref, tag stripped,
// "new-job" when that yields nothing usable.
func imageDerivedName(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "new-job"
	}
	last := ref
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		last = ref[i+1:]
	}
	if i := strings.Index(last, ":"); i >= 0 {
		last = last[:i]
	}
	if last == "" {
		return "new-job"
	}
	return last
}

// runJobRun resolves the source (a (setup, version-number) pair, or an
// image ref), posts POST /jobs (which creates the job AND dispatches its one
// Run in the same call), and then either prints the created id (--detach)
// or streams the Run's log to completion, exiting non-zero if it does not
// succeed.
func runJobRun(opts jobRunOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}
	errOut := opts.errOut
	if errOut == nil {
		errOut = os.Stderr
	}

	// Never send a zero or negative pin on the wire under any circumstance,
	// the key must be absent unless it is a real attached deployment id.
	// CreateBatchJobRequest.PinnedDeploymentID carries `omitempty` for
	// exactly the zero case; this guards the negative case, which
	// omitempty does not.
	if opts.pinnedDeploymentID < 0 {
		return fmt.Errorf("internal error: refusing to send a negative pinnedDeploymentId (%d)", opts.pinnedDeploymentID)
	}

	client := newControlClient(opts.cred)

	req := api.CreateBatchJobRequest{
		PinnedDeploymentID: opts.pinnedDeploymentID,
		Secrets:            opts.secrets,
	}

	var name string
	if opts.image != "" {
		name = opts.name
		if name == "" {
			name = imageDerivedName(opts.image)
		}
		req.Image = &api.ImageSource{Ref: opts.image, RegistrySecret: opts.registrySecret}

		gpuModels := opts.gpuModels
		if opts.anyGPU {
			// The EXPLICIT opt-in to the console's "no card picked means any
			// card" default (page.tsx:510-516): fetch the model universe and
			// send all of it, rather than leaving gpuModels empty, which
			// placement refuses outright (no_gpu_models, job.service.ts:467).
			avail, err := client.HardwareAvailability(opts.diskGB)
			if err != nil {
				return fmt.Errorf("could not fetch the marketplace model universe for --any-gpu: %w", err)
			}
			gpuModels = make([]string, 0, len(avail.Models))
			for _, m := range avail.Models {
				gpuModels = append(gpuModels, m.GPUModel)
			}
			if len(gpuModels) == 0 {
				return errors.New("--any-gpu found no GPU model on the market right now; try again, or pass --gpu-model explicitly")
			}
		}
		req.Hardware = &api.Hardware{
			GPUModels: gpuModels,
			GPUCount:  opts.gpuCount,
			DiskGB:    opts.diskGB,
		}
		// Placement is always sent for an image job, exactly as the console
		// does (buildParams() never gates the key itself); an empty object
		// and an absent key mean the same thing to PlacementSchema, but this
		// keeps the wire shape byte-identical to what the console sends.
		placement := &api.JobPlacement{}
		if opts.gpuOrder == "ordered" && len(gpuModels) > 1 {
			placement.GPUOrder = "ordered"
		}
		req.Placement = placement
	} else {
		setupID, err := resolveSetupID(client, opts.setupTarget)
		if err != nil {
			return err
		}
		versionRowID, err := resolveSetupVersionRowID(client, setupID, opts.version)
		if err != nil {
			return err
		}
		name = opts.name
		if name == "" {
			name = setupDisplayName(client, setupID)
		}
		req.VersionID = versionRowID
	}
	req.Name = name

	// argv applies to EITHER source: supplying an entrypoint skips the
	// backend's recipe-derivation entirely (job.service.ts:678), which is the
	// only way a non-ComfyUI template (no derivable entrypoint at all) can
	// run a Job from the CLI.
	if len(opts.argv) > 0 {
		argv := opts.argv
		if opts.installRequirements {
			argv = wrapWithRequirementsInstall(argv)
		}
		req.Entrypoint = &api.Entrypoint{
			Kind:       "command",
			Argv:       argv,
			OutputPath: opts.outputPath,
		}
	}

	// Applies to EITHER source: checkpointRequired runs unconditionally
	// before any source branch. Neither flag is locally required, so giving
	// neither leaves the key off the wire and the server's own refusal fires.
	if len(opts.checkpointPaths) > 0 || len(opts.checkpointExclude) > 0 {
		req.Checkpoint = &api.Checkpoint{
			Paths:   opts.checkpointPaths,
			Exclude: opts.checkpointExclude,
		}
	}
	job, err := client.CreateBatchJob(req)
	if err != nil {
		// A pin refused server-side (a bad --on bind) already names its own
		// fix, relay it verbatim rather than burying it inside a generic
		// "could not create job" wrapper.
		if opts.pinnedDeploymentID != 0 {
			var apiErr *api.APIError
			if errors.As(err, &apiErr) && apiErr.Status == http.StatusBadRequest {
				return errors.New(apiErr.Message)
			}
		}
		return fmt.Errorf("could not create job %q: %w", name, err)
	}

	switch {
	case opts.pinnedDeploymentID != 0:
		fmt.Fprintf(out, "✓ Created job %q → run %s (pinned to %s, bills nothing)\n", job.Name, job.Run.ID, opts.onAlias)
	case opts.image != "":
		fmt.Fprintf(out, "✓ Created job %q → image %s, run %s %s\n", job.Name, opts.image, job.Run.ID, jobStatusWord(job.Run.Status))
	default:
		fmt.Fprintf(out, "✓ Created job %q → v%d, run %s %s\n", job.Name, opts.version, job.Run.ID, jobStatusWord(job.Run.Status))
	}

	if opts.detach {
		return nil
	}
	return streamJobRunToCompletion(opts.cred, job.ID, job.Run.ID, out, errOut, opts.sleep, opts.maxWaitPolls)
}

// jobRunTerminalStatuses are the api.Run.Status values a Run never leaves:
// once here, nothing further will happen to it.
func isTerminalRunStatus(status string) bool {
	switch status {
	case "succeeded", "failed", "unservable", "cancelled":
		return true
	}
	return false
}

// streamJobRunToCompletion tails a freshly-created run's log (sharing
// runJobLogsFollow with `aq job logs -f` so the two can never drift) and
// then confirms the run's FINAL status: the log stream can end (source
// archived/box_gone, or the user's own Ctrl-C) slightly before the run row
// itself is marked terminal, and this must report the real outcome, not
// "the tail stopped". Exits non-zero (a plain error, `run()` in main.go maps
// that to exit 1) for failed/unservable/cancelled.
func streamJobRunToCompletion(cred *config.Credential, jobID, runID string, out, errOut io.Writer, sleep func(time.Duration), maxWaitPolls int) error {
	if err := runJobLogsFollow(jobLogsOptions{
		cred:   cred,
		jobRef: jobID,
		runID:  runID,
		follow: true,
		out:    out,
		errOut: errOut,
		sleep:  sleep,
	}); err != nil {
		return fmt.Errorf("could not stream run %s's log: %w", runID, err)
	}

	client := newControlClient(cred)
	slp := sleep
	if slp == nil {
		slp = time.Sleep
	}
	run, err := waitForRunTerminal(client, jobID, runID, slp, maxWaitPolls)
	if err != nil {
		return fmt.Errorf("could not check run %s's final status: %w", runID, err)
	}

	switch run.Status {
	case "succeeded":
		fmt.Fprintf(out, "✓ Run %s %s\n", run.ID, jobStatusWord(run.Status))
		return nil
	case "failed", "unservable", "cancelled":
		// "failed" is the owner's own code or runtime; "unservable" never
		// ran the owner's workload at all -- that must never be left for the
		// reader to infer from a bare status word, so only unservable
		// carries the disclaimer. Billing is unaffected by this addendum.
		msg := fmt.Sprintf("run %s: %s", run.ID, jobStatusWord(run.Status))
		if run.Status == "unservable" {
			msg += ". " + unservableDisclaimer
		}
		if run.Reason != "" {
			msg += fmt.Sprintf(" (%s)", run.Reason)
		}
		return errors.New(msg)
	default:
		// The log stream ended (e.g. the box was reclaimed mid-run before a
		// terminal status landed) but the run itself has not finished; say
		// so rather than pretending this was a clean completion.
		fmt.Fprintf(out, "Run %s is still %s; check `aq job logs %s` or `aq job ls`\n", run.ID, jobStatusWord(run.Status), jobID)
		return nil
	}
}

// waitForRunTerminal polls GET /jobs/:id/runs/:runId until the run reaches a
// terminal status, or maxPolls is exhausted (0 = unlimited, the production
// default -- a foreground `aq job run` is meant to wait).
func waitForRunTerminal(client *api.Client, jobID, runID string, sleep func(time.Duration), maxPolls int) (*api.Run, error) {
	for polls := 0; ; polls++ {
		run, err := client.GetRun(jobID, runID)
		if err != nil {
			return nil, err
		}
		if isTerminalRunStatus(run.Status) {
			return run, nil
		}
		if maxPolls > 0 && polls+1 >= maxPolls {
			return run, nil
		}
		sleep(2 * time.Second)
	}
}

// requirementsInstallScript is the FIXED LITERAL both aq and console must
// emit identically, with nothing ever substituted into it. An earlier form
// interpolated the command into this string (`... && exec <RAW_COMMAND>`,
// built here via `strings.Join(argv, " ")`), which is unsafe for a builder
// holding TOKENS rather than the user's original typed string: a token
// containing a space, `;`, `|` or `$(...)` reassembles into something
// `bash -lc` re-splits or executes, silently producing a WRONG run rather
// than an error. `exec "$@"` has no interpolation and therefore no quoting
// hazard at all -- see wrapWithRequirementsInstall for how $0/$@ get filled.
const requirementsInstallScript = `cp -r /inputs/. /workspace/ && pip install -q -r requirements.txt && exec "$@"`

// wrapWithRequirementsInstall composes the shared wire contract both aq and
// console build for the "install requirements.txt before running" opt-in
// (training-jobs DX DELTA section 2.6, revised 2026-09-20): copy whatever
// ogre already staged in its fixed /inputs dir into the checkpointed
// /workspace, pip install a declared requirements.txt, then exec the user's
// own command via `"$@"`. This is the literal both builders must emit
// verbatim -- the placement test asserts the WIRE argv, never this helper.
//
// argv is passed through UNTOUCHED, one element each, after two fixed
// entries: the script string, then a literal "bash" that fills $0 (`bash
// -lc` assigns its first operand to $0, not $1 -- omitting it would silently
// eat the user's first argument). aq already holds argv as separate tokens
// from its own `-- <argv>` parsing (never shell-parsed), so unlike console's
// client-side tokenizer there is nothing left to tokenize here.
func wrapWithRequirementsInstall(argv []string) []string {
	wrapped := make([]string, 0, 4+len(argv))
	wrapped = append(wrapped, "bash", "-lc", requirementsInstallScript, "bash")
	return append(wrapped, argv...)
}
