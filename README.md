# aq — Aquanode control CLI

[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![DCO](https://img.shields.io/badge/contributions-DCO%20signed--off-green.svg)](CONTRIBUTING.md)

`aq` is the Aquanode control / funnel CLI. It runs on your laptop and talks to the
Aquanode API to **rent a GPU, provision the [ogre](https://github.com/Aquanodeio/ogre)
on-box agent, and restore a snapshot** — turning the open-source `ogre` workflow into
one-command Aquanode deploys.

It is a thin orchestration wrapper over `ogre` + the Aquanode API. It does **not**
reimplement ogre.

## Status

Scaffold. Subcommands are built out by the funnel tickets:

- `aq login` — device-login pairing the CLI to an Aquanode account
- `aq up`    — rent a GPU + provision ogre + bring up a working env, one command
- `aq deploy` — restore an `ogre` snapshot onto a freshly-rented Aquanode box
- `aq status <id>` — re-check a deployment's status, HTTPS URL, and credentials
- `aq down <id>` — tear a deployment down (stop the rented GPU box + billing)

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
aq login                 # pair this CLI to your Aquanode account (device-login)
aq up --comfyui          # rent a GPU, provision ogre, bring up a working env
# → HTTPS URL printed when it's ready; tear it down with:
aq down <id>
```

`aq` does the renting and provisioning; the durable snapshot/restore that backs it is
the open-source [ogre](https://github.com/Aquanodeio/ogre) agent it installs on the box.

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
