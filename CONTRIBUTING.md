# Contributing to aq

Thanks for your interest in making `aq` better. `aq` is Aquanode's control / funnel CLI,
released under the **[Apache License 2.0](LICENSE)** (permissive). We welcome issues,
fixes, and features.

This project uses a **Developer Certificate of Origin (DCO)** — **not a CLA**. You keep
the copyright to your contribution; you just certify that you have the right to submit it
under the project's license. No paperwork, no rights assignment.

## TL;DR

```bash
git checkout -b my-change
# … make your change …
go build ./... && go vet ./... && go test ./...   # build + vet + test
git commit -s -m "fix: clear message"             # -s adds the DCO sign-off
git push origin my-change
# open a PR against Aquanodeio/aq:main
```

## Developer Certificate of Origin (sign-off, not a CLA)

Every commit must carry a `Signed-off-by` line certifying you agree to the
[DCO](DCO) (full text in this repo). The `-s` / `--signoff` flag adds it for you:

```bash
git commit -s -m "your message"
```

which appends, using your real name and email (matching your `git config user.name`
and `user.email`):

```
Signed-off-by: Jane Developer <jane@example.com>
```

By signing off you certify the Developer Certificate of Origin 1.1: that you wrote the
change (or have the right to submit it) and agree it's contributed under the Apache-2.0.

- **Forgot to sign off?** Amend the last commit: `git commit --amend -s --no-edit`,
  then force-push your branch. For multiple commits, rebase with
  `git rebase --signoff <base>`.
- **Use your real identity.** Anonymous / fake-email sign-offs aren't accepted, since
  the sign-off is a legal certification.

## Before you open a PR

1. **Discuss large changes first.** For anything beyond a small fix, open an issue so
   we can agree on direction before you invest the time.
2. **Build, vet, and test.** `aq` is Go:
   ```bash
   go build ./...
   go vet ./...
   go test ./...
   ```
3. **Keep `aq` thin.** `aq` is a control wrapper over [`ogre`](https://github.com/Aquanodeio/ogre)
   + the Aquanode API — it does **not** reimplement ogre. On-box snapshot/restore logic
   belongs in ogre; `aq` orchestrates renting a box and provisioning the agent.
4. **Match the surrounding code.** Follow the existing style, keep changes focused, and
   update `README.md` when behavior changes.

## Reporting bugs & requesting features

Open a GitHub issue with:

- What you expected vs. what happened
- Steps to reproduce (commands, env vars, OS if relevant)
- `aq version` and relevant output

For anything security-sensitive, please **do not** open a public issue — report it
privately via the repo's security advisories instead.

## License

By contributing, you agree that your contributions are licensed under the
[Apache-2.0](LICENSE), the same license that covers the project.
