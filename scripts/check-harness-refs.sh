#!/bin/bash
#
# check-harness-refs.sh — fail on a NEW reference to a harness (workspace-root)
# artifact in this repo's own source.
#
# *** GENERATED/VENDORED FILE — DO NOT HAND-EDIT A COPY OF THIS FILE. ***
# This is the CANONICAL copy, owned by the aquanode harness workspace at
# scripts/lib/check-harness-refs.sh. It is vendored byte-for-byte into each
# sub-repo (aq, mjolnir, ogre, website, aquanode-backend) by
# scripts/govern/sync-harness-refs.sh. Edit the canonical copy and re-run
# that script — never edit a vendored copy directly;
# scripts/govern/lint-harness-refs.sh detects and fails on drift between a
# vendored copy and this file.
# SOURCE-HASH: a405f80293e9ad5da9ff1ce5fd04a8d4c52f0c0e6074bd5ec36277bd6b1224f1
#
# Background: this repo is one of several independent git repos cloned inside
# an `aquanode` meta-repo workspace. The workspace root's CLAUDE.md carries a
# hard rule: product source must never cite a harness artifact — not a bare
# `#N` work-item number from the workspace's `queue/tickets.md`, not
# `.plans/`/`.specs/` (workspace-root, gitignored, deleted once implemented),
# not a "see the root CLAUDE.md" pointer. Anyone who clones THIS repo alone —
# which is the normal way it's consumed — cannot resolve any of those; the
# citation is permanently dangling. It shipped repeatedly, including several
# PRs on the SAME day this guard was written, despite the rule already being
# recorded in CLAUDE.md, because nothing enforced it. This guard is that
# enforcement.
#
# What it catches (see the matcher script alongside this one for the full
# matching logic): any UNQUALIFIED `#N` reference — `(#532)`, `ticket #534`,
# `harness #342` — plus `queue/tickets.md`, `.plans/`, `.specs/`, `LEDGER.md`,
# and a "root CLAUDE.md" pointer. A `#N` qualified with a known sub-repo name
# (`aquanode-backend#399`, `mjolnir#191`, `console#314`, optionally after an
# "owner/" prefix, optionally with one space before the '#') is ALLOWED — it
# resolves to something real for anyone who clones this repo standalone. A
# chained bare ref that continues a qualified one on the same line
# (`aquanode-backend#398/#400`) is allowed too — see the matcher's docstring
# "ACCEPTED GAP" note for the known, deliberate limit of that allowance. A
# reference inside an ALREADY-APPLIED `prisma/migrations/**` file is never a
# blocking violation — see the "APPLIED PRISMA MIGRATIONS ARE IMMUTABLE"
# comment below, right above where that exemption is applied.
#
# An earlier version of this guard matched ONLY `ticket #N` / `harness #N`
# on the theory that bare `#N` didn't happen in this workspace. That
# conclusion came from a tree that had already been hand-fixed hours
# earlier; the actual violations recorded that day were bare — `(#532)`,
# `(#548)`, `(#549)` — and would have sailed through the old pattern. Fixed
# by matching the SHAPE (any unqualified `#N`) instead of a specific
# spelling, so a new prefix word doesn't reopen the same hole.
#
# Baseline: this repo had a large PRE-EXISTING population of these refs when
# the guard was written (rewriting all of them in one pass would be an
# unreviewable diff). Those are baselined into the baseline file alongside
# this script, keyed as "<path>::<reason>", where <reason> is the matcher's
# stable, sorted set of the actual triggering token(s) on that line (e.g.
# "#695", or "queue/tickets.md") — NOT the full trimmed line text. Keying on
# the whole line was tried first and broke on pure reflow: rewrapping or
# reindenting an already-accepted comment changes its line content without
# changing what it cites, so the SAME accepted citation re-triggered as "new"
# purely from where the words happened to wrap (ticket #721: de-branching a
# file reflowed one accepted "#11" citation and it read as brand-new). Keying
# on the cited token(s) instead means a rewrap/reindent/reword of an accepted
# line is silent, while a genuinely different citation (a new number, or a
# literal path) on that same path still fails. A baselined line is allowed
# to persist; any NEW match not already in the baseline fails the check.
# Burning down the baseline (rewriting an old ref to be self-contained) is
# encouraged — just remove its line from the baseline file in the same
# commit, and the guard will hold the improvement.
#
# Usage:
#   scripts/check-harness-refs.sh
#   (or wherever this copy lives in the vendored repo — it locates its own
#   matcher and baseline relative to itself, either at ./lib/<matcher> or
#   directly alongside itself, so a standalone clone works unmodified.)
#
set -euo pipefail

SCRIPTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPTS_DIR}/.." && pwd)"
# aquanode-backend's vendor location is REPO_ROOT/.github/scripts, two
# levels down from REPO_ROOT — recompute REPO_ROOT generically by walking up
# to the nearest git top-level instead of hardcoding a depth.
if command -v git >/dev/null 2>&1 && git -C "$SCRIPTS_DIR" rev-parse --show-toplevel >/dev/null 2>&1; then
  REPO_ROOT="$(git -C "$SCRIPTS_DIR" rev-parse --show-toplevel)"
fi
cd "$REPO_ROOT"

# Matcher lives either at ./lib/harness-refs-matcher.py next to this script
# (aq/mjolnir/ogre/website layout) or directly alongside it (aquanode-backend's
# flat .github/scripts/ layout) — try both so this one canonical runner works
# vendored into either shape.
if [[ -f "${SCRIPTS_DIR}/lib/harness-refs-matcher.py" ]]; then
  MATCHER="${SCRIPTS_DIR}/lib/harness-refs-matcher.py"
elif [[ -f "${SCRIPTS_DIR}/harness-refs-matcher.py" ]]; then
  MATCHER="${SCRIPTS_DIR}/harness-refs-matcher.py"
else
  echo "FAIL: could not locate harness-refs-matcher.py next to or under lib/ of $0" >&2
  exit 2
fi

SELF_ABS="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")"
SELF_REL="${SELF_ABS#"${REPO_ROOT}"/}"
MATCHER_REL="${MATCHER#"${REPO_ROOT}"/}"
BASELINE_REL="$(dirname "$SELF_REL")/.harness-refs-baseline.txt"
BASELINE_FILE="${REPO_ROOT}/${BASELINE_REL}"
# The workflow file that invokes this guard necessarily DESCRIBES the
# patterns it checks for (e.g. names "queue/tickets.md" in a comment) —
# exclude it too, same rationale as excluding this script's own source.
WORKFLOW_REL=".github/workflows/check-harness-refs.yml"

# Tracked files only (gitignored/generated output never trips this), minus
# lockfiles/vendor/node_modules/this guard's own files. The matcher itself
# skips CHANGELOG*/CHANGES* files and "Merge pull request #N from" lines.
matches="$(git ls-files -z -- . \
  ':!go.sum' ':!go.mod' ':!package-lock.json' \
  ':!vendor/**' ':!node_modules/**' \
  ":!${SELF_REL}" ":!${MATCHER_REL}" ":!${BASELINE_REL}" ":!${WORKFLOW_REL}" \
  | python3 "$MATCHER" || true)"

