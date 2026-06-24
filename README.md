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

## Build

```sh
go build ./...
go test ./...
```

## Relationship to ogre

| | runs where | role |
|---|---|---|
| **ogre** | on the GPU box | OSS on-box agent: snapshot / restore / pause / resume / up |
| **aq**   | your laptop    | control CLI: login / deploy / up (wraps ogre + Aquanode API) |
