# aq — Aquanode control CLI

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

## Build from source

```sh
go build ./...
go test ./...
```

Releases are cut by pushing a `v*` tag — `.github/workflows/release.yml` runs
[GoReleaser](https://goreleaser.com) (`.goreleaser.yml`) to build the
linux/darwin × amd64/arm64 binaries and publish the Release.

## Relationship to ogre

| | runs where | role |
|---|---|---|
| **ogre** | on the GPU box | OSS on-box agent: snapshot / restore / pause / resume / up |
| **aq**   | your laptop    | control CLI: login / deploy / up (wraps ogre + Aquanode API) |
