#!/bin/sh
# aq installer — one-command install of the Aquanode control CLI.
#
#   curl -fsSL https://github.com/Aquanodeio/aq-releases/releases/latest/download/install.sh | sh
#
# What it does:
#   1. Detects OS/arch (linux/darwin × amd64/arm64).
#   2. Resolves the release on Aquanodeio/aq-releases (latest published, or $AQ_VERSION).
#   3. Downloads the matching tarball + checksums.txt and VERIFIES the SHA-256
#      before installing (refuses to install unverified).
#   4. Installs the single `aq` binary into $AQ_BIN_DIR (default /usr/local/bin),
#      falling back to ~/.local/bin if that dir is not writable.
#
# aq is a laptop CLI — there is no service to supervise. After install, run
# `aq version` to confirm, then `aq login` to pair it to your account.
#
# Environment overrides (all optional):
#   AQ_VERSION       pin a release tag (e.g. v0.1.0); default = latest published
#   AQ_PRERELEASE    if set (e.g. 1), select the newest release INCLUDING
#                     prereleases (-rc/-dev/-beta tags); default = stable only
#   AQ_BIN_DIR       install dir for the binary (default /usr/local/bin, then ~/.local/bin)
#   AQ_RELEASES_REPO owner/repo that hosts the release artifacts (default Aquanodeio/aq-releases)
set -eu

# ----- config -------------------------------------------------------------
RELEASES_REPO="${AQ_RELEASES_REPO:-Aquanodeio/aq-releases}"
# AQ_API overrides the GitHub API base (used by tests / mirrors); default is the
# public GitHub releases API for $RELEASES_REPO.
API="${AQ_API:-https://api.github.com/repos/${RELEASES_REPO}/releases}"

info() { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mwarn:\033[0m %s\n' "$*" >&2; }
err()  { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

# ----- prerequisites ------------------------------------------------------
command -v curl >/dev/null 2>&1 || err "curl is required"
command -v tar  >/dev/null 2>&1 || err "tar is required"

# sha256 tool — coreutils on Linux is sha256sum; macOS has shasum.
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    err "no sha256 tool found (need sha256sum or shasum)"
  fi
}

# ----- platform detection -------------------------------------------------
# Map uname output to the goreleaser archive naming (.goreleaser.yml):
#   OS:   Linux / Darwin   (title-cased, as uname -s already prints)
#   arch: x86_64 (amd64) / arm64 (aarch64)
os="$(uname -s)"
case "$os" in
  Linux)  OS="Linux" ;;
  Darwin) OS="Darwin" ;;
  *) err "aq supports Linux and macOS only (detected ${os})" ;;
esac

arch="$(uname -m)"
case "$arch" in
  x86_64|amd64)  ARCH="x86_64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *) err "aq ships amd64/arm64 builds only (detected ${arch})" ;;
esac

asset_suffix="${OS}_${ARCH}.tar.gz"

# ----- resolve the release JSON -------------------------------------------
if [ -n "${AQ_VERSION:-}" ] && [ "${AQ_VERSION}" != "latest" ]; then
  info "resolving aq release ${AQ_VERSION}"
  release_json="$(curl -fsSL "${API}/tags/${AQ_VERSION}")" \
    || err "release tag ${AQ_VERSION} not found in ${RELEASES_REPO}"
elif [ -n "${AQ_PRERELEASE:-}" ]; then
  info "resolving newest aq release (including prereleases) from ${RELEASES_REPO}"
  # ?per_page=1 orders by creation date and excludes DRAFTS only — it happily
  # returns a prerelease (-rc/-dev/-beta). That's exactly what AQ_PRERELEASE
  # opts into, as an explicit escape hatch for testers.
  release_json="$(curl -fsSL "${API}?per_page=1")" \
    || err "could not query releases for ${RELEASES_REPO}"
