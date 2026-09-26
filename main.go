// Command aq is the Aquanode control / funnel CLI.
//
// It runs on a developer's laptop and talks to the Aquanode API to rent GPUs,
// provision the ogre on-box agent, and restore snapshots. It is a thin
// orchestration wrapper over `ogre` (the OSS on-box agent) + the Aquanode API —
// it does not reimplement ogre.
//
// Subcommands are built out by the funnel tickets. `login` (device-grant
// pairing) ships here; deploy / up follow. See research/action/02-console-dx.md
// in the meta-repo.
package main

import (
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"strings"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// version is overridden at build time via -ldflags "-X main.version=...", which is
// how release binaries get their number. `go install github.com/Aquanodeio/aq@latest`
// sets no ldflags, so those builds fall back to the version the Go toolchain stamps
// into the build info from the module tag.
var version = "0.0.0-dev"

// resolveVersion returns the ldflags-injected version when there is one, else the
// module version recorded in the build info. Returns the dev sentinel for a plain
// `go build` from a checkout, where neither source has a real version.
func resolveVersion() string {
	if version != "0.0.0-dev" {
		return version
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok || bi.Main.Version == "" {
		return version
	}
	// A `go build` from a checkout reports "(devel)"; only a module-proxy install
	// carries a real tag.
	if bi.Main.Version == "(devel)" {
		return version
	}
	return strings.TrimPrefix(bi.Main.Version, "v")
}

func main() {
	// Label every API request `aq/<version>` so the orchestrator can tell a CLI
	// action from a scripted one. Set here rather than in the api package so the
	// ldflags-injected version stays a main-package concern.
	version = resolveVersion()
	api.Version = version

	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	cmd, args := os.Args[1], os.Args[2:]

	// Resolve the host this run will talk to ONCE, here, and apply the two
	// target rails before anything dispatches — see prodguard.go for why they
	// live at the dispatch rather than inside each command. Doing it here also
	// keeps the rails out of the run<Verb> functions the tests drive directly,
	// so no existing test's captured output changes.
	//
	// A credential that fails to load is not an error at this point: the
	// commands that need one report that themselves, with a message about
	// logging in. Here it only narrows resolveAPIURL to the env var and the
	// built-in default, which is the same host the failing command would have
	// used anyway.
	cred, _ := config.Load()
	apiURL := resolveAPIURL(cred)

	if _, billable := billableCommands[cmd]; billable {
		var prodFlag bool
		args, prodFlag = stripProdFlag(args)
		allowProd := prodFlag || os.Getenv("AQ_ALLOW_PROD") == "1"
		overseen := hasHumanOversight(os.Getenv, isInteractiveStdin())
		if err := guardBillable(cmd, apiURL, args, allowProd, overseen); err != nil {
			run(err)
		}
	}
	announceTarget(cmd, apiURL, os.Stderr)

	switch cmd {
	case "version", "--version", "-v":
		fmt.Printf("aq %s\n", version)
	case "help", "--help", "-h":
		usage()
	case "gpus":
		run(gpus(args))
	case "login":
		run(login(args))
	case "up":
		run(up(args))
	case "deploy":
		run(deploy(args))
	case "import":
		run(importCmd(args))
	case "host":
		run(hostCmd(args))
	case "attach":
		run(attachCmd(args))
	case "release":
		run(releaseCmd(args))
	case "ssh":
		run(sshCmd(args))
	case "push":
		run(push(args))
	case "run":
		run(runCmd(args))
	case "logs":
		run(logsCmd(args))
	case "ls":
		run(lsCmd(args))
	case "status":
		run(status(args))
	case "save":
		run(snapshot(args))
	case "sync-now":
		run(syncNow(args))
	case "start":
		run(start(args))
	case "stop":
		run(stop(args))
	case "move":
		run(move(args))
	case "autostop":
		run(autostop(args))
	case "env":
		run(env(args))
	case "volume":
		run(volume(args))
	case "pods":
		run(pods(args))
	case "idle":
		run(idle(args))
	// `job` is a command GROUP, not three top-level verbs, and that is a
	// deliberate departure from the plan in aq#63. It asked for `aq run <job>`,
	// `aq runs <job>` and `aq logs <run-id>` — but `aq run` already means "push
	// this directory to a box and run a command on it with my terminal
	// attached", and `aq logs` already tails a box. Taking either verb would
	// break a command people use daily, to save one word on a command they use
	// occasionally. `aq run` also could not be disambiguated by argument shape:
	// `aq run mybox` and `aq run myjob` are the same string.
	case "job":
		run(job(args))
	case "endpoint":
		run(endpoint(args))
	case "secret":
		run(secret(args))
	case "down":
		run(down(args))
	case "logout":
		run(logout(args))
	case "whoami":
		run(whoami(args))
	default:
		fmt.Fprintf(os.Stderr, "aq: unknown command %q\n\n", cmd)
		usage()
		os.Exit(1)
	}
}

// Exit codes. 1 is the catch-all for "the command failed"; a code above it is
// a SPECIFIC, documented refusal a script can branch on. Give a refusal its own
// code rather than folding it into 1: a caller that cannot distinguish "no
// build exists for your machine, and never will" from "the network blipped"
// retries forever.
//
//	1   command failed
//	12  no ogre build is published for this platform (`aq import`)
const (
	exitFailure                = 1
	exitNoOgreBuildForPlatform = 12
)

// exitError carries a specific exit code out of a command. Anything that does
// not wrap one exits exitFailure.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

// run reports a command error to stderr and exits non-zero, with the error's
// own documented code when it carries one.
func run(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "aq: %v\n", err)
		var exitErr *exitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.code)
		}
		os.Exit(exitFailure)
	}
}

