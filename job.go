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

// job dispatches `aq job <sub>` — the whole Jobs vocabulary.
//
// A GROUP rather than top-level verbs: `aq run` and `aq logs` already mean
// "push this directory to a box and run something on it" and "tail a box's
// logs". Those are daily commands, and `aq run mybox` / `aq run myjob` are the
// same string, so there is no argument shape that could disambiguate them.
func job(args []string) error {
	if len(args) == 0 {
		// Every subcommand, not three of eight. This line listed only
		// create/point/rm while the unknown-subcommand error below listed all
		// eight, so `aq job` with no args hid run, runs, logs, cancel and pull
		// from the exact user who was asking what the verbs are.
		return errors.New("usage: aq job <create|point|rm|run|runs|logs|cancel|pull> ...")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "create":
		return jobCreate(rest)
	case "point":
		return jobPoint(rest)
	case "rm":
		return jobRemove(rest)
	case "run":
		return jobRun(rest)
	case "runs":
		return jobRuns(rest)
	case "logs":
		return jobLogs(rest)
	case "cancel":
		return jobCancel(rest)
	case "pull":
		return jobPull(rest)
	default:
		return fmt.Errorf("aq job: unknown subcommand %q, expected one of create, point, rm, run, runs, logs, cancel, pull", sub)
	}
}

// jobCreateOptions configures runJobCreate. jobCreate() fills
// in the real environment; tests run runJobCreate directly.
type jobCreateOptions struct {
	cred *config.Credential
	// Exactly one source: (setupTarget, version) for a version-source create,
	// or image for an image-source one. jobCreate refuses before this is ever
	// built if both or neither were given, so runJobCreate branches on
	// image == "" and trusts the XOR already holds.
	setupTarget string // setup id (uuid) or name
	version     int    // the per-lineage version NUMBER to make callable
	image       string // --image ref; "" means version-source
	// registrySecret names a `type: registry` team secret to pull a private
	// --image with (`aq secret set --type registry`). Only meaningful with
	// image set; jobCreate refuses it locally otherwise.
	registrySecret string
	// argv is the "command" entrypoint's argv, taken verbatim from everything
	// after a bare `--` (splitRemoteCommand): nil means "derive it from the
	// version's recipe" on a version-source create (job.service.ts
	// deriveDefaultEntrypoint), and is REQUIRED on an image-source create,
	// which has no recipe to derive from. A non-nil argv applies to EITHER
	// source and skips derivation entirely, which is the only way a template
	// with no derivable entrypoint at all (e.g. Torch+Jupyter, which the
	// backend cannot derive a command from) can create a Job from the CLI.
	argv []string
	// outputPath pairs with argv; ignored when argv is nil.
	outputPath string
	// gpuModels/anyGPU/gpuOrder/diskGB are the image-source Job's hardware
	// constraint. Never applied to a version-source create: the backend seeds
	// that job's hardware from its recipe instead (job.service.ts
	// seedHardwareFromRecipe), and the CLI does not override it.
	gpuModels []string
	anyGPU    bool
	// gpuOrder is "" (cheapest, the default, never written to the wire),
	// "ordered", or "cheapest" (written as "" too, matching what an absent
	// key already means).
	gpuOrder     string
	diskGB       int
	name         string // job name (defaults to the source's own name)
	maxInstances int
	// The MONTHLY budget, in cents, and optional. Not the old per-job
	// `spendCapCents`, which the backend deleted: a dollar ceiling could not be
	// translated into calls, because the same amount bought 13 cold ones or 200
	// warm ones. The bound that is ALWAYS present is wall-clock and comes from
	// the job's own time limit, attempts and machine count. -1 means "not set".
	monthlyCapCents int64
	// onAlias is the `--on <alias>` value as typed, kept only for output,
	// pinnedDeploymentID is what actually goes on the wire.
	onAlias string
	// pinnedDeploymentID pins the job to a box the customer already
	// attached, instead of hardware Aquanode rents. Zero means the ordinary
	// managed path, never send it as a bare "0" or a negative number; the
	// wire key must be absent unless this is a real attached deployment id.
	pinnedDeploymentID int
	// secrets names `type: "env"` team secrets (`aq secret set --type env`)
	// this job's Runs need injected at dispatch. nil/empty
	// means none; CreateJobRequest.Secrets carries `omitempty` for exactly
	// that, the same convention every optional field on the request follows.
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
	// (training-jobs DX DELTA section 2.6, the same literal contract the
	// console's "Install requirements.txt before running" toggle composes):
	// copy whatever landed in ogre's fixed /inputs dir into /workspace, pip
	// install -q -r requirements.txt, then exec the user's own command. Only
	// meaningful with argv given (there is nothing else to wrap), and refused
	// locally otherwise.
	installRequirements bool
	out                 io.Writer
}

