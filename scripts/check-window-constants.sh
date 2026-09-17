#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# The `release_lease=on_remove` window is three constants added up, and
# the tree states that sum in prose: the reference, the internals page,
# the release notes, the comment over `releaseSettle`, and the tests
# that name the window (#984). A statement like that can go stale when
# a constant moves, and it can be wrong from the start: the doc comment
# in `pkg/plugin/deferred_release_test.go` stated a floor of 60 in the
# same commit that set `releaseSettle` to 5, which had already put the
# floor at 65. None of them is reached by a rename, a refactor or a
# test: they are sentences, and nothing in the build read them before
# this gate.
#
# THIS COMMENT STATES NO COUNT OF THEM. The gate counts them on every
# run and prints the total. A number written here would be one more
# sentence about this window with nothing keeping it true, which is the
# defect this gate exists to catch, and it would sit in the one file
# the gate cannot read.
#
# THE DERIVATION. Read from the Go source, never from any prose:
#
#   tombstoneTTL       how long a stopped container keeps its MAC, and
#                      therefore how long the address is still its own
#   releaseSettle      how far past the deadline the sweep waits
#   ipamSweepInterval  how often the sweep runs
#
#   floor   = tombstoneTTL + releaseSettle
#            a sweep landing exactly on the settle sends then
#   ceiling = floor + ipamSweepInterval
#            a sweep that ran a moment before the settle expired cannot
#            send until the next one, so the worst case is one whole tick
#
# WHAT IT CHECKS, in blocks of the tree that name this feature:
#
#   1. A BAND. A sentence that says it is talking about this window --
#      the wall clock, the datagram, the time after the stop -- and
#      states "<a> to <b> seconds" is required to state the derived
#      floor and ceiling. Keyed on the derivation and not on the current
#      values: "60 to 80 seconds" is caught while 65 is the floor, and
#      so is "65 to 80" once a constant moves. The feature's own name is
#      NOT one of those subjects, because a paragraph about `on_remove`
#      may state some other range of seconds and saying so is not
#      claiming to state the window.
#   2. A CONSTANT, PAIRED WITH ITS SUBJECT. Each duration in a sentence
#      about this window is paired with the NEAREST of the three
#      subjects and checked against that one constant. A sentence
#      routinely carries two ("the sweep runs every 15 seconds and waits
#      5 seconds past the deadline"), and testing only that each number
#      is one of this window's five numbers passes that same sentence
#      with the two swapped, which is exactly backwards and is the
#      sentence a reader uses to understand the mechanism. A tie between
#      two subjects, or a subject further away than PAIR_REACH, is no
#      pairing: the number then gets the weaker test rather than a
#      guessed one. A duration in subject position that names no subject
#      at all ("Five seconds is three orders of magnitude...") is paired
#      with the constant its block DECLARES.
#   3. THE POPULATION. Each of REQUIRED_BAND_DOCS must state the band.
#      A guard that only fires at zero is satisfied by a population of
#      one: delete the sentence from the reference and the internals
#      page, leave one in a Go comment, and a gate asking only "more
#      than none" certifies a tree whose readers can no longer find the
#      number.
#
# A SENTENCE, NOT A LINE. The prose wraps: RELEASE_NOTES.md states the
# tick as "runs every 15\n  seconds", which no line-keyed scan sees.
# Lines of a block are joined before the sentence split, so a statement
# broken across two lines is read as one.
#
# THE SPELLINGS. Written from the sites in this tree, which spell these
# five values nine ways: "65 to 80 seconds", "tombstone TTL, 60
# seconds", "every 15 seconds", "15 s", "(15s)", "5 seconds", "Five
# seconds", "5s" and `5 * time.Second`. Digits, number-words, a bare
# `s`, a parenthesised `s` and a Go literal are all read.
#
# WHAT IT CANNOT SEE, stated here rather than discovered later:
#
#   * A SUBJECT IT DOES NOT KNOW. Membership of a block, of a sentence
#     within it, and the pairing itself are keyed on spelling
#     (SUBJECT_TABLE and BAND_SUBJECT below). A sentence that states the
#     tick without naming a sweep, a tick or a frequency is invisible,
#     and so is a band stated without saying it is this window's. That
#     half is a ratchet against the next drift, not evidence that every
#     sentence in the tree is true. REQUIRED_BAND_DOCS is what keeps the
#     population from being emptied underneath it, and a range of
#     seconds in a feature block that carries none of BAND_SUBJECT's
#     phrases is PRINTED as unjudged, so the blindness is in the output
#     and not only in this paragraph.
#   * ITSELF. Both scripts are excluded from the sweep (SELF below),
#     because this file quotes the spellings it hunts and would report
#     every one of them as a stale sentence. The cost is that nothing
#     checks the prose in this comment, which is why it states the
#     derivation and the method and leaves every count to the run.
#   * A LOOSE STATEMENT. "the address goes back about a minute later"
#     survives 5s becoming 7s and does not survive the window becoming
#     five minutes, so it is not a claim this gate can check against an
#     exact number. Each one found is printed as a NOTE with its file
#     and line, so the set is visible rather than silently excluded.
#
# It REFUSES rather than passing when it cannot see: a constant that no
# longer parses, an empty domain, a document in REQUIRED_BAND_DOCS that
# does not exist, or a tree in which no band statement is found at all
# (a universal gate is satisfied by emptying its domain, and this one's
# domain is prose that nothing else protects).
#
# A line may say `window-exempt: <reason>` to state a duration beside
# one of these subjects for a reason that is not this window. It exempts
# THE LINE IT IS WRITTEN ON and nothing else: not the paragraph, and not
# the next line, because either wider reach lets one marker silence a
# statement it was not written for. The line to put it on is the one the
# report names, which for a statement that wraps is the line it starts
# on. A marker anywhere else leaves the statement red, which is the
# direction this has to fail in. An exempted band is also not one of the
# statements a required document is credited with, so the marker cannot
# be used to empty the set either.
#
# Usage: bash scripts/check-window-constants.sh [--root <dir>]
# Exit:  0 every statement agrees with the derivation
#        1 a statement disagrees
#        2 cannot see -- refuse rather than pass
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
while [ "$#" -gt 0 ]; do
    case "$1" in
        --root) ROOT="${2:-}"; shift 2 ;;
        *) echo "usage: $0 [--root <dir>]" >&2; exit 2 ;;
    esac
