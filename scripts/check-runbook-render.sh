#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only
#
# The release walkthrough must RENDER inside the procedure step that
# introduces it (#972 follow-up).
#
# docs/release-runbook.md is a numbered procedure whose list items are
# continued at THREE spaces, because `9. ` is three characters wide and
# CommonMark continues a list item at the marker width. MkDocs does not
# use CommonMark. It uses python-markdown, which continues a list item at
# `tab_length` (4) and nothing less, so a continuation block indented to
# three spaces silently CLOSES the <li> and the </ol> with it. Nothing
# goes red: `mkdocs build --strict` is happy, the page renders, and the
# walkthrough just quietly stops being part of step 9.
#
# That is what this gate exists to see, and it sees it the only way that
# cannot drift from the site: by rendering the file with the site's own
# extension set, read out of mkdocs.yml, and asking where the text landed.
#
# Exit codes
#   0  the walkthrough renders inside the step that introduces it
#   1  it does not — the message names the source line and its indent
#   2  CANNOT JUDGE: no renderer, no file, no anchor, or nothing to judge.
#      A gate that has lost its subject must refuse. Passing would report
#      "the walkthrough is inside step 9" about a page that no longer has
#      a walkthrough, which is the one answer it must never give.
#
# Runs in the `docs-site` job of .github/workflows/pages.yml, which is
# where python-markdown, pymdown-extensions and PyYAML are installed and
# hash-pinned (docs/requirements.txt). It is NOT runnable in the Test
# workflow's policy-gates job, which has no Python toolchain.
#
# Usage: bash scripts/check-runbook-render.sh [runbook] [mkdocs.yml]

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/.." && pwd)"

runbook="${1:-$root/docs/release-runbook.md}"
mkdocs="${2:-$root/mkdocs.yml}"

python3 - "$runbook" "$mkdocs" <<'PY'
import html.parser
import os
import re
import sys

REFUSE, FAIL, PASS = 2, 1, 0


def refuse(msg):
    print("check-runbook-render: CANNOT JUDGE: %s" % msg, file=sys.stderr)
    sys.exit(REFUSE)


runbook, mkdocs_yml = sys.argv[1], sys.argv[2]

try:
    import markdown
except ImportError as exc:
    refuse("python-markdown is not importable (%s). This gate belongs in the "
           "docs-site job, which installs docs/requirements.txt." % exc)
try:
    import yaml
except ImportError as exc:
    refuse("PyYAML is not importable (%s); mkdocs.yml cannot be read." % exc)

for path in (runbook, mkdocs_yml):
    if not os.path.isfile(path):
        refuse("%s does not exist" % path)

# ---------------------------------------------------------------- config
# The extension list is READ, never transcribed. A gate carrying its own
# copy of the site's extensions is a gate that renders a different page
# from the one readers get, and the drift is invisible from both sides.


class Tolerant(yaml.SafeLoader):
    pass


Tolerant.add_multi_constructor(
    "", lambda loader, suffix, node: "!" + suffix)
Tolerant.add_multi_constructor(
    "tag:yaml.org,2002:python/name:",
    lambda loader, suffix, node: suffix)

with open(mkdocs_yml, encoding="utf-8") as fh:
    try:
        cfg = yaml.load(fh, Loader=Tolerant) or {}
    except yaml.YAMLError as exc:
        refuse("mkdocs.yml is not parseable: %s" % exc)

raw = cfg.get("markdown_extensions")
if not raw:
    refuse("mkdocs.yml declares no markdown_extensions; there is no "
           "rendering to reproduce.")

exts, ext_cfg = [], {}
for item in raw:
    if isinstance(item, str):
        exts.append(item)
    elif isinstance(item, dict) and len(item) == 1:
        (name, conf), = item.items()
        exts.append(name)
        if isinstance(conf, dict):
            ext_cfg[name] = conf
    else:
        refuse("unrecognised markdown_extensions entry: %r" % (item,))

# python-markdown continues a list item at tab_length and nothing less.
# mkdocs.yml may override it; read it rather than assuming.
tab_length = int(cfg.get("tab_length", 4))

with open(runbook, encoding="utf-8") as fh:
    source = fh.read()

try:
    rendered = markdown.Markdown(
        extensions=exts, extension_configs=ext_cfg).convert(source)
except Exception as exc:  # a missing pymdownx extension lands here
    refuse("rendering failed with the site's extension set (%s): %s"
           % (", ".join(exts), exc))

# --------------------------------------------------------------- anchors
#
# STEP_ANCHOR identifies the list item under test. It is prose, and prose
# can be reworded -- so the gate REFUSES when it is gone rather than
# passing. Losing the anchor is a "come back and re-key me", not a green.
STEP_ANCHOR = "Expected steps, under the names the run shows"

# The content anchors are NOT prose. Every one of them is a literal
# `name:` of a step in .github/workflows/release.yml, and
# scripts/check-runbook-release-steps.sh already holds this page to those
# names in both directions -- present here and present there. So a
# rewording cannot quietly slip past this gate: renaming the workflow step
# is what it would take, and that turns check-runbook-release-steps.sh red
# in the same run. They are the hardest strings on the page to reword away.
RELEASE_JOB_ANCHORS = [
    "Sign published images (cosign keyless)",   # release.yml step name
    "Workflow summary",                         # last step of the release job
]
PROMOTE_JOB_ANCHORS = [
    "Promote the GHCR floating tags",           # promote-latest step name
    "Verify the floating tags resolve to the signed digests",
]

VOID = {"area", "base", "br", "col", "embed", "hr", "img", "input", "link",
        "meta", "param", "source", "track", "wbr"}


def squash(text):
    return re.sub(r"\s+", " ", text)


