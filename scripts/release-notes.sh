#!/usr/bin/env bash
# release-notes.sh — generate the body for a GitHub Release, fed to goreleaser
# via `--release-notes <file>` (which skips goreleaser's own changelog pipe
# entirely — see .goreleaser.yml's `changelog.disable`).
#
# Why this exists (workspace ticket #460 A4): goreleaser's built-in changelog
# renders raw commit subjects verbatim, e.g.
#   Merge pull request #21 from Aquanodeio/ticket-422-aq-ssh
#   feat: aq ssh — managed keypair (#422)
# Those trailing `(#N)` / `Merge pull request #N` refs point at issues/PRs in
# the PRIVATE Aquanodeio/aq repo, but the release is published on the PUBLIC
# Aquanodeio/aq-releases repo — GitHub autolinks `#N` there regardless, and
# every one of those links 404s for every reader. There is no goreleaser
# template func to strip a variable `(#N)` (confirmed: no regex func is
# registered in `changelog.format`'s FuncMap), so we generate the notes
# ourselves instead of fighting the template engine.
#
# Usage: scripts/release-notes.sh [output-file]
#   Defaults to writing to stdout if no output-file is given.
set -euo pipefail

out="${1:-/dev/stdout}"

# Resolve the tag that triggered this release. In CI, GITHUB_REF is
# refs/tags/vX.Y.Z; locally, fall back to the most recent tag reachable
# from HEAD (which is what goreleaser itself would be building for).
current_tag="${GORELEASER_CURRENT_TAG:-}"
if [ -z "$current_tag" ]; then
	if [ -n "${GITHUB_REF:-}" ] && [[ "$GITHUB_REF" == refs/tags/* ]]; then
		current_tag="${GITHUB_REF#refs/tags/}"
	else
		current_tag="$(git describe --tags --abbrev=0)"
	fi
fi

# The tag immediately before current_tag, in version order — mirrors what
# goreleaser calls `previous_tag`. Empty if this is the first tag ever.
previous_tag="$(git tag --sort=-v:refname | awk -v cur="$current_tag" '
	found { print; exit }
	$0 == cur { found = 1 }
')"

if [ -n "$previous_tag" ]; then
	range="${previous_tag}..${current_tag}"
else
	range="$current_tag"
fi

{
	echo "## Changelog"
	git log "$range" --no-merges --reverse --pretty=format:'%H%x09%s' |
		grep -Ev $'\t''Merge pull request #[0-9]+ from ' |
		# 1. Drop a trailing " (#N)" ref entirely (the common `git squash`/
		#    PR-title-carried-into-subject case, e.g. "feat: foo (#22)").
		sed -E 's/[[:space:]]*\(#[0-9]+\)[[:space:]]*$//' |
		# 2. Any other bare "#N" mid-subject (e.g. "resolves ticket #423" —
		#    that #423 is this workspace's internal ticket number, not a
		#    GitHub issue, but GitHub autolinks it identically and it still
		#    404s on the public repo) — de-hash it so it can't autolink,
		#    keeping the number for readability.
		sed -E 's/#([0-9]+)/\1/g' |
		awk -F'\t' '{ printf "* %s %s\n", $1, $2 }'
} >"$out"