done

if [ ! -d "$ROOT" ]; then
    echo "FAIL  --root $ROOT is not a directory" >&2
    exit 2
fi

export WINDOW_ROOT="$ROOT"
python3 - <<'PY'
import os, re, sys

root = os.environ["WINDOW_ROOT"]

def refuse(title, msg):
    print(f"REFUSE  {title}: {msg}", file=sys.stderr)
    sys.exit(2)

# ---------------------------------------------------------------- derive

# The constants are found by name across the package, not at a path, so
# moving one between files does not quietly empty this gate.
UNIT = {"Second": 1, "Minute": 60, "Millisecond": 0}

def go_sources():
    out = []
    for d in ("pkg", "cmd"):
        base = os.path.join(root, d)
        for dirpath, _, names in os.walk(base):
            for n in names:
                if n.endswith(".go"):
                    out.append(os.path.join(dirpath, n))
    return sorted(out)

sources = go_sources()
if not sources:
    refuse("No Go sources", f"no *.go under {root}/pkg or {root}/cmd, so the three "
                            "constants cannot be read and the prose has nothing to be "
                            "checked against")

def derive(name):
    pat = re.compile(rf"\b{name}\s*=\s*(\d+)\s*\*\s*time\.(Second|Minute)\b")
    hits = []
    for p in sources:
        try:
            text = open(p, encoding="utf-8", errors="replace").read()
        except OSError:
            continue
        for m in pat.finditer(text):
            hits.append((p, int(m.group(1)) * UNIT[m.group(2)]))
    if not hits:
        refuse("A constant vanished",
               f"{name} is not declared as `<n> * time.Second` anywhere under "
               f"{root}/pkg or {root}/cmd. It was renamed, moved or given another "
               "shape; the window this gate checks can no longer be derived, so it "
               "refuses instead of passing on a stale number.")
    vals = {v for _, v in hits}
    if len(vals) > 1:
        refuse("A constant is declared twice",
               f"{name} has {len(vals)} different values: " +
               ", ".join(f"{p}={v}s" for p, v in hits))
    return hits[0][1]

tombstone = derive("tombstoneTTL")
settle = derive("releaseSettle")
tick = derive("ipamSweepInterval")
floor = tombstone + settle
ceiling = floor + tick

CONSTANTS = {"tombstoneTTL": tombstone, "releaseSettle": settle,
             "ipamSweepInterval": tick}
DERIVED = {tombstone, settle, tick, floor, ceiling}
DERIVED_TEXT = (f"tombstoneTTL={tombstone}s, releaseSettle={settle}s, "
                f"ipamSweepInterval={tick}s, floor={floor}s, ceiling={ceiling}s")