declare -A baseline_keys=()
if [[ -f "$BASELINE_FILE" ]]; then
  while IFS= read -r line; do
    [[ -z "$line" ]] && continue
    [[ "$line" == \#* ]] && continue
    baseline_keys["$line"]=1
  done < "$BASELINE_FILE"
fi

# APPLIED PRISMA MIGRATIONS ARE IMMUTABLE — report, never block. Prisma
# stores a checksum of every migration.sql in `_prisma_migrations`; editing
# an applied file after the fact does not fail `migrate deploy` outright,
# but it warns on EVERY subsequent `migrate deploy`/`migrate status` and
# breaks the exact signal a deploy pipeline uses to confirm a migration
# landed. Rewriting a citation inside one for the sake of a tidier comment
# trades a permanent warning on our own verification path for that
# tidiness — worse than leaving the citation in place. So: any path under
# `prisma/migrations/` is FROZEN — reported below in its own section,
# never counted toward the blocking offender set, and never required to be
# fixed OR baselined (baselining would hide the pattern here for the NEXT
# migration; the path-based exemption is the fix). Mirrors
# scripts/govern/lint-harness-refs.sh's own FROZEN handling in the
# workspace harness — same rule, same rationale, on both sides of the
# harness/product boundary. This is a REPORT, not a silent skip: a NEW
# migration file that has not been applied yet still shows up here so its
# citation can be fixed before it ships (once applied, it can't be).
MIGRATION_PATH_PATTERN='(^|/)prisma/migrations/'

# Distinguish accepted-and-unchanged (in baseline_keys) from newly-introduced
# — never dump the raw match list and make the reader diff it by hand
# against another checkout to find out which is which.
new_violations=()
new_violation_keys=()
frozen_violations=()
while IFS= read -r m; do
  [[ -z "$m" ]] && continue
  reason="${m%%$'\x01'*}"
  m="${m#*$'\x01'}"       # from here on, m is the original "path:lineno:content" for display
  file="${m%%:*}"
  if [[ "$file" =~ $MIGRATION_PATH_PATTERN ]]; then
    frozen_violations+=("$m")
    continue
  fi
  key="${file}::${reason}"
  if [[ -z "${baseline_keys[$key]:-}" ]]; then
    new_violations+=("$m")
    new_violation_keys+=("$key")
  fi
done <<< "$matches"

# Report FROZEN refs BEFORE the blocking check below, unconditionally — even
# when there ARE blocking violations too, so a caller always sees both
# states rather than the frozen list being silently swallowed by an early
# exit 1. Mirrors scripts/govern/lint-harness-refs.sh's own ordering.
if [[ ${#frozen_violations[@]} -gt 0 ]]; then
  echo "FROZEN: ${#frozen_violations[@]} reference(s) in ALREADY-APPLIED prisma migration(s) (not blocking):" >&2
  for f in "${frozen_violations[@]}"; do
    echo "  FROZEN $f" >&2
  done
  echo "  These files are checksummed in _prisma_migrations and must not be edited." >&2
  echo "  Fix the pattern in NEW migrations before they are applied." >&2
  echo >&2
fi

if [[ ${#new_violations[@]} -gt 0 ]]; then
  echo "FAIL: found NEW (unaccepted) reference(s) to a harness (workspace-root) artifact in this repo's source." >&2
  echo >&2
  echo "This repo is cloned and built independently of the aquanode workspace — a citation of" >&2
  echo "an unqualified #N, queue/tickets.md, .plans/, .specs/, LEDGER.md, or a \"root CLAUDE.md\"" >&2
  echo "pointer is permanently unresolvable to anyone who clones it alone. Rewrite the comment to" >&2
  echo "state its reason self-contained (what and why), or qualify a real cross-repo reference as" >&2
  echo "<repo>#N (e.g. aquanode-backend#399, mjolnir#191, console#314)." >&2
  echo >&2
  echo "Offending line(s) — not already in ${BASELINE_REL}:" >&2
  for i in "${!new_violations[@]}"; do
    echo "  ${new_violations[$i]}" >&2
    echo "    baseline key: ${new_violation_keys[$i]}" >&2
  done
  echo >&2
  echo "If this is a pre-existing reference you are only moving/reformatting/rewording (not a" >&2
  echo "genuinely new citation), append the printed \"baseline key:\" line verbatim to" >&2
  echo "${BASELINE_REL}." >&2
  exit 1
fi

if [[ ${#frozen_violations[@]} -gt 0 ]]; then
  echo "OK: no NEW (blocking) harness (workspace-root) artifact references found (${#frozen_violations[@]} frozen ref(s) reported above)."
else
  echo "OK: no new harness (workspace-root) artifact references found in tracked source."
fi
