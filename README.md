# aq — Aquanode control CLI

[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![DCO](https://img.shields.io/badge/contributions-DCO%20signed--off-green.svg)](CONTRIBUTING.md)

`aq` is the command-line client for **[Aquanode](https://www.aquanode.io)**. It runs on
your laptop and talks to the Aquanode API to **rent a GPU, provision it, and restore a
snapshot** — turning a multi-step GPU rental into a one-command deploy.

It is a thin orchestration wrapper over the Aquanode API and does not reimplement
any of the box-side provisioning itself.

**What Aquanode is:** a GPU cloud where your *environment* survives. Stop a box and
your packages, model weights, custom nodes and config are still there when you come
back — on the same provider or a different one. If you have ever reinstalled the same
ComfyUI custom nodes at the start of every session, that is the problem it exists for.
Aquanode prices GPUs across many providers in one place, so `aq up` rents the cheapest
box that matches what you asked for.

Docs: **[docs.aquanode.io](https://docs.aquanode.io/docs)** · Live GPU pricing in the
browser: **[aquanode.io/gpu-index](https://www.aquanode.io/gpu-index)**

**No account? Run `aq gpus`.** Bare, it prints a market-summary table, one row
per GPU model, cheapest per-GPU rate first, with how many providers and offers
back it, with nothing installed or configured beyond the binary. Add a filter
(`--gpu`, `--provider`, `--region`, `--max-price`) or `--json` and it switches to
a per-offer table listing every individual offer that matches, for example:

```console
$ aq gpus --gpu H100 --limit 8
GPU   GPUS  VRAM  PROVIDER      REGION        AVAIL  $/GPU-HR  $/HR TOTAL
H100  1     80GB  massecompute  us-central-3  1      2.4500    2.4500
H100  2     80GB  massecompute  us-central-3  2      2.4500    4.9000
H100  1     80GB  hyperstack    CANADA-1      1      2.5250    2.5250
H100  1     80GB  hyperstack    CANADA-1      1      2.5250    2.5250
H100  1     94GB  runpod        unknown       1      2.5900    2.5900
H100  2     94GB  runpod        unknown       2      2.5900    5.1800
showing 8 of 71 offers; use --limit 0 for all
```

That table prints two price columns, `$/GPU-HR` and `$/HR TOTAL`, because the
marketplace's raw rate is a whole-offer total for every provider except Akash
(whose feed reports an already-per-GPU rate); both columns are normalized so
they mean the same thing for every provider, and `--max-price` filters on
`$/GPU-HR`. `aq login` is the next step once you've found a box worth renting.

## Capabilities

`aq` is a complete, production-ready CLI for managing Aquanode GPU deployments:

- **`aq gpus`** — cheapest rate per GPU model across every provider at a glance; add a filter to see individual offers; works with no account
- **`aq login`** — pair the CLI to your Aquanode account via device-login
- **`aq up`** — rent the cheapest matching GPU, provision it, and bring up a working environment (ComfyUI, Jupyter, or custom snapshot) in one command
- **`aq watch` / `aq queue`**: wait for a sold-out GPU on the server (a watch that tells you, or `aq up --when-available` that rents it for you) and manage what you queued
- **`aq deploy`** — restore a snapshot onto a freshly-rented Aquanode GPU box
- **`aq import`**: run on a box you already rent elsewhere and capture its data into a new Aquanode volume, ready to attach to a pod
- **`aq start` / `aq stop` / `aq move`**: bring a pod up on any matching GPU, save its environment and volume and release the box, or move a running pod to a different GPU
- **`aq env` / `aq volume`**: keep or share an environment version; list, duplicate, restore, or delete a volume's history
- **`aq ssh [name]`** — get a shell on a box: managed keypair, managed `~/.ssh/config` alias, zero setup
- **`aq status <name|id>`** — check a deployment's status, provisioning state, HTTPS URL, and credentials
- **`aq down <name|id>`**: tear down a deployment outright, nothing saved
- **`aq logout` / `aq whoami`** — manage authentication state

## Install

```sh
curl -fsSL https://github.com/Aquanodeio/aq-releases/releases/latest/download/install.sh | sh
```

This downloads the right binary for your OS/arch from the latest
[GitHub Release](https://github.com/Aquanodeio/aq-releases/releases), verifies its
SHA-256 checksum, and installs `aq` to `/usr/local/bin` (or `~/.local/bin` if
that isn't writable). Then:

```sh
aq version   # confirm the install
aq login     # pair the CLI to your Aquanode account
```

Overrides: `AQ_VERSION` pins a release tag, `AQ_BIN_DIR` sets the install dir.

Or, with a Go toolchain (1.26+):

```sh
go install github.com/Aquanodeio/aq@latest
```

## Use it from an AI coding agent

This repo ships an agent skill, [`aquanode-gpu-jobs`](skills/aquanode-gpu-jobs/SKILL.md),
that teaches a coding agent to run a batch job on a cloud GPU with `aq` and bring
the results back. The agent tells you the GPU and the price and waits for your
yes before it starts anything.

Claude Code:

```
/plugin marketplace add Aquanodeio/aq
/plugin install aquanode@aquanode
```

Codex, Cursor, Gemini CLI, GitHub Copilot and other agents that read skills:

```sh
npx skills add Aquanodeio/aq
```

Then ask for what you want, e.g. "fine-tune this on an A100 and bring back the adapter".

## Quickstart — the one-command deploy

```sh
aq login                          # pair this CLI to your Aquanode account (device-login)
aq up --comfyui --name my-box     # rent a GPU, provision it, bring up a working env
# → HTTPS URL printed when it's ready
aq ssh my-box                     # get a shell on it
aq down my-box                    # tear it down and stop billing
```

`aq` does the renting and provisioning; snapshot/restore is a durable, standalone
capability of the box itself, so your data outlives any single `aq` session.

## What's running

```sh
aq ls              # live boxes: id, name, status, GPU, provider, hourly rate, age
aq ls --all        # include closed and failed ones
```

The rate column always names its currency (`0.1500 USD`). That is the provider's
own denomination — not always dollars — so it is never printed as a bare `$`.

## Multiple GPUs

```sh
aq up --gpus 4                  # only consider offers with at least 4 GPUs
aq deploy --snapshot ext-42 --gpus 2
```

Default is 1, max is 8. CPU, RAM and storage scale with the count.

Worth knowing: on providers that sell whole nodes, you get the whole physical box
whatever you ask for, and the price is the same — so `--gpus` there is how you
stop paying node price for a box configured with one GPU's worth of CPU and RAM.
On providers that sell per-GPU slices it picks a different box. `--max-price` is
compared against the **whole offer's** price, not the per-GPU price.

## When the GPU you want is sold out

Two requests that live on the Aquanode server, so they keep working while your
laptop sleeps. The server re-checks the market every 30 to 120 seconds.

```sh
aq watch --gpu H100 --max-price 3          # arm a watch: you get an email when a match appears
aq watch --gpu H100 --wait --timeout 25m   # arm it and block on it
aq watch list                              # active watches
aq watch wait 12 --timeout 25m             # reattach to a watch by id
aq watch cancel 12

aq up --when-available --gpu H100 --max-price 3   # queue a bare box; it is RENTED when supply appears
aq queue list
aq queue cancel 3
```

- `--gpu` is the model name exactly as `aq gpus` spells it. `aq watch` fixes the
  letter case from the live feed and warns (but still arms) when the name is not
  in it.
- `--gpus N` is matched **exactly**, not "at least": a 2-GPU offer does not
  satisfy a watch for 1.
- `--max-price` is the **whole offer's** hourly price, not per-GPU. On
  `--when-available` it is required: the box is rented with nobody watching, so
  the ceiling is a mandatory spend rail. A queued box is a bare box with idle
  auto-stop off, so `--comfyui`, `--jupyter`, `--auto-stop`, `--warn-after`,
  `--stop-after`, `--show-secrets` and `host:` targets are refused. `--name` is
  allowed.
- `aq up --when-available` goes through the same production guard as any `aq up`:
  from a script or agent it needs `--prod` (or `AQ_ALLOW_PROD=1`).
- Waiting polls the one watch row every 15 seconds. A network error or 5xx does
  not end the wait; a 401, 403 or 404 does. Ctrl-C leaves the watch armed.

Exit codes for `aq watch --wait` and `aq watch wait`: **0** matched, **13** the
watch ended with no match (expired or cancelled), **14** `--timeout` elapsed and
the watch is still armed (run `aq watch wait <id>` to pick it up; this is not a
failure). With `--json`, stdout is exactly one JSON object, the watch row.

## Run your local code on the box

`aq push` sends your working directory up; `aq run` sends it and then runs something
in it, with your terminal attached:

```sh
aq push                              # current directory -> /workspace on your only live box
aq run -- python train.py            # push, then run it there (Ctrl-C reaches the process)
aq run my-box -- python train.py     # pick the box by name or id
aq run --no-push -- nvidia-smi       # skip the upload, just run
```

Edit locally, `aq run` again — that's the loop. Nothing to install on the box, and it
uses the same SSH alias described below, so `scp`/`rsync`/VSCode keep working alongside it.

| Flag | Meaning |
|---|---|
| `--from <dir>` | Directory to send (default: the current one) |
| `--to <dir>` | Where it lands on the box (default: `/workspace`, must be absolute) |
| `--exclude <pat>` | Skip matching paths — repeatable |
| `--no-default-excludes` | Send `.git`, `node_modules`, `__pycache__` and friends too |
| `--delete` | Make the box mirror your local tree, removing what you deleted |
| `--print` | Print the transfer command instead of running it |
| `--dir <dir>` | *(run)* Directory to run in — defaults to where the push landed |
| `--no-push` | *(run)* Don't send anything first |

Add a `.aqignore` next to your code — one pattern per line, `#` for comments — for
excludes you want every push to apply. Datasets and checkpoints belong there; pushing
them over ssh is slower than fetching them on the box.

By default `aq` skips version-control metadata, virtualenvs, module trees and
interpreter caches, which is what keeps a first push seconds rather than minutes.

### Long runs

`aq run` holds your terminal, so closing the laptop kills the run. For anything
long, detach it:

```sh
aq run --detach -- python train.py    # prints a run id and returns
aq logs -f                            # follow the newest run
aq logs --list                        # every run on this box, state, command
aq logs --run 20260826-141230 -n 500  # a specific one
```

A detached run keeps going after you disconnect, writes its output to a file on
the box, and records its exit code — so a log that stops moving tells you whether
it finished, crashed, or is just quiet. The run id goes to stdout on its own, so
`RUN=$(aq run --detach -- python train.py)` works.

**Transport.** If both your machine and the box have `rsync`, `aq` uses it and only
changed files move. Otherwise it falls back to tar over ssh, which re-sends the whole
tree — several provider images ship without `rsync`. `aq` prints which one it used, so
a slow push is never a mystery. `--delete` needs `rsync` and fails loudly without it
rather than silently leaving deleted files on the box.


## SSH

`aq ssh` is the whole connect story — no key to create, no IP to copy, no id to remember:

```sh
aq ssh                            # your only live box — just connects
aq ssh my-box                     # by the --name you gave it (or by numeric id)
aq ssh my-box -- nvidia-smi       # run one command instead of opening a shell
aq ssh my-box -L 8888:localhost:8888   # forward a port
aq ssh my-box --print             # print the ssh command instead of running it
```

**The alias is the point.** `aq` maintains `~/.ssh/aquanode.config` — one `aq-<name>`
Host block per live box — and adds a single `Include` line to your own `~/.ssh/config`.
So everything that speaks `ssh_config` works with no `aq` involved:

```sh
ssh aq-my-box
scp big.safetensors aq-my-box:/workspace/
rsync -av ./data/ aq-my-box:/workspace/data/
code --remote ssh-remote+aq-my-box /workspace
```

What `aq` touches, and why:

| Path | Owner | Notes |
|---|---|---|
| `~/.ssh/aquanode_ed25519[.pub]` | `aq` | Generated on first use **only if** you have no usable key. An existing `~/.ssh/id_ed25519` is preferred so your setup stays un-fragmented; `AQ_SSH_KEY` overrides both. |
| `~/.ssh/aquanode.config` | `aq` | Regenerated wholesale from your live deployments on every `aq ssh` / `up` / `down`. Don't edit it. |
| `~/.ssh/aquanode_known_hosts` | `aq` | Aquanode boxes only, so a recycled provider IP can never trigger a host-key warning against *your* infrastructure. |
| `~/.ssh/config` | **you** | `aq` adds exactly three lines, once, between `# BEGIN aquanode` / `# END aquanode` markers. Everything outside them is left byte for byte. Delete the block to opt out. |

Two details worth knowing. The `Include` goes at the **top** of your config because
`ssh_config` is first-match-wins per keyword — appended blocks lose to any earlier
`Host *` stanza you already have. And because the platform does not publish box host
keys, the generated stanzas use a dedicated known_hosts with
`StrictHostKeyChecking accept-new`: no prompt on first connect, but a *changed* key on
a later connect still fails loudly.

## Build from source

```sh
go build ./...
go test ./...
```

Releases are cut by pushing a `v*` tag — `.github/workflows/release.yml` runs
[GoReleaser](https://goreleaser.com) (`.goreleaser.yml`) to build the
linux/darwin × amd64/arm64 binaries and publish the Release.

## Our promise — no rug-pull

- **`aq` stays Apache-2.0.** We won't relicense already-released code out from
  under you, and we won't move the CLI behind a paywall. The `LICENSE` file ships
  inside every release tarball alongside the binary.
- **BYO-bucket is forever.** Snapshots taken via this flow go to a bucket *you* own;
  any hosted convenience is strictly additive and opt-in.

If we ever add paid services, they sit *on top of* these commitments, not in place of
them.

## Contributing

Contributions welcome — by **DCO sign-off, not a CLA**. Add a `Signed-off-by` line to each
commit (`git commit -s`) to certify you wrote the change and can submit it under the
project license. See **[CONTRIBUTING.md](CONTRIBUTING.md)** and the **[DCO](DCO)**.

## License

[Apache-2.0](LICENSE) © Aquanode and the aq contributors.
