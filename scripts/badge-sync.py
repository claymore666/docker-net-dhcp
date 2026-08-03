#!/usr/bin/env python3
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

"""Check .bestpractices.json against the OpenSSF Best Practices entry.

The badge site (https://www.bestpractices.dev) renders the badge from its
own copy of the answers, hand-entered through a web form, so that copy
drifts from the repository silently. This script makes the repository the
source of truth: `.bestpractices.json` holds the answers, review happens
in a pull request, and this reports whenever the live entry has fallen
behind.

Two modes, both read-only:

  --diff  (default)  Read the project's public JSON and print how the
                     live answers differ from the file. Needs no
                     credentials and no session.
  --check            Validate the answers file and exit. No network.

There is deliberately no push mode. See "Applying a change" below.

Applying a change
-----------------

Edit `.bestpractices.json`, land it in a PR, then enter the changed
fields once through the badge site's own form in a browser you are
logged into. The form is **per criteria level** — a field only appears
on the level that owns it, and there is no combined page:

    https://www.bestpractices.dev/en/projects/<id>/passing/edit
    https://www.bestpractices.dev/en/projects/<id>/silver/edit
    https://www.bestpractices.dev/en/projects/<id>/gold/edit

(`/en/projects/<id>/edit` — no level — does not exist and 404s for
everyone, including the project's owner.)

**Verify the text you paste, do not trust that you pasted it right.**
Typing an answer by hand is how this project produced a 65-field
divergence from its own source of truth. Before submitting, hash each
justification in the page and compare against the file:

    # in the browser console, per field
    await crypto.subtle.digest('SHA-256',
        new TextEncoder().encode(
            document.querySelector('textarea[name="project[<key>_justification]"]').value))
      .then(h => [...new Uint8Array(h)].map(x => x.toString(16).padStart(2,'0')).join('').slice(0,16))

    # locally, the value it must equal
    python3 -c "import json,hashlib; d=json.load(open('.bestpractices.json')); \\
        print(hashlib.sha256(d['<key>_justification'].encode()).hexdigest()[:16])"

Then run `--diff` afterwards: it must report that the entry matches.
That check is the whole point of this script — the form is where
answers are entered, and this is how we find out when it has gone
stale.

Why there is no --push
----------------------

There was one. It never worked: it requested an edit URL that 404s,
omitted the form's `project[lock_version]`, and posted the CSRF
parameter under the wrong name. It stayed broken across releases while
reading like working automation, because no gate can exercise it — a
write path against a third-party Rails app needs a live human session
cookie, so CI cannot test it and the next form change breaks it
silently again. A verifier that runs on every check is worth more than
an un-runnable writer.

Usage:
    scripts/badge-sync.py                    # diff against the live entry
    scripts/badge-sync.py --check            # validate the file, no network
    scripts/badge-sync.py --project 13229    # override the project id
"""

import argparse
import json
import sys
import urllib.error
import urllib.request

PRODUCTION_BASE_URL = "https://www.bestpractices.dev/"
DEFAULT_PROJECT_ID = 13229
DEFAULT_ANSWERS = ".bestpractices.json"

VALID_STATUSES = ("Met", "Unmet", "N/A", "?")


def die(message):
    print("error: " + message, file=sys.stderr)
    sys.exit(1)


