---
name: aquanode-gpu-jobs
description: Run a batch job on a cloud GPU with Aquanode's `aq` CLI and bring the results back. Use when the user wants to run something on a GPU they do not have locally (H100, A100, L40S, RTX 4090 and others), for example fine-tuning or training a model, batch inference, transcription, embeddings, evals, benchmarks or rendering, from any Docker image and a command.
license: Apache-2.0
---

# Run a GPU job on Aquanode

A job is one Docker image plus one command, run to completion on a rented GPU
machine. Whatever the command writes to `/outputs` can be downloaded afterwards.
The machine is released when the job ends.

## Rules

1. **A job rents real hardware and charges the user's Aquanode balance.** Before
   starting one, tell the user the GPU models, the hourly price and your estimate
   of the total, and wait for a yes. If the user already gave you a budget for
   this job, an estimate inside it counts as the yes. Never start a job the user
   has not agreed to.
2. **Always pass `--detach`.** Without it `aq job run` follows the log with no
   time limit, which outlives most shell tool timeouts. Start the job detached,
   then check on it in bounded loops (step 5).
3. **Never put a token or password in a command line.** Ask the user to store it
   as a secret themselves (see "Secrets" below).
4. If the user changes their mind, or you started something by mistake, stop it
   with `aq job cancel <job>`.

Every `aq` command below that takes `<job>` accepts the job's id or its name.

## 1. Install and log in

```bash
aq version || curl -fsSL https://github.com/Aquanodeio/aq-releases/releases/latest/download/install.sh | sh
aq job run -h 2>&1 | grep -q -- '--image' || echo "aq is too old for image jobs: re-run the install command"
aq whoami
```

The installer puts `aq` in `/usr/local/bin`, or `~/.local/bin` when that is not
writable, so that directory must be on `PATH`. Every `aq` call prints a one-line
`aq: api https://...` banner on stderr; it is not an error.

If `aq whoami` says `Not logged in`, run `aq login`. It prints a URL and a
pairing code, then waits up to 10 minutes for the user to approve the code in
their browser. Show the user the URL and the code. If your shell tool cannot show
output from a command that is still running, ask the user to run `aq login` in
their own terminal instead (in Claude Code: `! aq login`).

## 2. Pick the GPU and agree on the cost

```bash
aq gpus                    # every model, cheapest $/GPU-hour first; needs no login
aq gpus --gpu A100         # one model's offers (substring match)
aq gpus --max-price 1.5    # only models under $1.50 per GPU-hour
```

- Use the exact spelling from the `GPU` column for `--gpu-model`.
- `VRAM` is per GPU, in GB, even on rows that print a bare number.
- Pick by the VRAM the workload needs, then name two or three models that fit
  (repeat `--gpu-model`). A job tied to one scarce model can wait a long time
  for a machine. With several models it takes the cheapest available first.
- Prefer recent cards (A10, L4, L40S, A100, H100, RTX 30 and 40 series) unless
  the user asks otherwise: the newest framework builds may not support older
  ones such as the V100.
- Estimate: `$/GPU-hour x number of GPUs x expected hours`, using the most
  expensive model you named. Tell the user this number before you start.

## 3. Get the code into the job

There is no file upload. The command has to bring everything it needs:

- **The user's code fetched by the command**, e.g. `git clone` a repo, or `curl`
  a tarball, as the first step of the command.
- **A short script inlined into the command** (see below).
- **A public image that already has the tool**, for example an inference server
  or training framework image.
- **An image the user builds and pushes** to a registry they own.

The image must contain `sh` and either `curl` or `wget`. Without them the job
ends as `image_bootstrap_failed`. Many slim images do not have them, including
the `pytorch/pytorch` and `nvidia/cuda` runtime tags.

When nothing else is needed, use **`aquanodeio/job-base:cuda12.8-py3-1`**:
Ubuntu 24.04 with the CUDA 12.8 runtime, a Python virtualenv (so `pip install`
just works), `git`, `curl` and `wget`, starting in `/workspace`. Install what
the job needs at the start of the command.

To inline a short local script without quoting trouble, base64 it:

```bash
SCRIPT=$(base64 < run.py | tr -d '\n')
# then, as the command after `--`:
#   bash -c "echo $SCRIPT | base64 -d > run.py && pip install -q torch && python run.py"
```

Write results to `/outputs` (or pass `--output-path <dir>`). Only that path is
downloaded afterwards.

### Secrets

For a token the command needs (for example `HF_TOKEN`), ask the user to run this
themselves. It reads the value from stdin, so the token never appears in your
transcript or their shell history:

```bash
aq secret set HF_TOKEN --type env
```

Then pass `--secret HF_TOKEN`. The job sees it as the environment variable
`HF_TOKEN`. For a private image, the user runs
`aq secret set <name> --type registry --server <host> --username <user> --token <token>`
and you pass `--registry-secret <name>`.

## 4. Start it

