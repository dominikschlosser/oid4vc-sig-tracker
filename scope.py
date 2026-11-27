#!/usr/bin/env python3
"""Finds new Keycloak issues and PRs that fit the EUDI scope but that the keyword search in fetch.py misses.

It lists what was created on keycloak/keycloak since the last run, leaves out what fetch.py finds anyway, and
lets headless Claude Code pick the items that matter for Keycloak as an EUDI issuer or verifier. Their numbers
go into scope.json, which fetch.py reads, so they get rated and shown like the rest.
"""
import json, os, subprocess, sys, time

import fetch

MODEL = "claude-opus-5-5"
# The first run looks back this many days. Later runs start where the last one stopped.
FIRST_DAYS = 14
# Bots open many PRs that never fit, such as dependency updates.
BOTS = "-author:app/dependabot -author:app/github-actions -author:app/renovate"


def created_since(day):
    """Issues and PRs created on or after day, at most 1000 of each kind, as the search API allows."""
    found = {}
    for kind in ("is:issue", "is:pr"):
        for page in range(1, 11):
            r = fetch.gh("api", "-X", "GET", "search/issues", "-f", f"q=repo:{fetch.REPO} {kind} created:>={day} {BOTS}",
                         "-f", "per_page=100", "-f", f"page={page}")
            found |= {i["number"]: i for i in r["items"]}
            if len(r["items"]) < 100:
                break
    return found


def main():
    scope = json.load(open("scope.json")) if os.path.exists("scope.json") else {"checked": "", "in": []}
    # The search goes by day, so the day of the last run is searched again. Items already decided are left out.
    day = scope["checked"][:10] or time.strftime("%Y-%m-%d", time.gmtime(time.time() - FIRST_DAYS * 86400))
    started = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    known = {i["number"] for i in json.load(open("raw.json"))["items"]} if os.path.exists("raw.json") else set()
    known |= set(scope["in"]) | set(scope.get("out", []))
    new = [i for n, i in sorted(created_since(day).items()) if n not in known
           and not fetch.TITLE_RE.search(i["title"]) and not fetch.BODY_RE.search(i["body"] or "")]
    print(f"{len(new)} new items outside the keyword search since {day}", file=sys.stderr)
    if new:
        json.dump([{"n": i["number"], "kind": "PR" if i.get("pull_request") else "issue", "title": i["title"],
                    "labels": [l["name"] for l in i["labels"]], "body": (i["body"] or "")[:800]} for i in new],
                  open("scope-input.json", "w"), indent=1)
        if os.path.exists("scope-answer.json"):
            os.remove("scope-answer.json")
        subprocess.run([
            "claude", "-p", open("scope-prompt.md").read(), "--model", MODEL,
            # Claude may read files and GitHub items and write its answer file, nothing else.
            "--allowedTools", "Read", "Edit(scope-answer.json)", "Bash(gh issue view:*)", "Bash(gh pr view:*)",
        ], check=True, stdin=subprocess.DEVNULL)
        asked = {i["number"] for i in new}
        picked = {n for n in json.load(open("scope-answer.json")).get("in", []) if n in asked}
        for i in new:
            if i["number"] in picked:
                print(f"in scope: #{i['number']} {i['title']}")
        scope["in"] = sorted(set(scope["in"]) | picked)
        scope["out"] = sorted(set(scope.get("out", [])) | (asked - picked))
    scope["checked"] = started
    json.dump(scope, open("scope.json", "w"), indent=1)


if __name__ == "__main__":
    main()