def load_answers(path):
    """Load and validate the answers file."""
    try:
        with open(path, encoding="utf-8") as handle:
            answers = json.load(handle)
    except FileNotFoundError:
        die("answers file not found: " + path)
    except json.JSONDecodeError as exc:
        die("answers file is not valid JSON: " + str(exc))

    if not isinstance(answers, dict):
        die("answers file must be a JSON object")

    problems = []
    for key, value in answers.items():
        if not isinstance(value, str):
            problems.append("%s: value must be a string" % key)
            continue
        if key.endswith("_status"):
            if value not in VALID_STATUSES:
                problems.append(
                    "%s: %r is not one of %s"
                    % (key, value, ", ".join(VALID_STATUSES))
                )
            criterion = key[: -len("_status")]
            if value != "?" and not answers.get(criterion + "_justification"):
                problems.append(
                    "%s is %s but %s_justification is missing or empty"
                    % (key, value, criterion)
                )
        elif key.endswith("_justification"):
            criterion = key[: -len("_justification")]
            if criterion + "_status" not in answers:
                problems.append(
                    "%s has no matching %s_status" % (key, criterion)
                )
        else:
            problems.append(
                "%s: expected a field ending in _status or _justification" % key
            )

    if problems:
        for problem in problems:
            print("error: " + problem, file=sys.stderr)
        sys.exit(1)

    return answers


def fetch_live(base_url, project_id):
    """Return the project's current answers from the public JSON endpoint."""
    url = "%sprojects/%d.json" % (base_url, project_id)
    try:
        with urllib.request.urlopen(url) as response:
            return json.loads(response.read().decode("utf-8"))
    except urllib.error.URLError as exc:
        die("could not read %s: %s" % (url, exc))


def differences(answers, live):
    """Return {field: (live_value, wanted_value)} for fields that differ.

    A `?` status in the answers file means "no answer here"; it is skipped
    rather than used to blank out a live answer, matching how the badge
    site treats `?` in .bestpractices.json.
    """
    diff = {}
    for key, wanted in answers.items():
        criterion = key.rsplit("_", 1)[0]
        if answers.get(criterion + "_status") == "?":
            continue
        current = live.get(key)
        if current is None:
            current = ""
        if (current or "").strip() != wanted.strip():
            diff[key] = (current, wanted)
    return diff


def print_diff(diff, project_id):
    if not diff:
        print("project %d already matches %s" % (project_id, DEFAULT_ANSWERS))
        return
    statuses = sorted(k for k in diff if k.endswith("_status"))
    print(
        "%d field(s) differ from project %d (%d status, %d justification)\n"
        % (len(diff), project_id, len(statuses), len(diff) - len(statuses))
    )
    for key in statuses:
        current, wanted = diff[key]
        print("  %-42s %s -> %s" % (key, current or "(unset)", wanted))
    extra = sorted(k for k in diff if not k.endswith("_status"))
    if extra:
        print("\n  justification text updated for:")
        for key in extra:
            print("    %s" % key[: -len("_justification")])


def main():
    parser = argparse.ArgumentParser(
        description="Check .bestpractices.json against the OpenSSF Best "
                    "Practices badge entry.",
        epilog="Read-only. Changes are entered through the badge site's own "
               "form — see this script's header for the procedure, including "
               "the hash check that keeps a hand-entered answer honest.",
    )
    parser.add_argument(
        "--diff",
        action="store_true",
        help="show how the live entry differs from the file (the default)",
    )
    parser.add_argument(
        "--project",
        type=int,
        default=DEFAULT_PROJECT_ID,
        metavar="ID",
        help="badge project id (default: %(default)s)",
    )
    parser.add_argument(
        "--file",
        default=DEFAULT_ANSWERS,
        metavar="PATH",
        help="answers file (default: %(default)s)",
    )
    parser.add_argument(
        "--base-url",
        default=PRODUCTION_BASE_URL,
        metavar="URL",
        help="badge site base URL, ending in a slash "
             "(default: %(default)s; use https://staging.bestpractices.dev/ "
             "to rehearse)",
    )
    parser.add_argument(
        "--check",
        action="store_true",
        help="validate the answers file and exit; no network access",
    )
    args = parser.parse_args()

    answers = load_answers(args.file)
    if args.check:
        criteria = sum(1 for k in answers if k.endswith("_status"))
        print("%s is valid: %d criteria answered" % (args.file, criteria))
        return

    if not args.base_url.endswith("/"):
        die("--base-url must end in a slash")

    live = fetch_live(args.base_url, args.project)
    print_diff(differences(answers, live), args.project)


if __name__ == "__main__":
    main()