// usageText is the top-level help. Held as a named constant rather than
// inlined into usage() so a test can read the exact bytes a user sees:
// this block states flag defaults in prose, and one of them (--ogre-port)
// silently disagreed with the constant that actually supplies it, telling
// users to pick the one port that collides with ogre's own terminal proxy.
const usageText = `aq: Aquanode control CLI

Usage:
  aq <command> [flags]

Commands:
  gpus          Browse live GPU offers across every provider (no account needed)
  login         Pair this CLI to your Aquanode account (device login)
  up            Rent the cheapest matching GPU and bring up a working pod
  deploy        Restore a save onto a freshly-rented Aquanode GPU box
  import        Capture a box you rent elsewhere into a new Aquanode volume
  host          Register a box you own or lease, and drive it with no account
  attach        Adopt a registered box into your Aquanode control plane
  release       Hand an attached box back. The box keeps running
  ssh           Open a shell on a pod (managed key + ~/.ssh/config alias)
  push          Send your working directory to a box you already rented
  run           Push the working directory, then run a command on the box
  logs          Read a detached run's output
  ls            List your deployments: what is running and what it costs
  status        Show a pod's status, HTTPS URL, and credentials
  save          Detached only: capture a BYO-bucket box into its own remote
  sync-now      Detached only: force a BYO-bucket box's sync tick right now
  start         Start a Stopped pod on the cheapest matching GPU
  stop          Save a pod's environment and volume, then release its machine
  move          Stop a pod, then start it again on a different GPU
  autostop      Turn a pod's stop-when-idle preference on or off
  env           Manage a pod's Environment: keep it, share it, list, delete
  volume        Manage a pod's Volume: list, duplicate, restore a point, delete
  pods          List the pods you own
  idle          View or change a DEPLOYMENT's idle-auto-pause thresholds
  job           Create, run, inspect and cancel GPU jobs
  endpoint      Create, list and inspect callable HTTP endpoints
  secret        Manage team secrets: env vars and registry credentials a job
                can reference by name, never sent to the CLI as plaintext
  down          Tear down a deployment outright, nothing saved
  logout        Remove the stored CLI credential
  whoami        Show the current login state
  version       Print the aq version
  help          Show this help

gpus:
  aq gpus                     Browse every live GPU offer across all providers,
                              cheapest first. Works with no account: nothing
                              is read from or written to ~/.config/aq.

  --gpu <model>       Filter to a GPU model (substring, case-insensitive,
                      e.g. "B200")
  --max-price <n>     Only show offers at or below this hourly price
                      (the WHOLE offer's price, not per-GPU)
  --provider <name>   Restrict to a single provider (e.g. runpod)
  --region <name>     Filter to a region (substring, case-insensitive)
  --limit <n>         Max rows to print (default 20, 0 = all). With --json,
                      every match is returned unless you pass this explicitly
  --json              Print the filtered offers as JSON instead of a table

up flags:
  --name <name>      Set the deployment's display name (default: an
                     auto-generated one)
  --gpu <model>      Filter to a GPU model (substring, e.g. "RTX 4090")
  --gpus <n>         How many GPUs the box should have (default 1, max 8).
                     Only offers with at least this many are considered.
  --max-price <n>    Only rent GPUs at or below this hourly price
                     (the WHOLE offer's price, not per-GPU)
  --provider <name>  Restrict to a single provider (e.g. massecompute)
  --show-secrets     Echo the service password to stdout (hidden by default)
  --auto-pause       Enable idle auto-pause on this deployment (off by default)
  --warn-after <duration>  With --auto-pause: warn after this much idle time
  --pause-after <duration> With --auto-pause: auto-pause after this much idle time

  App (optional, you get a bare GPU box if you pick neither):
  --comfyui          Also install ComfyUI
  --jupyter          Also install Torch + Jupyter instead

deploy flags:
  --name <name>      Set the deployment's display name (default: an
                     auto-generated one)
  --snapshot <id>    Save to deploy (id from aq / the console, e.g. ext-42)
  --gpu <model>      Filter to a GPU model (substring, e.g. "RTX 4090")
  --gpus <n>         How many GPUs the box should have (default 1, max 8).
                     Only offers with at least this many are considered.
  --max-price <n>    Only rent GPUs at or below this hourly price
                     (the WHOLE offer's price, not per-GPU)
  --provider <name>  Restrict to a single provider (e.g. massecompute)
  --show-secrets     Echo the service password to stdout (hidden by default)

  App (optional, relaunches ComfyUI by default; --no-app restores data only):
  --comfyui          Relaunch ComfyUI on the restored data
  --jupyter          Relaunch Torch + Jupyter on the restored data instead
  --no-app           Restore only, do not relaunch an app

import:
  Run ON a box you already rent somewhere else (RunPod, Vast, your own
  hardware). Captures its /workspace-equivalent data into a new Aquanode
  Volume. Survey-first: aq shows exactly what it will and won't capture, and
  asks before anything is uploaded. This never rents anything itself — attach
  the resulting volume to a pod (any built-in environment) from the console
  to bring it online.

  aq import                 Survey, confirm, capture, and register the volume
  aq import --dry-run       Survey and print the plan; capture/upload nothing
  aq import --include <path>  Add a path to capture (repeatable)
  aq import --exclude <path>  Drop a detected path from capture (repeatable)
  aq import --name <name>   Name the resulting volume (default: from hostname)
  aq import --yes           Skip the interactive confirmation
  aq import --resume <volume-id>
                             Resume an import that started but didn't finish
                             (e.g. the upload credentials expired mid-capture).
                             Re-mints write credentials and re-runs the
                             capture into the exact same storage location:
                             restic dedups what already landed, so this never
                             restarts from zero and never bills a second,
                             parallel volume for the same box.

host / attach / release (boxes we never provisioned):
  Two modes for a machine you already own or lease, sharing one artifact format.

  DETACHED: your box, no control plane, no Aquanode account required. aq drives
  ogre on the box over your own ssh session, and ogre's CLI reaches its daemon on
  loopback, so the box needs no inbound connectivity from us at all. Nothing in
  detached mode contacts the Aquanode API.

  aq host add <alias> --ssh ubuntu@1.2.3.4
                             Survey the box, verify ogre's daemon answers on
                             loopback, and register it locally. Survey-first:
                             you see what aq found before anything changes.
                             --ssh root@1.2.3.4 works too; if the box refuses
                             root SSH (common on stock cloud images), name its
                             real user there or with --ssh-user.
  aq host add … --dry-run    Survey and print the plan; write nothing, anywhere
  aq host ls                 List registered boxes
  aq host rm <alias>         Forget a box. The box itself is untouched.

  --identity <path>    Private key to authenticate with (default: aq's own)
  --mount-path <dir>   Workspace root on the box (default: /workspace)
  --ogre-port <n>      Port ogre listens on once attached (default: 8444)
  --ogre-binary <path> Upload this Linux x86_64 ogre when the box has none.
                       There is no public ogre installer, so aq will not
                       download one: it installs the binary you name, or
                       refuses.

  Then address the box as "host:<alias>" from any box-facing verb:
    aq ssh host:lease-a              aq push host:lease-a
    aq run host:lease-a -- nvidia-smi    aq logs host:lease-a
    aq status host:lease-a           (ogre status, read from the box)
    aq save host:lease-a             (ogre snapshot, into your own bucket)
    aq sync-now host:lease-a         (ogre push, to your configured remote)
    aq up host:lease-a               (bring services up in place; rents nothing)

  ATTACHED: your box, our control plane. The box becomes a deployment we never
  provisioned and gains the console, environment/volume history, sharing, teams,
  metrics and jobs.

  aq attach <alias>          Adopt a registered box (needs a login)
  aq attach <alias> --dry-run
                             Print the plan; write nothing on the box and
                             create nothing in Aquanode
  aq attach <alias> --yes    Skip the confirmation
  --host <addr>              Address our orchestrator should dial
                             (default: the box's ssh host)

  Attach reaches the box one way only: a public address, the port open inbound
  from our infrastructure, TLS pinned. It probes before it commits, and a box it
  cannot reach is NOT attached: the failure is reported with the probe's own
  reason and the box stays fully usable in detached mode.

  Attach requires ogre's listen port and the port we dial to be THE SAME port:
  there is no separate dial port. On a port-mapped box (most container-pool
  marketplace listings: simplepod, vast.ai and similar, where sshd and the
  workload get remapped external ports and 8443 inbound does not reach the same
  8443 the box listens on) that equality can never hold, so attach cannot work
  there no matter which port you pass. This is a direct-connectivity-only
  design choice, not a bug: it is scoped to boxes with a real public IP and an
  inbound path to it (bare metal, most VM-pool providers). Detached mode has no
  such requirement: it needs no inbound connectivity at all.

  Everything aq writes on your box goes inside "# BEGIN aquanode" markers, and
  aq refuses to write to any file it could not first read. Your existing
  authorized_keys is never replaced.

  One attached box is ONE deployment running ONE pod at a time. Aquanode
  cannot partition a multi-GPU box into several independent pods: the whole
  box attaches as a single target. That does not exist in either mode.

  aq release <alias>         Hand an attached box back: Aquanode revokes its
                             credentials and drops its deployment row. The box
                             KEEPS RUNNING and no provider is ever contacted:
                             this is not a terminate. (--keep-host keeps the
                             box in your registry for detached use.)

  Detached does: capture, restore, pods, run/logs/ssh/sync, ogre up
  templates, BYO bucket.
  Attached adds: teams and RBAC, environment sharing ("aq env share"), the
  console, jobs and aq job run, cross-provider burst, the marketplace.
  Neither does: splitting one box across several independent pods.

ssh:
  aq ssh                     Open a shell on your only live deployment
  aq ssh <name|id>           Open a shell on a deployment by --name or id
  aq ssh <name> -- <cmd…>    Run a command on the box instead of opening a shell

  --print            Print the ssh command that would run, and exit
  -L <spec>          Forward a local port, e.g. 8888:localhost:8888 (repeatable)
  --user <name>      Override the login user (default: root)

  aq manages ~/.ssh/aquanode.config (included from your ~/.ssh/config) with one
  "aq-<name>" alias per live box, so ssh, scp, rsync, and VSCode Remote-SSH all
  work with that alias and no aq involved. If you have no SSH key at all, aq
  generates a passphrase-less one at ~/.ssh/aquanode_ed25519.

push / run:
  The local-code loop: edit on your laptop, execute on the GPU. Both send a
  directory tree over the same managed "aq-<name>" alias ssh uses, nothing
  new to authenticate, and scp/rsync against that alias keep working too.

  aq push [name|id]          Send the current directory to /workspace
  aq run [name|id] -- <cmd>  Send it, then run <cmd> in it with the terminal
                              attached (Ctrl-C reaches the remote process)

  --from <dir>       Local directory to send (default: the current directory)
  --to <dir>         Destination on the box (default: /workspace, absolute)
  --exclude <pat>    Skip paths matching this pattern (repeatable)
  --no-default-excludes
                     Send .git, node_modules, __pycache__ and friends too;
                     by default they are skipped
  --include-secrets  Also send .env, SSH keys, credentials.json and other
                     credential-shaped paths; by default they are skipped and
                     the skip is reported on stderr
  --delete           Make the remote tree mirror the local one, deleting what
                     you deleted. Needs rsync on the box.
  --print            Print the command that would run, and exit

  run also takes:
  --dir <dir>        Directory to run in (default: the push destination)
  --no-push          Run without sending anything first
  --detach           Start it and return. The run keeps going after you
                     disconnect; read it back with "aq logs". Prints the run
                     id on stdout so you can capture it.
  --then-pause <duration>
                     Valid only with --detach. After the run launches, arm
                     idle auto-pause on its deployment for this act-after
                     window (e.g. 1h): it pauses once this command has
                     finished AND the GPU has stayed idle that long, not the
                     instant the process exits. Off unless you ask for it —
                     each run that wants it opts in for itself.

  A .aqignore file in the directory you send adds exclude patterns, one per
  line, "#" for comments.

  Transport: rsync when both ends have it (only changed files move), otherwise
  tar over ssh, which re-sends the whole tree. aq prints which one it used.

ls / logs:
  aq ls                      Live deployments: id, name, status, GPU, provider,
                              hourly rate and age
  aq ls --all                Include closed and failed ones

  The rate column always names its currency (e.g. "0.4200 USD"). It is the
  provider's own denomination, which is not always dollars.

  aq logs [name|id]          Print the most recent detached run's output
  aq logs --run <id>         Read one specific run
  aq logs --list             List this box's runs, their state, and command
  -f                         Keep streaming as the run writes more
  -n <lines>                 Trailing lines to show (default 200)
  --dir <dir>                Working directory the run was launched in
                              (default: /workspace)

idle:
  A PER-DEPLOYMENT idle-auto-pause policy (warn/pause thresholds, GPU idle %).
  It always outranks a pod's own "aq autostop" preference above, see
  "autostop" for how the two differ.

  aq idle status <name|id>   Show the deployment's idle-auto-pause policy and
                              its current live verdict (ACTIVE / IDLE / UNKNOWN)
  aq idle set <name|id>      Update the policy (only the flags you pass change)

  --warn-after <duration>   Warn after this much idle time, e.g. 30m, 1h
  --pause-after <duration>  Auto-pause after this much idle time, e.g. 1h
  --gpu-threshold <percent> GPU utilization below which the box counts idle
  --on / --off              Enable / disable idle auto-pause

job:
  Everything about jobs lives under "aq job", not at the top level. "aq run"
  already means "push this directory to a box and run something on it" and
  "aq logs" already tails a box; those are daily commands, and "aq run mybox"
  and "aq run myjob" are the same string, so nothing could tell them apart.

  aq job create <pod> <version>
  aq job create --image <ref> -- <argv...>
                              Make something runnable as a job: either a pod
                              version you saved, or a container image you
                              already have. An image-source job states its
                              entrypoint after a bare "--".

  --name <name>             Job name (default: the source's own name)
  --max-instances <n>       Maximum concurrent instances this job may run.
                            Required: a job hands out a GPU budget, so it
                            never defaults to unbounded
  --monthly-cap-cents <n>   Monthly budget in cents; new runs stop once the
                            month's spend reaches it
  --on <alias>              Pin it to a box you already attached (aq attach
                            <alias>) instead of renting hardware; that box
                            bills nothing
  --secret <name>           Inject an "aq secret set --type env" secret into
                            the job's Runs (repeatable)
  --checkpoint-path <path>  Path ogre snapshots so a reclaimed or price-hopped
                            run can resume (repeatable). Optional here, but the
                            server refuses a job that names none
  --checkpoint-exclude <path>
                            Path left out of the checkpoint snapshot, e.g. a
                            venv or a cache dir (repeatable)
  --output-path <path>      Absolute path inside the box the command writes its
                            results into (default: /outputs). Used whenever a
                            command is given after "--"
  --install-requirements    Wrap the command (after "--") to install a
                            declared requirements.txt before running it: copy
                            /inputs into /workspace, "pip install -q -r
                            requirements.txt", then exec the command. Same
                            wire contract the console's own toggle composes
  --port <n>                Refused: a job that serves HTTP is an endpoint,
                            create it with "aq endpoint create --port" instead

  Image-source jobs only:
  --image <ref>             A public or private image ref, used instead of the
                            <pod> <version> positionals
  --registry-secret <name>  Name of an "aq secret set --type registry" secret
                            to pull a private --image with
  --gpu-model <name>        Exact marketplace GPU model name (see "aq gpus")
                            the job may run on (repeatable; required unless
                            --any-gpu)
  --any-gpu                 Explicit opt-in: run on any GPU model the market
                            currently offers, instead of naming one
  --gpu-order <mode>        With two or more --gpu-model, prefer them in the
                            order given ("ordered") or cheapest-first
                            ("cheapest", the default)
  --gpus <n>                How many GPUs the job's box should have: one of
                            1, 2, 4 or 8 (default: 1)
  --disk-gb <n>             Disk size in GB for an --image job (default: 100)

  aq job point <name> <version>
                              Repoint a job at a different version in its
                              lineage (also how you roll back).
  aq job rm <name>            Remove a job.
  aq job run <job> [--input file]
                              Start a run and print its run id. --input is a
                              JSON file of the declared params.
                              --wait  Block until the run finishes, up to
                              --wait-seconds (default 30, capped at 120).
                              --follow, -f  Stream the run's log until it ends.
  aq job runs <job>           List a job's recent runs: id, status, phase and
                              reason. "unservable" means Aquanode could not
                              get the run a machine at all — it does NOT mean
                              your own code failed.
  aq job logs <job> <run-id> [-f] [--attempt N]
                              Print a run's log. -f keeps printing as it is
                              written. A run that moved to another machine has
                              several attempts; --attempt picks one, and the
                              default is the latest rather than all of them
                              concatenated, which would put the timestamps out
                              of order in the middle.
  aq job cancel <job> <run-id>
                              Stop a run. Billing stops when the machine is
                              released.
  aq job pull <job> [--run <runId>] [dest]
                              Download a finished run's landed artifacts (its
                              log object and every declared output) into dest
                              (default: "./<job>-<runId>/"), one file per
                              artifact key. Re-running skips a file already
                              downloaded at the same size, so an interrupted
                              pull picks up where it left off.

  --run <runId>              Pull this run instead of the latest one that
                            actually reached a box (default)

endpoint:
  The service-shaped half of the Jobs vocabulary: an image with a port,
  callable over HTTP, instead of a batch command run. A SEPARATE top-level
  group from "aq job" — "aq job create" has no way to publish a port at all,
  it refuses --port by name.

  aq endpoint create --image <ref> --port <p> [--path /] --gpu-model <name>
                              Make an image callable over HTTP. Entrypoint on
                              the wire is always
                              {kind:"http", port, path, method:"POST", resultMode:"inline"}.

  --name <name>              Endpoint name (default: derived from the image ref)
  --image <ref>              A public or private image ref (required)
  --port <n>                 Port inside the box the image's own server
                            listens on (required)
  --path <path>              Path this endpoint's caller POSTs to (default: /)
  --gpu-model <name>         Exact marketplace GPU model name (see "aq gpus")
                            this endpoint may run on (repeatable; required)
  --disk-gb <n>              Disk size in GB (default: 100, matching the
                            console's Endpoints form)
  --max-instances <n>        Maximum concurrent instances this endpoint may
                            run (required)
  --keep-warm                Keep one instance running between calls instead
                            of scaling to zero (sends minInstances: 1)

  aq endpoint list            List your endpoints: id, name, status, and how
                              many instances are running out of the max.
  aq endpoint url <name|id>   Print the endpoint's callable URL verbatim
                              (POST to it with an "x-job-token", see
                              "aq job run" for how a token holder calls it).

secret:
  Team secrets: env vars and private-registry credentials a job can reference
  by NAME. A value is written once and never read back, by aq or anyone else:
  "aq secret list" shows names and metadata only.

  aq secret set <name> --type env --value <value>
                              Store an env secret. --value can be omitted to
                              read the value from stdin instead (it never
                              lands in shell history or a process list that
                              way). <name> must be a valid env var identifier,
                              since it IS the env var key a job's runs see.
  aq secret set <name> --type registry --server <host> --username <user> --token <token>
                              Store a private-registry credential (PAT or
                              password). <name> here is just a label.
  aq secret list              List secret names, types and rotation dates.
                              Never prints a value.
  aq secret rotate <name|id> --value <value>
                              Replace an env secret's stored value.
  aq secret rotate <name|id> --server <host> --username <user> --token <token>
                              Replace a registry secret's stored credential.
  aq secret rm <name|id>      Delete a secret. A job still referencing it
                              starts failing that reference at its next run.

status / save / sync-now / start / stop / move / autostop /
pods / down:
  A pod is a GPU plus its config (name, GPU choice, ports, which Environment,
  which Volume). It has no versions of its own: Start / Stop / Move / Delete
  are the only pod-level verbs. Rolling back what you installed is an older
  Environment version ("aq env"); rolling back your data is an older Volume
  point ("aq volume"). Neither touches the other.

  aq status <name|id>        Re-check a provisioning or running pod
                             (add --show-secrets to print the password)
  aq save host:<alias>       Detached only: capture a BYO-bucket box into its
                             own configured remote (ogre's own snapshot verb).
                             A managed pod saves its environment and volume
                             automatically on every "aq stop" instead: there
                             is no save button or lineage for one any more.
  aq sync-now host:<alias>   Detached only: force a BYO-bucket box's sync tick
                             right now instead of waiting for its own
                             schedule; it runs no scheduler of its own. A
                             managed pod's volume ticks itself on a
                             leader-elected schedule with no button to force.
  aq start <name|id>         Start a Stopped pod, on any matching GPU (not
                             necessarily the one it last ran on).
                             (--gpu <model>, --max-price <n>, --provider
                             <name>, --gpus <n>; no flags = cheapest anywhere)
  aq stop <name|id>          Save the pod's environment and volume (both
                             confirmed), then release its machine. The pod
                             keeps its config and history; bring it back with
                             "aq start". Never closes before both saves land.
  aq move <name|id> [flags]  Stop the pod, then start it again on a different
                             GPU. A failed Start after the Stop leaves the pod
                             Stopped with its data intact, never mid-air.
                             (same flags as "aq start")
  aq autostop <name|id> on|off
                             Turn this POD's stop-when-idle preference on or
                             off, using the platform's default idle
                             thresholds. This is NOT "aq idle" above: idle
                             policy is a per-DEPLOYMENT threshold config that
                             always outranks this, and this carries no
                             thresholds of its own: use "aq idle set" to
                             change WHEN idle counts as idle, and this to
                             turn auto-stop on pods on/off at all.
  aq pods                    List the pods you own: name, whether it's
                             running, and size.
  aq down <name|id>          Tear a deployment down outright: nothing is
                             saved and it cannot be resumed. This is the
                             lower-level "kill this box" escape hatch;
                             "aq stop" is the everyday, always-saved verb.

env:
  A pod's Environment is everything OUTSIDE /workspace: base image, installed
  packages, startup script. It saves silently with the pod's config on every
  Stop and never needs a click — it only becomes a visible, named thing when
  you Keep or Share it.

  aq env keep <name|id> <name>
                             Name the pod's current environment so it lists
                             under Yours in the New pod picker and survives
                             pod deletion. Mints nothing by itself.
  aq env share <name|id> [--version <id>]
                             Share one version of an environment: mints the
                             next version if the pod changed since the last
                             one (a fresh capture first on a Running pod),
                             then returns a link. Absent --version shares the
                             latest. Refuses if the startup script contains a
                             secret shape (hf_, sk-, AKIA, a PEM header, ...).
  aq env ls                  List environments you can pick from: Built-in,
                             Yours (kept or previously shared), and Shared
                             with you.
  aq env ls <name|id>        List one pod's or one environment's versions
                             (id, version, created, what's included/left out).
  aq env rm <id>             Delete a kept or shared environment. Breaks
                             nothing running; existing share links stop
                             working.

volume:
  A pod's Volume is /workspace: your code, checkpoints, datasets. It has
  automatic history, one point per Stop, and no manual save button.

  aq volume ls                List your volumes: name, size, attached pod.
  aq volume ls <id>            One volume's detail and history (points, with
                             what created each one: pod stopped, auto-stopped).
  aq volume dup <id> <name>   Duplicate a volume: an honest fork, writes never
                             merge back. Attach the copy to a different pod.
  aq volume restore <id> <pointId>
                             Roll the volume back to an earlier point. Refused
                             while the volume is attached to a running pod.
  aq volume rm <id>           Delete a volume and its whole history. Refused
                             while attached.

Environment:
  AQ_API_URL      Aquanode API base (default https://server.aquanode.io/api/v1)
  AQ_CONFIG_DIR   Credential directory (default <user-config-dir>/aq)
  AQ_SSH_KEY      Private key to use for box access (default: your ~/.ssh key,
                  else aq's managed ~/.ssh/aquanode_ed25519)
  AQ_NO_BROWSER   Set to skip auto-opening the approval URL
  AQ_ALLOW_PROD   Set to 1 to allow "aq up"/"aq deploy"/"aq start"/"aq move" to
                  rent hardware on a non-local host from a script or other
                  non-interactive shell. Same effect as passing --prod.
                  Typing at a terminal needs neither.

Exit codes:
  1               The command failed
  12              "aq import": Aquanode publishes no ogre build for this
                  machine's OS/architecture, so aq cannot survey the box from
                  here. Retrying will not help
`

func usage() {
	fmt.Fprint(os.Stderr, usageText)
}
