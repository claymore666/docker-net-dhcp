#!/usr/bin/env python3
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

"""Sync .bestpractices.json to the OpenSSF Best Practices badge entry.

The badge site (https://www.bestpractices.dev) is the source of truth for
the badge itself, but its answers are hand-entered through a web form, so
they drift from the repository silently. This script makes the repository
the source of truth: `.bestpractices.json` holds the answers, review
happens in a pull request, and this pushes the reviewed file to the
project entry.

Two modes:

  --diff  (default)  Read the project's public JSON and print how the
                     live answers differ from the file. Needs no
                     credentials and no session.
  --push             Apply the differing fields to the live entry.
                     Needs a logged-in browser session cookie.

Getting the cookie for --push: log in to https://www.bestpractices.dev
with GitHub, open the browser devtools (Chrome: More Tools -> Developer
Tools -> Application -> Cookies; Firefox: Web Developer -> Storage
Inspector -> Cookies), copy the value of the `_BadgeApp_session` cookie,
and export it:

    export BADGEAPP_SESSION='<value>'

The cookie is valid for 48 hours. It is read from the environment only —
never pass it on the command line, where it lands in shell history.

The push mechanics (fetch the edit form, scrape the Rails authenticity
and CSRF tokens, PATCH the HTML endpoint with project[field] parameters)
follow the upstream reference implementation, docs/best_practices_modify.py
in ossf/best-practices-badge; the JSON API endpoint does not accept
writes.

Usage:
    scripts/badge-sync.py                    # diff against the live entry
    scripts/badge-sync.py --push             # apply the differences
    scripts/badge-sync.py --project 13229    # override the project id
"""

import argparse
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request

PRODUCTION_BASE_URL = "https://www.bestpractices.dev/"
COOKIE_NAME = "_BadgeApp_session"
DEFAULT_PROJECT_ID = 13229
DEFAULT_ANSWERS = ".bestpractices.json"

VALID_STATUSES = ("Met", "Unmet", "N/A", "?")

AUTH_TOKEN_PATTERN = re.compile(
    r'<input type="hidden" name="authenticity_token" value="([^"]+)"'
)
CSRF_TOKEN_PATTERN = re.compile(r'<meta name="csrf-token" content="([^"]+)"')


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


def get_tokens(base_url, project_id, session_cookie):
    """Fetch the edit form and return (auth_token, csrf_token, cookie)."""
    url = "%sen/projects/%d/edit" % (base_url, project_id)
    request = urllib.request.Request(url)
    request.add_header("Cookie", COOKIE_NAME + "=" + session_cookie)
    try:
        response = urllib.request.urlopen(request)
    except urllib.error.HTTPError as exc:
        die("could not open the edit page (HTTP %d) — is the session cookie "
            "current? It expires after 48 hours." % exc.code)
    if response.url != url:
        die("redirected away from the edit page — the session cookie is not "
            "logged in, or that account cannot edit project %d" % project_id)

    html = response.read().decode("utf-8")
    auth_match = AUTH_TOKEN_PATTERN.search(html)
    csrf_match = CSRF_TOKEN_PATTERN.search(html)
    if not auth_match or not csrf_match:
        die("could not find the authenticity/CSRF tokens on the edit page — "
            "the badge site's form markup may have changed")

    # The site rotates the session cookie on each response; use the new one.
    set_cookie = response.headers.get("Set-Cookie")
    if set_cookie and set_cookie.startswith(COOKIE_NAME + "="):
        session_cookie = set_cookie[len(COOKIE_NAME) + 1:].split(";", 1)[0]

    return auth_match.group(1), csrf_match.group(1), session_cookie


def push(base_url, project_id, updates, session_cookie):
    """PATCH the project with updates; return True on success."""
    auth_token, csrf_token, session_cookie = get_tokens(
        base_url, project_id, session_cookie
    )

    url = "%sen/projects/%d" % (base_url, project_id)
    form = {"project[" + key + "]": value for key, value in updates.items()}
    form["authentication_token"] = auth_token
    body = urllib.parse.urlencode(form).encode("utf-8")

    request = urllib.request.Request(url, data=body, method="PATCH")
    request.add_header("Cookie", COOKIE_NAME + "=" + session_cookie)
    request.add_header("X-CSRF-Token", csrf_token)

    try:
        urllib.request.urlopen(request)
    except urllib.error.HTTPError as exc:
        # A 302 back to the project page is the success path: the form
        # submission redirects. Anything else is a real failure.
        if exc.code == 302 and exc.headers.get("Location") == url:
            return True
        die("PATCH failed with HTTP %d — no changes were saved" % exc.code)
    # urlopen followed the redirect instead of raising; the update landed.
    return True


def main():
    parser = argparse.ArgumentParser(
        description="Sync .bestpractices.json to the OpenSSF Best Practices "
                    "badge entry.",
        epilog="--push reads the session cookie from $BADGEAPP_SESSION.",
    )
    parser.add_argument(
        "--diff",
        action="store_true",
        help="show how the live entry differs from the file (the default)",
    )
    parser.add_argument(
        "--push",
        action="store_true",
        help="apply the differences (default is to only show them)",
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
    diff = differences(answers, live)

    if not args.push:
        print_diff(diff, args.project)
        return

    if not diff:
        print("nothing to push: project %d already matches %s"
              % (args.project, args.file))
        return

    session_cookie = os.environ.get("BADGEAPP_SESSION", "").strip()
    if not session_cookie:
        die("BADGEAPP_SESSION is not set — see the header of this script for "
            "how to obtain the session cookie")

    print_diff(diff, args.project)
    updates = {key: wanted for key, (_, wanted) in diff.items()}
    print("\npushing %d field(s) to project %d ..." % (len(updates), args.project))
    push(args.base_url, args.project, updates, session_cookie)

    # Confirm against the site rather than trusting the response: read the
    # public JSON back and report anything that did not take.
    remaining = differences(answers, fetch_live(args.base_url, args.project))
    if remaining:
        print("\nwarning: %d field(s) still differ after the push:"
              % len(remaining))
        for key in sorted(remaining):
            print("  %s" % key)
        sys.exit(1)
    print("done — project %d now matches %s" % (args.project, args.file))


if __name__ == "__main__":
    main()