Flags go before the bare `--`. Everything after `--` is the command. Give the
job a name no other job of the user's has, so you can refer to it later:

```bash
aq job run --image aquanodeio/job-base:cuda12.8-py3-1 \
  --gpu-model L40S --gpu-model A100 \
  --name finetune-$(date +%m%d-%H%M) \
  --detach \
  -- bash -c 'pip install -q torch transformers && git clone https://github.com/<user>/<repo> && cd <repo> && python train.py --out /outputs'
```

It prints `job <id>`. Keep that id (or the name).

Other flags you may need:

| Flag | Meaning |
|---|---|
| `--gpus 1\|2\|4\|8` | GPUs on one machine (default 1). The command must use them itself, e.g. `torchrun --nproc_per_node=2`. |
| `--disk-gb <n>` | Disk size (default 100). |
| `--any-gpu` | Accept any GPU model on the market instead of naming them. |
| `--gpu-order ordered` | With several `--gpu-model`, prefer them in the order given instead of cheapest first. |
| `--secret <name>` | Inject an env secret (repeatable). |

## 5. Watch it

```bash
aq job ls             # ID, NAME, STATUS, GPU, DURATION, COST for every job
aq job logs <job>     # the log so far
```

Getting a machine and starting the image usually takes a few minutes before the
first log line. Poll in loops that stay under your shell tool's timeout, and
stop as soon as the job ends:

```bash
JOB=<id>
for i in $(seq 1 9); do
  s=$(aq job ls 2>/dev/null | awk -v id="$JOB" '$1==id {print $3}')
  echo "$(date +%H:%M:%S) $s"
  case "$s" in succeeded|failed|UNSERVABLE|cancelled) break;; esac
  sleep 60
done
```

Run the loop again while the job is still `queued` or `running`. A job ends in
one of these states:

- `succeeded`: go to step 6.
- `failed`: the user's command exited non-zero. Read `aq job logs <job>`, fix the
  command, and start a new job.
- `UNSERVABLE`: Aquanode could not get the job a machine. This is not the user's
  code. Start it again with more `--gpu-model` choices.
- `cancelled`: someone stopped it.

If a job sits at `queued` for more than about ten minutes, the usual causes are
a balance too low to start a machine, or no machine free for the models you
named. Ask the user to check their balance at https://console.aquanode.io/billing.
A run waiting on credits starts on its own once they are added. If the balance
is fine, cancel it and start again with more `--gpu-model` choices.

## 6. Bring back the results

```bash
aq job pull <job> ./results
```

Without a directory it pulls into `./<job>-<runId>/`. Inside, the job's log is
at `log`, and everything it wrote to `/outputs` is under `outputs/`, keeping its
paths (so `/outputs/results.json` lands at `./results/outputs/results.json`).
Tell the user what came back, where, and what the job cost (the `COST` column
of `aq job ls`).

To run the same thing again, `aq job rerun <job>` makes a new job with the same
image, command, GPUs and secrets. It prints the new job's id; watch it the same
way.

## If the GPU you want is sold out

Do not poll `aq gpus` in a loop. Arm a watch on the server instead; it survives
the shell and the laptop, and emails the user once when a match appears:

```bash
aq watch --gpu "H100" --max-price 3 --wait --timeout 20m   # id is printed on stderr
echo "exit $?"
```

Pick a `--timeout` under your shell tool's limit, and when it exits 14 run
`aq watch wait <id> --timeout 20m` again, in a loop, until it exits 0 or 13.
Exit codes:

- `0`: a matching offer appeared. Start the job now (step 4), still after the
  user has agreed to the cost.
- `13`: the watch ended with no match (it expired after 72 hours by default, or
  was cancelled). Offer to arm another or pick other GPUs.
- `14`: your `--timeout` elapsed. The watch is still armed on the server: this is
  not a failure. Call `aq watch wait <id>` again.
- `1`: a real error (not logged in, watch not found). Read the message.

Some providers (AWS today) publish no stock at all, so a plain watch can never
fire on them. Add `--include-unreported` to also fire when nothing says the
offer is sold out. That match means worth trying, not known to be free: a launch
after it can still be refused for capacity, so expect to retry. Such a watch
sends no email; the exit code of `--wait` / `aq watch wait <id>` is the only
delivery.

`--gpu` must be the exact model name from `aq gpus`, and `--gpus` is matched
exactly (not "at least"). `--max-price` is the whole offer's hourly price.
`aq watch list` shows active watches and `aq watch cancel <id>` stops one; cancel
what the user no longer wants.

`aq up --when-available --gpu <model> --max-price <n>` is different: it RENTS a
bare box on its own when supply appears, with nobody watching. Only use it when
the user has agreed to that spend, and tell them the queue id, and that
`aq queue cancel <id>` stops it.

## More

- GPU jobs on the CLI: https://docs.aquanode.io/docs/jobs/cli
- Full command reference: `aq help`