// jobCreateFlags is every flag `aq job create` accepts, registered in one
// place so the top-level `aq --help` block can be checked against the real
// flag set rather than against a second copy someone remembered to update.
// The help block drifted before: it listed three of fourteen flags, so an
// image-source job was undocumented at the only place a user looks first.
type jobCreateFlags struct {
	name                *string
	maxInstances        *int
	monthlyCapCents     *int64
	on                  *string
	image               *string
	registrySecret      *string
	gpuModels           *stringList
	anyGPU              *bool
	gpuOrder            *string
	diskGB              *int
	outputPath          *string
	secrets             *stringList
	checkpointPaths     *stringList
	checkpointExclude   *stringList
	installRequirements *bool
	// port exists ONLY to be refused. `aq job create` has no HTTP-serving
	// shape at all (a command entrypoint has no server to publish a port
	// for — entrypoint.ts's own parser refuses `port` on a command kind by
	// name, see internal/api/jobs.go's Entrypoint doc), so registering the
	// flag and refusing it explicitly gives a typo'd `--port` a named fix
	// ("use `aq endpoint create`") instead of Go's generic "flag provided
	// but not defined" error, which points nowhere.
	port *int
}

func registerJobCreateFlags(fs *flag.FlagSet) *jobCreateFlags {
	f := &jobCreateFlags{
		gpuModels:         &stringList{},
		secrets:           &stringList{},
		checkpointPaths:   &stringList{},
		checkpointExclude: &stringList{},
	}
	f.name = fs.String("name", "", "job name (default: the source's own name)")
	f.maxInstances = fs.Int("max-instances", 0, "maximum concurrent instances this job may run (required)")
	f.monthlyCapCents = fs.Int64("monthly-cap-cents", -1, "optional monthly budget in cents; new runs stop once the month's spend reaches it")
	f.on = fs.String("on", "", "run this job on a host you already attached with `aq attach`, instead of renting hardware")
	f.image = fs.String("image", "", "a public or private image ref (source, instead of the <pod> <version> positionals)")
	f.registrySecret = fs.String("registry-secret", "", "name of a `type: registry` team secret (`aq secret set --type registry`) to pull a private --image with")
	fs.Var(f.gpuModels, "gpu-model", "exact marketplace GPU model name (see `aq gpus`) an --image job may run on (repeatable; required for --image unless --any-gpu)")
	f.anyGPU = fs.Bool("any-gpu", false, "explicit opt-in: let an --image job run on any GPU model the market currently offers, instead of naming one")
	f.gpuOrder = fs.String("gpu-order", "", "with two or more --gpu-model, prefer them in the order given (\"ordered\") or cheapest-first (\"cheapest\", the default)")
	f.diskGB = fs.Int("disk-gb", 100, "disk size in GB for an --image job")
	f.outputPath = fs.String("output-path", "/outputs", "absolute path inside the box the command writes results into (used whenever a command is given after `--`)")
	fs.Var(f.secrets, "secret", "name of a `type: env` team secret (`aq secret set --type env`) to inject into this job's Runs (repeatable)")
	fs.Var(f.checkpointPaths, "checkpoint-path", "path ogre snapshots so a reclaimed or price-hopped run can resume (repeatable); optional, but the server refuses a job with none named")
	fs.Var(f.checkpointExclude, "checkpoint-exclude", "path excluded from the checkpoint snapshot, e.g. a venv or cache dir (repeatable)")
	f.installRequirements = fs.Bool("install-requirements", false, "wrap the command (after --) to install a declared requirements.txt before running it: copies /inputs into /workspace, pip installs -q -r requirements.txt, then execs the command (shared wire contract with the console's same toggle)")
	f.port = fs.Int("port", 0, "refused: a job with a port is an endpoint, use `aq endpoint create --port` instead")
	return f
}

