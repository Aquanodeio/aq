#!/usr/bin/env python3
"""
harness-refs-matcher.py — find lines in a list of files that cite a harness
(workspace-root) artifact: an unqualified issue/ticket number, or a literal
path/pointer into the workspace root. Used by ../check-harness-refs.sh (or
./check-harness-refs.sh, per repo).

*** GENERATED/VENDORED FILE — DO NOT HAND-EDIT A COPY OF THIS FILE. ***
This is the CANONICAL copy, owned by the aquanode harness workspace at
scripts/lib/harness-refs-matcher.py. It is vendored byte-for-byte into each
sub-repo (aq, mjolnir, ogre, website, aquanode-backend) by
scripts/govern/sync-harness-refs.sh. Edit the canonical copy and re-run that
script — never edit a vendored copy directly; scripts/govern/lint-harness-refs.sh
detects and fails on drift between a vendored copy and this file.
SOURCE-HASH: d960e9cbb1cd21f7d3b72426c3f0934f2bbd86d34493232225e8d7f5cf865e61

Read from stdin: NUL-separated tracked file paths (matches `git ls-files
-z`). Prints "reason\x01path:lineno:content" for every offending line to
stdout — `reason` is the sorted, comma-joined set of the actual triggering
tokens on that line (e.g. "#695" or "queue/tickets.md"), NOT the full line
text. ../check-harness-refs.sh keys the baseline on "<path>::<reason>"
instead of the full trimmed line so a pure reflow/reindent/rewording of an
already-accepted citation doesn't change its key and spuriously re-flag it —
only the (path, cited token) pair matters. The `\x01` separator is safe:
line content can contain ':' or ',' but never that control byte (binary
files are already skipped before this point).

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

--- ACCEPTED GAP: chain-continuation is not verified against the SAME chain ---
The `/`-continuation rule above (`allowed_end[end]` / "prefix.endswith('/')
and (start-1) in allowed_end") does not check that the continuing `#N`
belongs to the SAME issue chain as the qualified ref before it — it accepts
ANY `#N` immediately after a `/` that follows ANY previously-allowed match,
including a genuinely dangling second reference that merely happens to sit
right after a real one. `// see ogre#122/#999999` PASSES this guard even
though `#999999` is a bare, unqualified, almost certainly bogus citation —
because it is textually identical in shape to the legitimate multi-PR
citation `aquanode-backend#398/#400`, and telling the two apart would need
either a live GitHub lookup (ruled out — this is a fast offline check) or a
much narrower heuristic that was measured to break real usage. Measured
blast radius of tightening this: 12 real chain citations across 6 repos
would newly fail. RULING (do not re-derive): this gap is ACCEPTED, not
fixed. If you are re-reading this because a bogus chained ref shipped, that
is the known, deliberate tradeoff — do not "fix" it without re-measuring
that 12-citation blast radius and getting a new ruling.

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
- HTML numeric character entities — `&#39;` (decimal) and `&#x27;` (hex) —
  are not issue numbers; the `#` there is never a citation, it's markup.
  `#[0-9]+` alone would still flag the decimal shape (`&#39;` contains
  `#39`) even though `&#x27;` already fails to match (a hex entity's `#` is
  followed by a literal `x`, not a digit, so `#[0-9]+` never starts there).
  Both shapes are matched and excluded explicitly by span so the exclusion
  doesn't rely on that adjacency accident holding forever.
- Prose ENUMERATION, not citation — "citation check #2", "step #3", "option
  #1" — where `#N` is standing in for "number N" of a list the surrounding
  prose is itself defining, not pointing out at a workspace-root ticket.
  Recognized structurally: an enumeration noun (check/step/case/item/
  option/part/phase) immediately before the `#N`, allowed regardless of the
  digits' value. This is NOT a magnitude/threshold rule (e.g. "harness
  tickets are currently 3 digits so anything under 100 is prose") — a
  numeric-range rule has a shelf life by construction, since ticket numbers
  only climb, and would need to be re-tuned forever. The word immediately
  before the `#` is what actually distinguishes the two cases and doesn't
  decay as the ticket counter grows, so it has no expiry to track.
- `LEDGER.md` — a workspace-root harness artifact too (a stub since its
  content moved into the `.claude/skills/billing-ledger` workspace skill),
  unresolvable to anyone who clones a vendored repo standalone, same as
  `queue/tickets.md`/`.plans/`/`.specs/`/"root CLAUDE.md" above. Flagged as
  a LITERAL_PATTERNS entry after product-source citations to it shipped
  undetected because no pattern here covered it until this one was added.
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
# HTML numeric character entities — &#39; (decimal) / &#x27; (hex) — whose
# `#` is markup, never an issue citation. See the module docstring's "HTML
# numeric character entities" note.
HTML_ENTITY_REF = re.compile(r"&#(?:[0-9]+|x[0-9a-fA-F]+);")
# A single optional space between the repo name and the '#' is accepted —
# "ogre #122" reads as correctly qualified to a human, and rejecting it is a
# silent trap: the guard fails on something that LOOKS right, which reads as
# the guard being wrong rather than as flagging a real problem. Still
# requires one of the known repo tokens immediately before (a bare "foo
# #123" is unaffected — "foo" isn't in REPO_ALT).
QUALIFIED_TAIL = re.compile(
    r"(?:^|[^A-Za-z0-9_./-])(?:[A-Za-z0-9_.-]+/)?(?:" + REPO_ALT + r") ?$"
)
# A `#N` immediately preceded by one of these nouns is prose enumerating a
# list the surrounding text itself defines ("citation check #2", "step #3"),
# not a citation of a workspace-root ticket. See the module docstring's
# "Prose ENUMERATION" note for why this is word-shaped, not number-shaped.
ENUM_WORD_TAIL = re.compile(
    r"\b(?:check|step|case|item|option|part|phase)\s*$", re.IGNORECASE
)

# (regex, stable label used in the baseline reason key — NOT the raw
# .pattern, which carries regex escaping like "queue/tickets\.md")
LITERAL_PATTERNS = [
    (re.compile(r"queue/tickets\.md"), "queue/tickets.md"),
    (re.compile(r"\.plans/"), ".plans/"),
    (re.compile(r"\.specs/"), ".specs/"),
    (re.compile(r"root CLAUDE\.md"), "root CLAUDE.md"),
    # LEDGER.md is a workspace-root harness artifact too — see the module
    # docstring's "LEDGER.md" note above for the full rationale.
    (re.compile(r"LEDGER\.md"), "LEDGER.md"),
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


def literal_violations(line: str) -> list:
    return [label for p, label in LITERAL_PATTERNS if p.search(line)]


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


def find_numeric_violations(line: str) -> list:
    matches = list(NUM_REF.finditer(line))
    if not matches:
        return []
    if GITHUB_URL.search(line):
        # A same-line github.com URL makes the citation self-contained —
        # typically a third-party issue tracker link in prose/docs, not a
        # dangling internal pointer.
        return []
    entity_spans = [(m.start(), m.end()) for m in HTML_ENTITY_REF.finditer(line)]
    allowed_end = {}  # end-of-match-index -> True if that match was allowed
    violations = []
    for m in matches:
        start, end = m.start(), m.end()
        prefix = line[:start]
        allowed = False
        if any(e_start <= start and end <= e_end for e_start, e_end in entity_spans):
            # &#39; / &#x27; — an HTML numeric character entity, not a
            # citation. (The hex shape's `#` is never followed by a digit,
            # so it wouldn't hit NUM_REF anyway; handled here explicitly
            # rather than relying on that.)
            allowed = True
        elif QUALIFIED_TAIL.search(prefix):
            allowed = True
        elif prefix.endswith("/") and (start - 1) in allowed_end:
            # Chain continuation: "...#398/#400" — the '/' sits directly
            # after a previously-allowed match. See the module docstring's
            # "ACCEPTED GAP" note: this does not verify same-chain identity.
            allowed = True
        elif is_hex_color_ref(line, start, end):
            allowed = True
        elif ENUM_WORD_TAIL.search(prefix):
            allowed = True
        allowed_end[end] = allowed
        if not allowed:
            violations.append(m.group(0))
    return violations


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
            reasons = literal_violations(line) + find_numeric_violations(line)
            if reasons:
                reason = ",".join(sorted(set(reasons)))
                print(f"{reason}\x01{path}:{i}:{line}")


if __name__ == "__main__":
    main()