# ---------------------------------------------------------------- domain

# A block names this feature, or lives in a file that exists for it.
# A file NAME is not a statement about the window: `release_lease_test.go`
# in a list of test files must not drag that list into the domain.
FEATURE = re.compile(r"(?:on_remove|release_lease|releaseSettle|ReleaseOnRemove|"
                     r"deferred release|sweepDeferredReleases|handRetainedRecordsBack|"
                     r"handOneRecordBack|restart window)(?![a-z_]*\.go)", re.I)
FEATURE_FILE = re.compile(r"(deferred_release[a-z_]*\.go|release_lease_on_remove[a-z_]*\.go)$")

# WHERE THE BAND MUST BE STATED. A guard that only fires at zero is
# satisfied by a population of one: delete the sentence from the
# reference and the internals page, leave one in a Go comment, and a
# gate counting "is it more than none" certifies a tree whose users can
# no longer find the number. These three answer three different readers
# and each has to carry it. A document renamed or retired is an edit to
# this list, made on purpose, not a silence.
REQUIRED_BAND_DOCS = ("docs/reference.md", "docs/internals.md", "RELEASE_NOTES.md")

SCAN_EXT = (".md", ".go")
SKIP_DIRS = {".git", "vendor", "node_modules", "dnsmasq-2.91"}

def candidate_files():
    out = []
    for dirpath, dirnames, names in os.walk(root):
        dirnames[:] = [d for d in dirnames if d not in SKIP_DIRS]
        for n in names:
            if n.endswith(SCAN_EXT):
                out.append(os.path.join(dirpath, n))
    return sorted(out)

# This gate's own text states every spelling it hunts for; reading it
# would report the header as a stale document.
SELF = {os.path.join(root, "scripts", "check-window-constants.sh"),
        os.path.join(root, "scripts", "test-check-window-constants.sh")}

# ----------------------------------------------------------- durations

WORDS = {"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6,
         "seven": 7, "eight": 8, "nine": 9, "ten": 10, "eleven": 11,
         "twelve": 12, "thirteen": 13, "fourteen": 14, "fifteen": 15,
         "sixteen": 16, "twenty": 20, "thirty": 30, "forty": 40,
         "fifty": 50, "sixty": 60, "ninety": 90}
NUMWORD = "|".join(sorted(WORDS, key=len, reverse=True))
NUM = rf"(?:\d+(?:\.\d+)?|{NUMWORD})"
SECS = r"(?:seconds|second|secs|sec|s)\b"
MINS = r"(?:minutes|minute|min|m)\b"
# "15 s", "15s", "(15s)", "15 seconds", "Five seconds", "a minute",
# and Go's `15 * time.Second`.
DURATION = re.compile(
    rf"\b({NUM})\s*\*\s*time\.(Second|Minute)\b"          # Go literal
    rf"|\b({NUM})\s?({SECS})"                              # 15s / 15 s / 15 seconds
    rf"|\b({NUM})\s?({MINS})"                              # 2 minutes
    rf"|\b(an?|one)\s(minute)\b",                          # a minute
    re.I)
# A hedge, or a unit coarser than the second: "about a minute later"
# survives a constant moving by seconds and does not survive the window
# becoming five minutes, so it is not a claim about an exact number.
HEDGE = re.compile(r"\b(about|roughly|around|approximately|nearly|almost|~)\s*$", re.I)
EXEMPT = re.compile(r"window-exempt:", re.I)

def durations(text):
    """[(seconds, raw, is_loose, offset)] for every duration in `text`."""
    out = []
    for m in DURATION.finditer(text):
        if m.group(1):
            val = float(m.group(1)) * (1 if m.group(2) == "Second" else 60)
        elif m.group(3):
            n = m.group(3).lower()
            val = float(n) if n[0].isdigit() else WORDS[n]
        elif m.group(5):
            n = m.group(5).lower()
            val = (float(n) if n[0].isdigit() else WORDS[n]) * 60
        else:
            val = 60.0
        minutes = bool(m.group(5) or m.group(7) or (m.group(1) and m.group(2) == "Minute"))
        out.append((val, m.group(0).strip(),
                    minutes or bool(HEDGE.search(text[:m.start()])), m.start()))
    return out

# ------------------------------------------------------------- subjects

