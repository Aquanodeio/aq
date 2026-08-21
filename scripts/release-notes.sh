#!/usr/bin/env bash
# release-notes.sh — generate the body for a GitHub Release, fed to goreleaser
# via `--release-notes <file>`. The changelog pipe in .goreleaser.yml MUST stay
# enabled (disable: false) because it is what loads this file; disabling the
# pipe means the flag is silently ignored and the release body comes out EMPTY.
#
# Why this exists: goreleaser's built-in changelog renders raw commit subjects
# verbatim, e.g.
#   Merge pull request #21 from Aquanodeio/ticket-422-aq-ssh
#   feat: aq ssh — managed keypair (#422)
# A bare `#N` in a release body published on Aquanodeio/aq-releases autolinks
# to an issue in AQ-RELEASES, which has none — so every such link 404s. There
# is no goreleaser template func to rewrite a variable `(#N)` (confirmed: no
# regex func is registered in `changelog.format`'s FuncMap), so we generate the
# notes ourselves instead of fighting the template engine.
#
# Aquanodeio/aq is PUBLIC as of 2026-08-21, so a PR ref is no longer something
# to delete — it is a working link we were throwing away. `#N` is now QUALIFIED
# to `Aquanodeio/aq#N`, which GitHub renders as a real cross-repo link from the
# release page back to the source. Merge-commit subjects are still dropped:
# they carry internal ticket-numbered branch names and no reader value.
#
# Usage: scripts/release-notes.sh [output-file]
#   Defaults to writing to stdout if no output-file is given.
set -euo pipefail

out="${1:-/dev/stdout}"

# The public repo that PR refs in commit subjects belong to. The release itself
# is published on aq-releases, so an unqualified "#N" would resolve there.
SOURCE_REPO="${SOURCE_REPO:-Aquanodeio/aq}"

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
	git log "$range" --no-merges --reverse --pretty=format:'%s' |
		grep -Ev '^Merge pull request #[0-9]+ from ' |
		# Qualify every "#N" to "Aquanodeio/aq#N" so it links to the PR in the
		# public source repo instead of autolinking to a nonexistent issue in
		# aq-releases. Guarded on a preceding non-slash so re-running over an
		# already-qualified subject cannot double-qualify it.
		sed -E 's@(^|[^/[:alnum:]])#([0-9]+)@\1'"$SOURCE_REPO"'#\2@g' |
		awk '{ printf "* %s\n", $0 }'
} >"$out"
