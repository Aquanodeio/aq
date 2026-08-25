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

Docs: **[docs.aquanode.io](https://docs.aquanode.io/docs)** · Live GPU pricing:
**[aquanode.io/gpu-index](https://www.aquanode.io/gpu-index)**

## Capabilities

`aq` is a complete, production-ready CLI for managing Aquanode GPU deployments:

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