class Buckets(html.parser.HTMLParser):
    """Collect the text of the document, bucketed by the ordered-list item
    that encloses it. The bucket key is the identity of the <li> whose
    parent is an <ol>, so a nested <ul> bullet still reports the numbered
    step it lives in. Text outside every numbered step lands in bucket
    None, which is exactly the failure this gate is looking for."""

    def __init__(self):
        super().__init__(convert_charrefs=True)
        self.stack = []
        self.next_uid = 0
        self.text = {}

    def _bucket(self):
        for i in range(len(self.stack) - 1, -1, -1):
            tag, uid = self.stack[i]
            if tag == "li" and i and self.stack[i - 1][0] == "ol":
                return uid
        return None

    def handle_starttag(self, tag, attrs):
        if tag in VOID:
            return
        self.next_uid += 1
        self.stack.append((tag, self.next_uid))

    def handle_endtag(self, tag):
        for i in range(len(self.stack) - 1, -1, -1):
            if self.stack[i][0] == tag:
                del self.stack[i:]
                return

    def handle_data(self, data):
        self.text.setdefault(self._bucket(), []).append(data)


scan = Buckets()
scan.feed(rendered)
buckets = {k: squash("".join(v)) for k, v in scan.text.items()}
whole = squash(" ".join(buckets.values()))


def bucket_of(phrase):
    for uid, text in buckets.items():
        if squash(phrase) in text:
            return uid, True
    return None, False


step_uid, found = bucket_of(STEP_ANCHOR)
if not found:
    refuse("the anchor phrase %r is not on the page. Re-key this gate on "
           "the sentence that now introduces the release walkthrough."
           % STEP_ANCHOR)
if step_uid is None:
    print("check-runbook-render: FAIL: the step that introduces the release "
          "walkthrough does not itself render inside a numbered list item.",
          file=sys.stderr)
    sys.exit(FAIL)

# ------------------------------------------------- refuse an empty domain
present_release = [a for a in RELEASE_JOB_ANCHORS if squash(a) in whole]
present_promote = [a for a in PROMOTE_JOB_ANCHORS if squash(a) in whole]
if not present_release or not present_promote:
    missing = []
    if not present_release:
        missing.append("release job step order (none of %r)"
                       % (RELEASE_JOB_ANCHORS,))
    if not present_promote:
        missing.append("promote-latest step order (none of %r)"
                       % (PROMOTE_JOB_ANCHORS,))
    refuse("there is no walkthrough content to judge: %s. This gate will "
           "not pass by having its domain emptied." % "; ".join(missing))

# ------------------------------------------------------------- the check
strays = []
for anchor in present_release + present_promote:
    uid, _ = bucket_of(anchor)
    if uid != step_uid:
        strays.append((anchor, uid))

if not strays:
    print("check-runbook-render: OK: the release walkthrough (%d anchors) "
          "renders inside the numbered step that introduces it."
          % (len(present_release) + len(present_promote)))
    sys.exit(PASS)

# ----------------------------------------------- say WHICH line, and why
lines = source.split("\n")
pattern = re.compile(r"\s+".join(re.escape(w) for w in STEP_ANCHOR.split()))
match = pattern.search(source)
anchor_line = source.count("\n", 0, match.start()) + 1 if match else None

start = end = None
if anchor_line:
    for n in range(anchor_line, 0, -1):
        if re.match(r"^\d+\. ", lines[n - 1]):
            start = n
            break
    if start:
        end = len(lines)
        for n in range(start + 1, len(lines) + 1):
            if re.match(r"^\d+\. ", lines[n - 1]):
                end = n - 1
                break

# The offender list is bounded by the LAST stray anchor, and that bound
# is load-bearing. An under-indented block start that sits after every
# stray cannot be what closed the <li> before them, so naming it would
# be telling the author to change a line this gate is content with --
# and a diagnostic that asks for an edit it does not require is a
# diagnostic people learn to stop reading.
last_stray = 0
for anchor, _ in strays:
    pat = re.compile(r"\s+".join(re.escape(w) for w in anchor.split()))
    hit = pat.search(source)
    if hit:
        last_stray = max(last_stray, source.count("\n", 0, hit.start()) + 1)

offenders = []
if start:
    limit = min(end, last_stray) if last_stray else end
    after_blank = False
    for n in range(start + 1, limit + 1):
        line = lines[n - 1]
        if not line.strip():
            after_blank = True
            continue
        if after_blank:
            indent = len(line) - len(line.lstrip(" "))
            if indent < tab_length:
                offenders.append((n, indent, line))
        after_blank = False

print("check-runbook-render: FAIL: the release walkthrough renders OUTSIDE "
      "the numbered step that introduces it.", file=sys.stderr)
for anchor, uid in strays:
    where = "a different list item" if uid is not None else "top-level prose"
    print("  %r rendered in %s" % (anchor, where), file=sys.stderr)
if offenders:
    print("", file=sys.stderr)
    print("  python-markdown continues a list item at %d spaces, not at the "
          "width of the `N. ` marker. These lines start a block inside the "
          "step and are indented less than that, so each one closes the "
          "<li>:" % tab_length, file=sys.stderr)
    for n, indent, line in offenders:
        print("    %s:%d: indented %d, must be indented %d — %s"
              % (runbook, n, indent, tab_length, line.strip()[:60]),
              file=sys.stderr)
    print("", file=sys.stderr)
    print("  Continuation lines of those blocks need the same shift "
          "(3 -> %d, 5 -> %d for nested bullets)."
          % (tab_length, tab_length * 2), file=sys.stderr)
else:
    print("  No under-indented block start was found in the step's source, "
          "so the break is structural rather than an indent: inspect the "
          "rendered HTML.", file=sys.stderr)
sys.exit(FAIL)
PY
