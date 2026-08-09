#!/usr/bin/env python3
"""
harness-refs-matcher.py — find lines in a list of files that cite a harness
(workspace-root) artifact: an unqualified issue/ticket number, or a literal
path/pointer into the workspace root. Used by ../check-harness-refs.sh (or
./check-harness-refs.sh, per repo).

Read from stdin: NUL-separated tracked file paths (matches `git ls-files
-z`). Prints "path:lineno:content" for every offending line to stdout.

--- Why "unqualified #N" is the rule ---
A bare `#N` (`(#532)`, `ticket #534`, `harness #342`) is ambiguous by
construction: GitHub also autolinks a repo's OWN issues/PRs as `#N`, and
there is no way to tell the two apart by reading the digits — an earlier
version of this guard only matched `ticket #N` / `harness #N` on the theory
that bare `#N` never happened in practice; it was wrong, because that
conclusion was drawn from a tree that had already been hand-fixed. The
actual violations recorded that day were bare: `(#532)`, `(#548)`, `(#549)`.

The workspace's own convention for a CROSS-repo citation is to qualify it
with the repo name — `aquanode-backend#399`, `mjolnir#191`, `console#314` —
which is (a) unambiguous, (b) already how this codebase's own commits/PRs
write a cross-repo reference, and (c) actually resolves for someone who
clones this repo standalone. So: a bare `#N` is a VIOLATION, even a bare
`#N` that would resolve to THIS repo's own GitHub issue/PR (deliberately
stricter than "only flag other-repo refs" — indistinguishable without a
live GitHub API call, which a fast offline check should not make). A
`<repo>#N` reference, where <repo> is one of this workspace's own
sub-repos (optionally after an "owner/" prefix), is ALLOWED. A bare `#N`
that continues a qualified reference via `/` — e.g.
`aquanode-backend#398/#400` — is also allowed, since it reads as the SAME
qualified citation, not a second dangling one.

--- False-positive classes handled ---
- Binary files (images etc.) — skipped outright via a NUL-byte sniff, same
  heuristic `grep -I`/git itself use. An earlier version had no such check
  and flagged raw PNG bytes as "#4" / "#3" ticket refs.
- Hex color literals — `#2A2A2A`, Tailwind's `bg-[#0F0E11]`, SVG
  `fill="#78ABFF"` etc. `#[0-9]+` only captures the LEADING DIGIT RUN (e.g.
  "#2" out of "#2A2A2A"), so any match immediately followed by a hex letter
  (a-f/A-F) is a color, not an issue number, and is allowed regardless of
  file type. A fully-decimal-looking hex code (`#000`, `"#000000"`) has no
  letter to catch it, so it's separately allowed when quote-delimited
  (`"#000"`, `'#000'`, `` `#000` ``) or in `property: #000;`-shaped CSS, at
  the standard hex lengths (3/4/6/8 digits) — a real ticket number is never
  written inside quotes with no other text, or immediately after a bare
  CSS-declaration colon.
- Third-party GitHub issue citations in prose/docs (blog posts citing e.g.
  "ComfyUI-Manager issue #2008") — allowed when the SAME line also carries
  a `github.com` URL, since the citation is then self-contained (resolves
  for a reader with no workspace context) rather than a dangling internal
  pointer.
- `CHANGELOG*`/`CHANGES*` files and "Merge pull request #N from ..." lines
  (goreleaser/git-generated text) are skipped outright.
"""
import re
import sys

KNOWN_REPOS = [
    "aquanode-backend",
    "admin-panel",
    "aq",
    "console",
    "docs",
    "mjolnir",
    "ogre",
    "website",
]
# Longest-first so "aquanode-backend" matches before a hypothetical shorter
# overlapping name would.
REPO_ALT = "|".join(sorted(KNOWN_REPOS, key=len, reverse=True))

NUM_REF = re.compile(r"#[0-9]+")
QUALIFIED_TAIL = re.compile(
    r"(?:^|[^A-Za-z0-9_./-])(?:[A-Za-z0-9_.-]+/)?(?:" + REPO_ALT + r")$"
)

