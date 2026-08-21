# Security Policy

We take the security of **`aq`** seriously. `aq` is Aquanode's control / funnel CLI — it
handles login tokens and orchestrates renting boxes — so we appreciate reports that help
us keep it and its users safe.

## Reporting a vulnerability

**Please do not report security vulnerabilities through public GitHub issues, discussions,
or pull requests.**

Report it privately through
[GitHub security advisories](https://github.com/Aquanodeio/aq/security/advisories/new),
or email **hi@aquanode.io** if you prefer.

Please do **not** disclose the issue publicly until we have shipped a fix.

Include as much as you can so we can reproduce and triage quickly:

- A description of the issue and its impact (what an attacker can do).
- Steps to reproduce, a proof-of-concept, or the affected code path.
- `aq version`, your OS, and relevant command output.

> On-box snapshot/restore logic lives in `ogre`, Aquanode's closed-source on-box
> agent. If the issue is in the on-box agent rather than the CLI, email it to the same
> address and say so — we will route it.

## What to expect

- **Acknowledgement** within **3 business days**.
- An initial assessment and severity triage within **7 business days**.
- We'll keep you updated on remediation progress and let you know when a fix ships.
- We're happy to credit you in the advisory once the fix is released, unless you'd
  prefer to remain anonymous. We follow **coordinated disclosure** — please give us a
  reasonable window to release a fix before any public write-up.

## Supported versions

`aq` is under active development. Security fixes land on `main` and in the **latest
released version**; please upgrade to the latest release before reporting.

| Version                 | Supported          |
| ----------------------- | ------------------ |
| `main` / latest release | :white_check_mark: |
| Older releases          | :x:                |

Thank you for helping keep `aq` and its users safe.
