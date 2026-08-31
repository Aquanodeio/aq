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

**No account? Run `aq gpus`.** Bare, it prints a market-summary table — one row
per GPU model, cheapest per-GPU rate first, with how many providers and offers
back it — with nothing installed or configured beyond the binary. Add a filter
(`--gpu`, `--provider`, `--region`, `--max-price`) or `--json` and it switches to
a per-offer table listing every individual offer that matches. That table prints
two price columns, `$/GPU-HR` and `$/HR TOTAL`, because the marketplace's raw
rate is a whole-offer total for every provider except Akash (whose feed reports
an already-per-GPU rate); both columns are normalized so they mean the same
thing for every provider, and `--max-price` filters on `$/GPU-HR`. `aq login` is
the next step once you've found a box worth renting.

## Capabilities

`aq` is a complete, production-ready CLI for managing Aquanode GPU deployments:

- **`aq gpus`** — cheapest rate per GPU model across every provider at a glance; add a filter to see individual offers; works with no account
- **`aq login`** — pair the CLI to your Aquanode account via device-login
- **`aq up`** — rent the cheapest matching GPU, provision it, and bring up a working environment (ComfyUI, Jupyter, or custom snapshot) in one command
- **`aq deploy`** — restore a snapshot onto a freshly-rented Aquanode GPU box
- **`aq import`** — run on a box you already rent elsewhere and capture its environment into a new Aquanode setup you can launch on any provider we support
- **`aq ssh [name]`** — get a shell on a box: managed keypair, managed `~/.ssh/config` alias, zero setup
- **`aq status <name|id>`** — check a deployment's status, provisioning state, HTTPS URL, and credentials
- **`aq down <name|id>`** — tear down a deployment and stop billing
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