LITERAL_PATTERNS = [
    re.compile(r"queue/tickets\.md"),
    re.compile(r"\.plans/"),
    re.compile(r"\.specs/"),
    re.compile(r"root CLAUDE\.md"),
]

HEX_LETTERS = set("abcdefABCDEF")
HEX_COLOR_LENS = (3, 4, 6, 8)
QUOTE_CHARS = {'"': '"', "'": "'", "`": "`", "[": "]"}

CHANGELOG_NAME = re.compile(r"(^|/)(CHANGELOG|CHANGES)(\.[A-Za-z0-9]+)?$", re.IGNORECASE)
MERGE_PR_LINE = re.compile(r"Merge pull request #[0-9]+ from ")
GITHUB_URL = re.compile(r"github\.com")

BINARY_SNIFF_BYTES = 8192


def looks_binary(raw: bytes) -> bool:
    return b"\x00" in raw[:BINARY_SNIFF_BYTES]


def line_has_literal(line: str) -> bool:
    return any(p.search(line) for p in LITERAL_PATTERNS)


def is_hex_color_ref(line: str, start: int, end: int) -> bool:
    """True if the #<digits> at [start, end) is a hex color literal, not an
    issue/ticket number."""
    digits_len = end - start - 1
    # "#2A2A2A" — our digit-only regex stops at "#2", and the very next
    # char is a hex letter. An issue number is never immediately glued to
    # a letter with no separator.
    if end < len(line) and line[end] in HEX_LETTERS:
        return True
    if digits_len not in HEX_COLOR_LENS:
        return False
    # Quote-delimited: "#000", '#000', `#000` — the whole token is quoted
    # with nothing else inside.
    if start > 0 and end < len(line):
        opener = line[start - 1]
        closer = QUOTE_CHARS.get(opener)
        if closer is not None and line[end] == closer:
            return True
    # Bare CSS declaration: `property: #000;` / `property:#000` end-of-line.
    prefix = line[:start].rstrip()
    if prefix.endswith(":"):
        tail = line[end:].lstrip()
        if tail == "" or tail.startswith(";") or tail.startswith(","):
            return True
    return False


def find_numeric_violations(line: str) -> bool:
    matches = list(NUM_REF.finditer(line))
    if not matches:
        return False
    if GITHUB_URL.search(line):
        # A same-line github.com URL makes the citation self-contained —
        # typically a third-party issue tracker link in prose/docs, not a
        # dangling internal pointer.
        return False
    allowed_end = {}  # end-of-match-index -> True if that match was allowed
    for m in matches:
        start, end = m.start(), m.end()
        prefix = line[:start]
        allowed = False
        if QUALIFIED_TAIL.search(prefix):
            allowed = True
        elif prefix.endswith("/") and (start - 1) in allowed_end:
            # Chain continuation: "...#398/#400" — the '/' sits directly
            # after a previously-allowed match.
            allowed = True
        elif is_hex_color_ref(line, start, end):
            allowed = True
        allowed_end[end] = allowed
        if not allowed:
            return True
    return False


def main() -> None:
    # Some tracked files carry non-UTF-8/lone-surrogate bytes (binary-ish
    # fixtures, odd encodings); read them permissively and replace
    # unencodable bytes on output rather than crashing the guard.
    sys.stdout.reconfigure(errors="replace")
    data = sys.stdin.buffer.read()
    paths = [p.decode("utf-8", "surrogateescape") for p in data.split(b"\0") if p]
    for path in paths:
        if CHANGELOG_NAME.search(path):
            continue
        try:
            with open(path, "rb") as fh:
                raw = fh.read()
        except (IsADirectoryError, FileNotFoundError, PermissionError):
            continue
        if looks_binary(raw):
            continue
        text = raw.decode("utf-8", "surrogateescape")
        for i, line in enumerate(text.split("\n"), start=1):
            if MERGE_PR_LINE.search(line):
                continue
            if line_has_literal(line) or find_numeric_violations(line):
                print(f"{path}:{i}:{line}")


if __name__ == "__main__":
    main()
