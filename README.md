# aq — Aquanode control CLI

[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![DCO](https://img.shields.io/badge/contributions-DCO%20signed--off-green.svg)](CONTRIBUTING.md)

`aq` is the Aquanode control / funnel CLI. It runs on your laptop and talks to the
Aquanode API to **rent a GPU, provision the [ogre](https://github.com/Aquanodeio/ogre)
on-box agent, and restore a snapshot** — turning the open-source `ogre` workflow into
one-command Aquanode deploys.

It is a thin orchestration wrapper over `ogre` + the Aquanode API. It does **not**
reimplement ogre.

## Capabilities

`aq` is a complete, production-ready CLI for managing Aquanode GPU deployments:

- **`aq login`** — pair the CLI to your Aquanode account via device-login
- **`aq up`** — rent the cheapest matching GPU, provision the ogre agent, and bring up a working environment (ComfyUI, Jupyter, or custom snapshot) in one command
- **`aq deploy`** — restore an `ogre` snapshot onto a freshly-rented Aquanode GPU box
- **`aq ssh [name]`** — get a shell on a box: managed keypair, managed `~/.ssh/config` alias, zero setup
- **`aq status <name|id>`** — check a deployment's status, provisioning progress, HTTPS URL, and credentials
- **`aq down <name|id>`** — tear down a deployment and stop billing
- **`aq logout` / `aq whoami`** — manage authentication state

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/Aquanodeio/aq/main/scripts/install.sh | sh
```

This downloads the right binary for your OS/arch from the latest
[GitHub Release](https://github.com/Aquanodeio/aq/releases), verifies its
SHA-256 checksum, and installs `aq` to `/usr/local/bin` (or `~/.local/bin` if
that isn't writable). Then:

```sh
aq version   # confirm the install
aq login     # pair the CLI to your Aquanode account
```

Overrides: `AQ_VERSION` pins a release tag, `AQ_BIN_DIR` sets the install dir.

## Quickstart — the one-command deploy

```sh
aq login                          # pair this CLI to your Aquanode account (device-login)
aq up --comfyui --name my-box     # rent a GPU, provision ogre, bring up a working env
# → HTTPS URL printed when it's ready
aq ssh my-box                     # get a shell on it
aq down my-box                    # tear it down and stop billing
```

`aq` does the renting and provisioning; the durable snapshot/restore that backs it is
the open-source [ogre](https://github.com/Aquanodeio/ogre) agent it installs on the box.

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

## Relationship to ogre

| | runs where | role | license |
|---|---|---|---|
| **ogre** | on the GPU box | OSS on-box agent: snapshot / restore / pause / resume / up | [AGPL-3.0](https://github.com/Aquanodeio/ogre/blob/main/LICENSE) |
| **aq**   | your laptop    | control CLI: login / deploy / up (wraps ogre + Aquanode API) | [Apache-2.0](LICENSE) |

`aq` is intentionally **permissive (Apache-2.0)**: it's the convenience layer that drives
Aquanode deploys, and we want it to be as frictionless to use, fork, and embed as possible.
The OSS *wedge* — the part with real lock-in risk if it were closed — is `ogre`, which is
strong-copyleft **AGPL-3.0** so it can never be taken closed.

## Our promise — no rug-pull

- **`aq` stays open and Apache-2.0.** We won't relicense already-released code out from
  under you, and we won't move the open CLI behind a paywall.
- **You're never locked into `aq`.** `aq` is a convenience wrapper. The durable value —
  snapshot, restore, BYO-bucket — lives in the open-source [ogre](https://github.com/Aquanodeio/ogre)
  agent, which works **standalone on any GPU box** without `aq` or an Aquanode account.
- **BYO-bucket is forever.** Snapshots taken via this flow go to a bucket *you* own
  (through ogre); any hosted convenience is strictly additive and opt-in.

If we ever add paid services, they sit *on top of* these open primitives, not in place of
them. (ogre's matching commitment lives in its [README](https://github.com/Aquanodeio/ogre#our-promise--no-rug-pull).)

## Contributing

Contributions welcome — by **DCO sign-off, not a CLA**. Add a `Signed-off-by` line to each
commit (`git commit -s`) to certify you wrote the change and can submit it under the
project license. See **[CONTRIBUTING.md](CONTRIBUTING.md)** and the **[DCO](DCO)**.

## License

[Apache-2.0](LICENSE) © Aquanode and the aq contributors.