// jobCreate parses `aq job create <setup> <version>` (a version-source
// create) or `aq job create --image <ref> ...` (an image-source one) and
// wires the real environment into runJobCreate.
//
// --max-instances is required: a job hands out a GPU budget and never defaults
// to unbounded.
//
// --spend-cap-cents is GONE, not renamed. The backend deleted the per-job
// dollar cap on purpose — it could not be translated into runs, since the same
// amount bought 13 cold ones or 200 warm ones, and a limit you cannot express
// in your own units is not a control. Keeping the flag as an accepted no-op
// would be worse than removing it: it would keep telling people they had a hard
// stop they no longer have.
//
// What replaces it is two things. The always-present bound is wall-clock and
// needs no flag — the job's time limit times its attempts times its machines is
// the worst case for one run. On top of that, --monthly-cap-cents is an
// OPTIONAL budget over billed time for the calendar month.
func jobCreate(args []string) error {
	// Everything after a bare `--` is the entrypoint's argv, verbatim. No
	// quoting gymnastics, and never re-parsed as our own flags (which a plain
	// loop over fs.Parse would do, breaking on a user argv token that itself
	// looks like a flag, e.g. `-- python train.py --epochs 3`).
	head, argv := splitRemoteCommand(args)

	fs := flag.NewFlagSet("job create", flag.ContinueOnError)
	f := registerJobCreateFlags(fs)
	name, maxInstances, monthlyCapCents := f.name, f.maxInstances, f.monthlyCapCents
	on, image, registrySecret := f.on, f.image, f.registrySecret
	anyGPU, gpuOrder, diskGB, outputPath := f.anyGPU, f.gpuOrder, f.diskGB, f.outputPath

	positional, err := parseInterspersed(fs, head)
	if err != nil {
		return err
	}
	// Named refusal, checked before anything else: a job with a port is an
	// endpoint, and the fix is a different command, not a different flag.
	if *f.port != 0 {
		return fmt.Errorf("aq job create has no --port flag: a job that serves HTTP is an endpoint, create it with `aq endpoint create --image <ref> --port %d ...`", *f.port)
	}
	// Repeatable flags are read only after Parse has filled them in.
	gpuModels, secrets := *f.gpuModels, *f.secrets
	checkpointPaths, checkpointExclude := *f.checkpointPaths, *f.checkpointExclude
	if *maxInstances <= 0 {
		return errors.New("--max-instances is required and must be a positive number: a job hands out a GPU budget, so it never defaults to unbounded")
	}

	imageRef := strings.TrimSpace(*image)
	registrySecretName := strings.TrimSpace(*registrySecret)
	hasPositionalSource := len(positional) > 0

	// SOURCE XOR, refused locally by name before any request is built:
	// mirrors job.service.ts's own refusal (createJob:618-632), just earlier.
	if imageRef != "" && hasPositionalSource {
		return errors.New("aq job create takes exactly one source: --image, or <pod> <version> positionals, never both")
	}
	if registrySecretName != "" && imageRef == "" {
		return errors.New("--registry-secret only applies to an --image job")
	}
	if imageRef == "" && !hasPositionalSource {
		return errors.New("usage: aq job create <pod> <version> --max-instances <n> [...]  OR  aq job create --image <ref> --gpu-model <name> --max-instances <n> -- <argv...>")
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
			return fmt.Errorf("host %q is not attached: run `aq attach %s` first, then retry `aq job create ... --on %s`", onAlias, onAlias, onAlias)
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

	var setupTarget string
	var version int
	if imageRef == "" {
		if len(positional) < 2 || positional[0] == "" || positional[1] == "" {
			return errors.New("usage: aq job create <pod> <version> --max-instances <n> [--monthly-cap-cents <n>] [--on <alias>]")
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
			return errors.New("an image-source job must state its entrypoint: pass the command to run after `--`, e.g. `aq job create --image ... -- python train.py`")
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
		return errors.New("--install-requirements has no command to wrap: pass one after `--`, e.g. `aq job create ... --install-requirements -- python train.py`")
	}

	// No required-cap check any more. The wall-clock bound is structural and
	// always applies; the monthly budget is genuinely optional, and -1 means the
	// key is left OFF THE WIRE entirely rather than sent as a 0 that would read
	// as "budget of nothing".

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runJobCreate(jobCreateOptions{
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
		diskGB:              *diskGB,
		name:                *name,
		maxInstances:        *maxInstances,
		monthlyCapCents:     *monthlyCapCents,
		onAlias:             onAlias,
		pinnedDeploymentID:  pinnedDeploymentID,
		secrets:             []string(secrets),
		checkpointPaths:     []string(checkpointPaths),
		checkpointExclude:   []string(checkpointExclude),
		installRequirements: *f.installRequirements,
		out:                 os.Stdout,
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

// runJobCreate resolves the source (a (setup, version-number) pair, or an
// image ref) and makes it callable.
func runJobCreate(opts jobCreateOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	// Never send a zero or negative pin on the wire under any circumstance,
	// the key must be absent unless it is a real attached deployment id.
	// CreateJobRequest.PinnedDeploymentID carries `omitempty` for the
	// zero case; this guards the negative case, which omitempty does not.
	if opts.pinnedDeploymentID < 0 {
		return fmt.Errorf("internal error: refusing to send a negative pinnedDeploymentId (%d)", opts.pinnedDeploymentID)
	}

	client := newControlClient(opts.cred)

	req := api.CreateJobRequest{
		MaxInstances:       opts.maxInstances,
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
			GPUCount:  1,
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
	// create a Job from the CLI.
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

	// OMITTED unless set. The backend's schema is optional, and optional means
	// the key is ABSENT -- sending 0 would read as "a budget of nothing", which
	// would refuse every run.
	if opts.monthlyCapCents >= 0 {
		req.MonthlySpendCapCents = &opts.monthlyCapCents
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
	ep, err := client.CreateJob(req)
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
		fmt.Fprintf(out, "✓ Created job %q → v%d (max %d instance(s), pinned to %s, bills nothing)\n",
			ep.Name, opts.version, opts.maxInstances, opts.onAlias)
	case opts.image != "":
		if opts.monthlyCapCents >= 0 {
			fmt.Fprintf(out, "✓ Created job %q → image %s (max %d instance(s), monthly budget %s)\n",
				ep.Name, opts.image, opts.maxInstances, formatCents(opts.monthlyCapCents))
		} else {
			fmt.Fprintf(out, "✓ Created job %q → image %s (max %d instance(s), bounded by its run time limit)\n",
				ep.Name, opts.image, opts.maxInstances)
		}
	default:
		if opts.monthlyCapCents >= 0 {
			fmt.Fprintf(out, "✓ Created job %q → v%d (max %d instance(s), monthly budget %s)\n",
				ep.Name, opts.version, opts.maxInstances, formatCents(opts.monthlyCapCents))
		} else {
			fmt.Fprintf(out, "✓ Created job %q → v%d (max %d instance(s), bounded by its run time limit)\n",
				ep.Name, opts.version, opts.maxInstances)
		}
	}
	return nil
}

// jobPointOptions configures runJobPoint. jobPoint() fills in
// the real environment; tests run runJobPoint directly.
type jobPointOptions struct {
	cred    *config.Credential
	target  string // job id or name
	version int    // the per-lineage version NUMBER to repoint to
	out     io.Writer
}

// jobPoint parses `aq job point <name> <version>` and wires the
// real environment into runJobPoint.
func jobPoint(args []string) error {
	fs := flag.NewFlagSet("job point", flag.ContinueOnError)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) < 2 || positional[0] == "" || positional[1] == "" {
		return errors.New("usage: aq job point <name> <version>")
	}
	target := positional[0]
	version, err := strconv.Atoi(positional[1])
	if err != nil || version <= 0 {
		return fmt.Errorf("invalid version %q; pass the version number shown by `aq save` or `aq pods` (e.g. 3 for v3)", positional[1])
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runJobPoint(jobPointOptions{cred: cred, target: target, version: version, out: os.Stdout})
}

// runJobPoint repoints a job at a different version NUMBER within
// the same save lineage its current version already belongs to — the same
// command rolls forward or back, it just depends which number is passed.
//
// The repoint API and the number a user types are both scoped to a version
// NUMBER within one lineage (the same per-lineage counter `aq share` and
// `aq job create` use), but an Job only carries its current
// VersionID, not the owning setup/lineage name. So this first resolves
// VersionID → (setup id, lineage name) via GetSetupVersion, then resolves
// the typed number against THAT lineage via ListSetupVersions — the same
// two-step resolveSetupVersionRowID already does starting from a setup id
// directly, just starting from the job's live version instead.
func runJobPoint(opts jobPointOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	client := newControlClient(opts.cred)
	jobID, err := resolveJobID(client, opts.target)
	if err != nil {
		return err
	}
	ep, err := findJob(client, jobID)
	if err != nil {
		return err
	}

	current, err := client.GetSetupVersion(ep.VersionID)
	if err != nil {
		return fmt.Errorf("could not resolve job %q's current version: %w", ep.Name, err)
	}

	versions, err := client.ListSetupVersions(current.Name)
	if err != nil {
		return fmt.Errorf("could not look up versions named %q: %w", current.Name, err)
	}
	var targetRowID int
	found := false
	for _, v := range versions {
		if v.SetupID == current.SetupID && v.Version == opts.version {
			targetRowID = v.ID
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("no version %d found in %q's save lineage %q", opts.version, ep.Name, current.Name)
	}

	updated, err := client.RepointJob(jobID, api.RepointJobRequest{VersionID: targetRowID})
	if err != nil {
		return fmt.Errorf("could not repoint job %q: %w", ep.Name, err)
	}

	fmt.Fprintf(out, "✓ Repointed job %q → v%d\n", updated.Name, opts.version)
	return nil
}

// jobRemoveOptions configures runJobRemove. jobRemove() fills
// in the real environment; tests run runJobRemove directly.
type jobRemoveOptions struct {
	cred   *config.Credential
	target string // job id or name
	out    io.Writer
}

// jobRemove parses `aq job rm <name>` and wires the real
// environment into runJobRemove.
func jobRemove(args []string) error {
	fs := flag.NewFlagSet("job rm", flag.ContinueOnError)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 || positional[0] == "" {
		return errors.New("usage: aq job rm <name>")
	}
	target := positional[0]

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runJobRemove(jobRemoveOptions{cred: cred, target: target, out: os.Stdout})
}

// runJobRemove deletes a job.
func runJobRemove(opts jobRemoveOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	client := newControlClient(opts.cred)
	jobID, err := resolveJobID(client, opts.target)
	if err != nil {
		return err
	}
	ep, err := findJob(client, jobID)
	if err != nil {
		return err
	}

	if err := client.DeleteJob(jobID); err != nil {
		return fmt.Errorf("could not remove job %q: %w", ep.Name, err)
	}

	fmt.Fprintf(out, "✓ Removed job %q\n", ep.Name)
	return nil
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

// formatCents renders a cent amount as a dollar figure, e.g. 150 -> "$1.50".
func formatCents(cents int64) string {
	neg := ""
	if cents < 0 {
		neg = "-"
		cents = -cents
	}
	return fmt.Sprintf("%s$%d.%02d", neg, cents/100, cents%100)
}