# Keyed on spelling, and each written from a site in this tree. A
# duration is PAIRED with the nearest of these, and checked against that
# one constant: a number in a sentence about this window can be wrong by
# being attached to the wrong subject, which is the defect :934 had.
SUBJECT_TABLE = [
    ("tombstoneTTL", "the tombstone TTL",
     re.compile(r"tombstone", re.I)),
    ("releaseSettle", "the settle",
     re.compile(r"settle|past the deadline|passed by", re.I)),
    ("ipamSweepInterval", "the sweep tick",
     re.compile(r"sweeper|sweep|\btick|every(?=\s)", re.I)),
]
SUBJECTS = re.compile("|".join(rx.pattern for _, _, rx in SUBJECT_TABLE), re.I)

# How far a subject may be from a duration and still be what it is about.
# Beyond this the pairing is not made and the number falls back to the
# weaker test, that it is one of this window's five numbers at all.
PAIR_REACH = 60
# "65 to 80 seconds", and the dashed range the tree uses elsewhere
# ("15-30s", "4.0-7.0s") so a band rewritten into that shape is still read.
BAND = re.compile(rf"\b({NUM})\s*(?:to|-|\u2013|\u2014)\s*({NUM})\s?(?:seconds|second|s)\b", re.I)
# ...but only where the sentence says it is THIS window. Every one of
# the band sentences in this tree carries one of these; a paragraph that
# mentions the feature and then states some other range of seconds, such
# as how long a slow runner takes to start a container, is not a
# statement of this window and must not be told that it is.
# The feature's own name is deliberately NOT one of these. A paragraph
# about `on_remove` may state some other range of seconds, and naming
# the feature is not saying "this range is the window".
BAND_SUBJECT = re.compile(
    r"wall clock|datagram|after the stop|for the next"
    r"|from (?:the |`?docker`? )?stop", re.I)
# A duration in SUBJECT position is a statement about a constant even
# when it names no other subject: deferred_release.go opens with "Five
# seconds is three orders of magnitude more than the gap it covers".
LEADING = re.compile(rf"^[\s`*\"(]*{NUM}\s?(?:{SECS}|{MINS})\s+(?:is|was|are|were)\b", re.I)

# --------------------------------------------------------------- blocks

def blocks(path, lines):
    """[(start_line, [lines])] -- a markdown paragraph, a table row of its
    own, or a Go comment block (a lone `//` ends one)."""
    md = path.endswith(".md")
    out, cur, start = [], [], 0
    def flush():
        nonlocal cur, start
        if cur:
            out.append((start, cur))
        cur, start = [], 0
    for i, raw in enumerate(lines, 1):
        s = raw.strip()
        sep = (s == "" or s == "//" or
               (md and s.startswith("|")) or
               (md and s.startswith("#")))
        if md and s.startswith("|"):
            flush()
            out.append((i, [raw]))
            continue
        if sep:
            flush()
            continue
        if not cur:
            start = i
        cur.append(raw)
    flush()
    return out

def segments(path, lines):
    """A block's lines as (is_prose, text) segments. Comment and prose
    lines are joined, so a statement wrapped across two lines reads as
    one; a line of Go code is a segment of its own and not prose,
    because a declaration is not a sentence of the comment above it."""
    go = path.endswith(".go")
    out, cur = [], []
    for raw in lines:
        s = raw.strip()
        if go and not s.startswith("//"):
            if cur:
                out.append((True, " ".join(cur)))
                cur = []
            out.append((False, s))
            continue
        s = re.sub(r"^//\s?", "", s)
        s = re.sub(r"^[-*]\s+", "", s)
        cur.append(s)
    if cur:
        out.append((True, " ".join(cur)))
    return out

SENT = re.compile(r"(?<=[.!?])\s+(?=[A-Z`*\"(])")

# A block that DECLARES one of the three says which constant its prose
# is about, which is how "Five seconds is three orders of magnitude..."
# is paired at all: it names no subject and sits above `const
# releaseSettle`.
DECLARES = re.compile(r"\b(tombstoneTTL|releaseSettle|ipamSweepInterval)\s*=\s*\d+\s*\*\s*time\.")

# How close the feature's own name must be to a loose duration for the
# sentence to count as a statement about this window.
NEAR = 300

def sentence_at(seg, pos):
    """The sentence of `seg` that contains offset `pos`."""
    at = 0
    for part in SENT.split(seg):
        nxt = seg.find(part, at)
        if nxt <= pos < nxt + len(part):
            return part
        at = nxt + len(part)
    return seg

# ----------------------------------------------------------------- scan

bad_band, bad_const, notes = [], [], []
blocks_seen = 0
bands_seen = 0
band_files = set()
unjudged = []

