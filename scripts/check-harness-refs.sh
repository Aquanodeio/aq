#!/bin/bash
#
# check-harness-refs.sh — fail on a NEW reference to a harness (workspace-root)
# artifact in this repo's own source.
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
# What it catches (see scripts/lib/harness-refs-matcher.py for the full
# matching logic): any UNQUALIFIED `#N` reference — `(#532)`, `ticket #534`,
# `harness #342` — plus `queue/tickets.md`, `.plans/`, `.specs/`, and a "root
# CLAUDE.md" pointer. A `#N` qualified with a known sub-repo name
# (`aquanode-backend#399`, `mjolnir#191`, `console#314`, optionally after an
# "owner/" prefix) is ALLOWED — it resolves to something real for anyone who
# clones this repo standalone. A chained bare ref that continues a qualified
# one on the same line (`aquanode-backend#398/#400`) is allowed too.
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
# unreviewable diff). Those are baselined by exact content into
# scripts/.harness-refs-baseline.txt, keyed as "<path>::<trimmed matched
# line>". A baselined line is allowed to persist; any NEW match not already
# in the baseline fails the check. Burning down the baseline (rewriting an
# old ref to be self-contained) is encouraged — just remove its line from
# the baseline file in the same commit, and the guard will hold the
# improvement.
#
# Usage:
#   scripts/check-harness-refs.sh
#
set -euo pipefail

SCRIPTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPTS_DIR}/.." && pwd)"
cd "$REPO_ROOT"

MATCHER="${SCRIPTS_DIR}/lib/harness-refs-matcher.py"
SELF_REL="scripts/$(basename "${BASH_SOURCE[0]}")"
MATCHER_REL="scripts/lib/harness-refs-matcher.py"
BASELINE_REL="scripts/.harness-refs-baseline.txt"
BASELINE_FILE="${REPO_ROOT}/${BASELINE_REL}"
# The workflow file that invokes this guard necessarily DESCRIBES the
# patterns it checks for (e.g. names "queue/tickets.md" in a comment) —
# exclude it too, same rationale as excluding this script's own source.
WORKFLOW_REL=".github/workflows/check-harness-refs.yml"

# Tracked files only (gitignored/generated output never trips this), minus
# lockfiles/vendor/node_modules/this guard's own files. The matcher itself
# skips CHANGELOG*/CHANGES* files and "Merge pull request #N from" lines.
matches="$(git ls-files -z -- . \
  ':!go.sum' ':!go.mod' \
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

new_violations=()
while IFS= read -r m; do
  [[ -z "$m" ]] && continue
  file="${m%%:*}"
  rest="${m#*:}"
  content="${rest#*:}"
  # Trim leading/trailing whitespace so incidental reindentation of an
  # already-baselined line doesn't spuriously re-flag it.
  trimmed="${content#"${content%%[![:space:]]*}"}"
  trimmed="${trimmed%"${trimmed##*[![:space:]]}"}"
  key="${file}::${trimmed}"
  if [[ -z "${baseline_keys[$key]:-}" ]]; then
    new_violations+=("$m")
  fi
done <<< "$matches"

if [[ ${#new_violations[@]} -gt 0 ]]; then
  echo "FAIL: found NEW reference(s) to a harness (workspace-root) artifact in this repo's source." >&2
  echo >&2
  echo "This repo is cloned and built independently of the aquanode workspace — a citation of" >&2
  echo "an unqualified #N, queue/tickets.md, .plans/, .specs/, or a \"root CLAUDE.md\" pointer" >&2
  echo "is permanently unresolvable to anyone who clones it alone. Rewrite the comment to state" >&2
  echo "its reason self-contained (what and why), or qualify a real cross-repo reference as" >&2
  echo "<repo>#N (e.g. aquanode-backend#399, mjolnir#191, console#314)." >&2
  echo >&2
  echo "Offending line(s):" >&2
  for v in "${new_violations[@]}"; do
    echo "  $v" >&2
  done
  echo >&2
  echo "If this is a pre-existing reference you are only moving/reformatting (not a genuinely" >&2
  echo "new citation), add its \"<path>::<trimmed line>\" key to ${BASELINE_REL}." >&2
  exit 1
fi

echo "OK: no new harness (workspace-root) artifact references found in tracked source."
