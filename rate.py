#!/usr/bin/env python3
"""Rates the items in unrated.json with headless Claude Code and re-checks a batch of earlier ratings.

Both merge into ratings.json. A rating that Dominik set by hand ("manual": true) is never changed here.
"""
import hashlib, json, os, subprocess, sys, time

MODEL = "claude-opus-5-5"
# How many earlier ratings each run re-checks. Changed items come first, then ratings made under older rules.
RECHECK_MAX = 30
RATINGS = ("blocker", "high", "medium", "low", "none")
BASES = ("legal", "profile", "usable", "security", "hygiene")
NEW = ("Rate each item in {file} in the current directory. These are new open Keycloak issues and PRs in the "
       "OpenID4VC and EUDI scope. Rate how much each one matters for using Keycloak as an issuer or verifier in a "
       "production EUDI Wallet ecosystem. Judge each item on its own merit and ignore who wrote it.")
RECHECK = ("Each item in {file} in the current directory is an open Keycloak issue or PR with its current rating "
           "under \"current\". Check whether that rating still holds under the rules below and the item as it is "
           "now, including comments since it was rated. Rate how much each one matters for using Keycloak as an "
           "issuer or verifier in a production EUDI Wallet ecosystem. Judge each item on its own merit and ignore "
           "who wrote it. Keep a rating that holds, with its note unless the note is out of date.")


def rules():
    """The rating rules, and a hash of them and of the spec quotes. A rating made under other rules is re-checked."""
    text = open("rate-prompt.md").read()
    h = hashlib.sha256((text + open("specs.json").read()).encode()).hexdigest()[:16]
    return text, h


def ask(task, todo, path, out):
    """Has Claude rate the items in todo, written to path, and returns the well-formed answers from out."""
    json.dump(todo, open(path, "w"), indent=1)
    if os.path.exists(out):
        os.remove(out)
    text, _ = rules()
    prompt = task.format(file=path) + "\n\n" + text.replace("{out}", out).replace("{file}", path)
    subprocess.run([
        "claude", "-p", prompt, "--model", MODEL,
        # Claude may read files and GitHub items and write its answer file, nothing else.
        "--allowedTools", "Read", f"Edit({out})", "Bash(gh issue view:*)", "Bash(gh pr view:*)",
    ], check=True, stdin=subprocess.DEVNULL)
    wanted = {str(i["n"]) for i in todo}
    got = {}
    for n, v in json.load(open(out)).items():
        # Only well-formed answers for the items asked about get in.
        if n not in wanted or v.get("rating") not in RATINGS or v.get("basis") not in BASES or not isinstance(v.get("note"), str):
            print(f"skipped {n}: {v}", file=sys.stderr)
            continue
        got[n] = v
    return got


def rating(v, specs, h):
    return {"rating": v["rating"], "basis": v["basis"], "specs": [s for s in v.get("specs", []) if s in specs][:1], "note": v["note"],
            "why": v.get("why", "") if isinstance(v.get("why"), str) else "", "checked": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "rules": h}


def due(ratings, raw, h):
    """The open items whose rating is due for a re-check, the most urgent first.

    An item is due when it changed on GitHub since its rating was checked, when its rating was made under other
    rules, or when it was never checked. Items without a rating of their own, such as epics, aren't in ratings.json."""
    out = []
    for n, r in ratings.items():
        i = raw.get(int(n))
        if not i or i["state"] != "OPEN" or r.get("manual"):
            continue
        checked = r.get("checked", "")
        if checked and i["updatedAt"] > checked:
            out.append((0, checked, n))
        elif r.get("rules") != h:
            out.append((1, checked, n))
    return [n for _, _, n in sorted(out)][:RECHECK_MAX]


def main():
    specs = {s["id"] for s in json.load(open("specs.json"))}
    ratings = json.load(open("ratings.json"))
    _, h = rules()
    todo = json.load(open("unrated.json"))
    if todo:
        for n, v in ask(NEW, todo, "unrated.json", "new-ratings.json").items():
            ratings[n] = rating(v, specs, h)
            print(f"rated #{n} {v['rating']} {v['basis']}: {v['note']}")
        json.dump(ratings, open("ratings.json", "w"), indent=1)
    raw = {i["number"]: i for i in json.load(open("raw.json"))["items"]}
    recheck = due(ratings, raw, h)
    if not recheck:
        return
    items = [{"n": int(n), "kind": "issue" if raw[int(n)]["__typename"] == "Issue" else "PR", "title": raw[int(n)]["title"],
              "labels": [l["name"] for l in raw[int(n)]["labels"]["nodes"]], "body": (raw[int(n)]["body"] or "")[:1500],
              "current": {k: ratings[n].get(k, "") for k in ("rating", "basis", "specs", "note", "why")}} for n in recheck]
    got = ask(RECHECK, items, "recheck.json", "recheck-ratings.json")
    for n in recheck:
        if n not in got:
            continue
        old, new = ratings[n], rating(got[n], specs, h)
        if (old["rating"], old["basis"]) != (new["rating"], new["basis"]):
            print(f"re-rated #{n} {old['rating']} -> {new['rating']} {new['basis']}: {new['note']}")
        ratings[n] = new
    json.dump(ratings, open("ratings.json", "w"), indent=1)
    print(f"re-checked {len(got)} of {len(recheck)} ratings", file=sys.stderr)


if __name__ == "__main__":
    main()