def pair(sent, pos, length, declared):
    """Which of the three this duration is a statement about, or None.

    The nearest subject marker wins. A tie between two different
    constants is no pairing at all: a sentence this gate cannot read
    unambiguously gets the weaker test rather than a guessed one. With
    no marker anywhere, a block that declares exactly one of the three
    says which it is about."""
    best, best_gap, tie = None, None, False
    for name, label, rx in SUBJECT_TABLE:
        for m in rx.finditer(sent):
            if m.start() <= pos < m.end() or pos <= m.start() < pos + length:
                gap = 0
            else:
                gap = min(abs(pos - m.end()), abs(m.start() - (pos + length)))
            if best_gap is None or gap < best_gap:
                best, best_gap, tie = (name, label), gap, False
            elif gap == best_gap and best is not None and best[0] != name:
                tie = True
    if best is not None and not tie and best_gap <= PAIR_REACH:
        return best
    if best is None and declared is not None:
        return declared
    return None

def where(lines, start, raw):
    """The line a statement begins on. A statement that wraps matches no
    single line, so its first token decides; without that, an exemption
    on the block's first line would silence every wrapped statement in
    it."""
    for off, line in enumerate(lines):
        if raw in line:
            return start + off
    head = raw.split()
    if head:
        for off, line in enumerate(lines):
            if head[0] in line:
                return start + off
    return start

for path in candidate_files():
    if path in SELF:
        continue
    rel = os.path.relpath(path, root)
    try:
        lines = open(path, encoding="utf-8", errors="replace").read().splitlines()
    except OSError:
        continue
    whole_file = bool(FEATURE_FILE.search(path))
    for start, blk in blocks(path, lines):
        segs = segments(path, blk)
        text = " ".join(t for _, t in segs)
        if not (whole_file or FEATURE.search(text)):
            continue
        blocks_seen += 1
        # The marker exempts the line it is written on, which is the
        # line a statement STARTS on, and nothing else: not the
        # paragraph, and not the next line. Either wider reach lets one
        # marker silence a statement it was not written for, which is
        # the gate being emptied a marker at a time. A statement that
        # wraps is exempted from its own first line, because that is
        # where this gate reports it.
        exempt = {start + off for off, l in enumerate(blk) if EXEMPT.search(l)}

        # A band, in a sentence that says it is this window's band. A
        # range of seconds in a sentence that does NOT say so is not
        # judged, because judging it is how the gate goes red on a
        # sentence about how long a runner takes to start a container.
        # It is printed instead, so the set this gate declines to judge
        # is visible in its own output and not only in its header.
        for sent in [x for _, seg in segs for x in SENT.split(seg)]:
            if not BAND_SUBJECT.search(sent):
                for m in BAND.finditer(sent):
                    line = where(blk, start, m.group(0))
                    if line in exempt:
                        continue
                    note = (rel, line, m.group(0), sent,
                            "a range of seconds that does not say it is this window")
                    if note not in unjudged:
                        unjudged.append(note)
                continue
            for m in BAND.finditer(sent):
                line = where(blk, start, m.group(0))
                if line in exempt:
                    continue
                bands_seen += 1
                band_files.add(rel)
                a, b = m.group(1).lower(), m.group(2).lower()
                a = float(a) if a[0].isdigit() else WORDS.get(a, -1)
                b = float(b) if b[0].isdigit() else WORDS.get(b, -1)
                if (a, b) != (floor, ceiling):
                    bad_band.append((rel, line, m.group(0),
                                     f"the derived band is {floor:g} to {ceiling:g} seconds "
                                     f"({DERIVED_TEXT})"))

        # Every loose statement about this window, subject or no subject:
        # the set this gate cannot check is printed rather than excluded
        # in silence. Prose only -- a `context.WithTimeout(5*time.Minute)`
        # is a test's budget and not a sentence about the window.
        for is_prose, seg in segs:
            if not is_prose:
                continue
            feature_at = [m.start() for m in FEATURE.finditer(seg)]
            for _, raw, loose, pos in durations(seg):
                if not loose:
                    continue
                # Near the feature's own name, not merely somewhere in a
                # block that mentions it once: a list of test files is not
                # a statement about this window.
                if not any(abs(f - pos) <= NEAR for f in feature_at):
                    continue
                note = (rel, where(blk, start, raw), raw, sentence_at(seg, pos))
                if note not in notes:
                    notes.append(note)

        dm = DECLARES.search(text)
        declared = None
        if dm:
            declared = next(((n, lbl) for n, lbl, _ in SUBJECT_TABLE
                             if n == dm.group(1)), None)

        for sent in [x for _, seg in segs for x in SENT.split(seg)]:
            if not (SUBJECTS.search(sent) or LEADING.match(sent)):
                continue
            spans = [(m.start(), m.end()) for m in BAND.finditer(sent)]
            for val, raw, loose, pos in durations(sent):
                if loose:
                    continue
                # A band is judged as a band, once, above.
                if any(a <= pos < b for a, b in spans):
                    continue
                line = where(blk, start, raw)
                if line in exempt:
                    continue
                subject = pair(sent, pos, len(raw), declared)
                if subject is not None:
                    name, label = subject
                    want = CONSTANTS[name]
                    if val != want:
                        bad_const.append((rel, line, raw, sent,
                                          f"states {label} as {val:g}s, and {name} "
                                          f"is {want:g}s"))
                elif val not in DERIVED:
                    bad_const.append((rel, line, raw, sent,
                                      f"names no subject this gate can pair it with, "
                                      f"and no constant of this window is {val:g}s "
                                      f"({DERIVED_TEXT})"))