else
  info "resolving latest stable aq release from ${RELEASES_REPO}"
  # GitHub's /releases/latest excludes BOTH drafts and prereleases — it is the
  # only endpoint that matches what "latest" means to a stable-install user,
  # and it's also what the advertised `.../releases/latest/download/install.sh`
  # URL resolves against, so the two must agree.
  # It 404s if the repo has no non-prerelease releases at all (e.g. every tag
  # published so far is an -rc); fall back to ?per_page=1 (newest of ANY kind,
  # drafts excluded) only in that case, so the installer still works.
  #
  # A plain curl failure (network blip, DNS) and a 403 rate-limit or 5xx look
  # IDENTICAL to a 404 if all we check is curl's exit status — but only a 404
  # actually means "verifiably no stable release". A rate-limit/5xx/network
  # error means "could not determine", and that must NOT fall into the same
  # branch as "verifiably missing": silently falling back there would install
  # an unverified prerelease exactly when we can least tell what's going on.
  # So: fetch WITHOUT -f (to get the body+status on non-2xx too), capture the
  # HTTP status explicitly, and only 404 gets the fallback — every other
  # failure (403/5xx/network) is a hard error, never a silent downgrade.
  latest_resp="$(curl -sS -L -w '\n%{http_code}' "${API}/latest")" && curl_rc=0 || curl_rc=$?
  if [ "$curl_rc" -ne 0 ] || [ -z "${latest_resp:-}" ]; then
    err "could not reach the GitHub API to resolve the latest release for ${RELEASES_REPO} (network error, curl exit ${curl_rc}) — refusing to fall back to an unverified release"
  fi
  latest_http_code="$(printf '%s\n' "$latest_resp" | tail -n1)"
  latest_body="$(printf '%s\n' "$latest_resp" | sed '$d')"
  case "$latest_http_code" in
    200)
      release_json="$latest_body"
      ;;
    404)
      warn "${RELEASES_REPO} has no stable release yet — falling back to newest (may be a prerelease)"
      release_json="$(curl -fsSL "${API}?per_page=1")" \
        || err "could not query releases for ${RELEASES_REPO}"
      ;;
    *)
      err "GitHub API returned HTTP ${latest_http_code} while resolving the latest release for ${RELEASES_REPO} — refusing to fall back to an unverified release (likely rate-limited or a transient error; retry, or pin an exact release with AQ_VERSION)"
      ;;
  esac
fi

tag="$(printf '%s' "$release_json" | grep -o '"tag_name": *"[^"]*"' | head -1 | sed 's/.*": *"//; s/"$//')"
[ -n "$tag" ] || err "no release found in ${RELEASES_REPO}"

# Asset URLs — match by suffix so the artifact prefix can change without breaking us.
urls="$(printf '%s' "$release_json" | grep -o '"browser_download_url": *"[^"]*"' | sed 's/.*": *"//; s/"$//')"
tarball_url="$(printf '%s' "$urls" | grep "_${asset_suffix}\$" | head -1)"
sums_url="$(printf '%s' "$urls" | grep 'checksums\.txt' | head -1)"
[ -n "$tarball_url" ] || err "release ${tag} has no ${asset_suffix} build"
[ -n "$sums_url" ]    || err "release ${tag} has no checksums.txt (refusing to install unverified)"

# ----- download + verify --------------------------------------------------
tmp="$(mktemp -d "${TMPDIR:-/tmp}/aq-install.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT INT TERM

info "downloading aq ${tag} (${OS}/${ARCH})"
curl -fsSL -o "${tmp}/aq.tar.gz"     "$tarball_url" || err "download failed: $tarball_url"
curl -fsSL -o "${tmp}/checksums.txt" "$sums_url"    || err "download failed: $sums_url"

tb_name="$(basename "$tarball_url")"
expected="$(awk -v f="$tb_name" '$2 == f {print $1}' "${tmp}/checksums.txt")"
[ -n "$expected" ] || err "no checksum entry for ${tb_name} in checksums.txt"
actual="$(sha256_of "${tmp}/aq.tar.gz")"
if [ "$expected" != "$actual" ]; then
  err "checksum mismatch for ${tb_name} (expected ${expected}, got ${actual})"
fi
info "checksum verified (${actual})"

# ----- install the binary -------------------------------------------------
tar -xzf "${tmp}/aq.tar.gz" -C "$tmp"
[ -f "${tmp}/aq" ] || err "tarball did not contain an 'aq' binary"
chmod +x "${tmp}/aq"

# Pick an install dir: prefer the explicit/default system dir; if it is not
# writable (and we cannot sudo), fall back to a per-user dir on PATH.
BIN_DIR="${AQ_BIN_DIR:-/usr/local/bin}"
SUDO=""
if [ ! -d "$BIN_DIR" ] || [ ! -w "$BIN_DIR" ]; then
  if [ "$(id -u)" -ne 0 ] && command -v sudo >/dev/null 2>&1 && [ -z "${AQ_BIN_DIR:-}" ]; then
    SUDO="sudo"
  elif [ -z "${AQ_BIN_DIR:-}" ]; then
    BIN_DIR="${HOME}/.local/bin"
    info "no write access to /usr/local/bin — installing to ${BIN_DIR}"
  fi
fi
asroot() { if [ -n "$SUDO" ]; then $SUDO "$@"; else "$@"; fi; }

asroot mkdir -p "$BIN_DIR"
asroot install -m 0755 "${tmp}/aq" "${BIN_DIR}/aq"
info "installed ${BIN_DIR}/aq (${tag})"

# ----- confirm + PATH hint ------------------------------------------------
case ":${PATH}:" in
  *":${BIN_DIR}:"*) ;;
  *) warn "${BIN_DIR} is not on your PATH — add it:"
     warn "  export PATH=\"${BIN_DIR}:\$PATH\"" ;;
esac

if "${BIN_DIR}/aq" version >/dev/null 2>&1; then
  info "done — $("${BIN_DIR}/aq" version) ready."
else
  info "done — aq ${tag} installed at ${BIN_DIR}/aq."
fi
info "next: run 'aq login' to pair this CLI to your Aquanode account."