# ---------------------------------------------------------------- verdict

if blocks_seen == 0:
    refuse("Empty domain",
           "no block under this root names `release_lease`, `on_remove` or the "
           "deferred-release code, so this gate checked nothing and would pass "
           "by having an empty domain.")
missing_docs = [d for d in REQUIRED_BAND_DOCS if d not in band_files]
absent_docs = [d for d in REQUIRED_BAND_DOCS if not os.path.isfile(os.path.join(root, d))]
if absent_docs:
    refuse("A document that must state the band is not there",
           ", ".join(absent_docs) + " does not exist under this root. The band's "
           "readership is not something this gate can rediscover from the tree, so "
           "it refuses rather than checking a smaller set than it was built for.")
if bands_seen == 0:
    refuse("No band is stated anywhere",
           f"{blocks_seen} block(s) name this feature and not one of them states the "
           "window as `<a> to <b> seconds`. The sentences this gate exists for are "
           "gone, moved out of its domain, or spelled a way it cannot read; it "
           "refuses rather than reporting that they all agree.")

def quote(s, n=150):
    s = " ".join(s.split())
    return s if len(s) <= n else s[:n - 1] + "…"

rc = 0
if missing_docs:
    for d in missing_docs:
        print(f"FAIL  {d}: states no band for this window at all. It is one of the "
              f"three documents that must carry it, and the derived band is "
              f"{floor:g} to {ceiling:g} seconds.")
    rc = 1
for rel, line, raw, why in bad_band:
    print(f"FAIL  {rel}:{line}: states the window as \"{raw}\"; {why}")
    rc = 1
for rel, line, raw, sent, why in bad_const:
    print(f"FAIL  {rel}:{line}: \"{quote(sent)}\"")
    print(f"      spells \"{raw}\" and {why}")
    rc = 1

for rel, line, raw, sent in notes:
    print(f"NOTE  {rel}:{line}: \"{quote(sent)}\" states \"{raw}\" loosely; "
          f"not checked against {floor:g}-{ceiling:g}s")
for rel, line, raw, sent, why in unjudged:
    print(f"NOTE  {rel}:{line}: \"{quote(sent)}\" states \"{raw}\", {why}; "
          f"not checked against {floor:g} to {ceiling:g} seconds")

if rc:
    print()
    print("The window is three constants added up, and these sentences no longer")
    print("add up to it. Correct each sentence, or the constant that moved:")
    print(f"  tombstoneTTL       {tombstone:g}s   (pkg/plugin/state.go)")
    print(f"  releaseSettle      {settle:g}s   (pkg/plugin/deferred_release.go)")
    print(f"  ipamSweepInterval  {tick:g}s   (pkg/plugin/ipam_reserve.go)")
    print(f"  the window is therefore {floor:g} to {ceiling:g} seconds")
    print("A sentence that states a duration beside one of these subjects for some")
    print("other reason says `window-exempt: <reason>` ON THE LINE NAMED ABOVE.")
    print("The marker exempts that one line, so a marker anywhere else leaves the")
    print("statement red.")
    sys.exit(1)

print(f"window-constants gate: {bands_seen} band statement(s) and every constant "
      f"sentence in {blocks_seen} block(s) agree with the derivation "
      f"({floor:g} to {ceiling:g} seconds; {DERIVED_TEXT})")
PY
