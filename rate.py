#!/usr/bin/env python3
"""Rates the items in unrated.json with headless Claude Code and merges the results into ratings.json."""
import json, os, subprocess, sys

todo = json.load(open("unrated.json"))
if not todo:
    sys.exit(0)
if os.path.exists("new-ratings.json"):
    os.remove("new-ratings.json")
subprocess.run([
    "claude", "-p", open("rate-prompt.md").read(),
    "--model", "claude-opus-5-5",
    # Claude may read files and GitHub items and write its answer file, nothing else.
    "--allowedTools", "Read", "Edit(new-ratings.json)", "Bash(gh issue view:*)", "Bash(gh pr view:*)",
], check=True, stdin=subprocess.DEVNULL)

specs = {s["id"] for s in json.load(open("specs.json"))}
ratings = json.load(open("ratings.json"))
wanted = {str(i["n"]) for i in todo}
for n, v in json.load(open("new-ratings.json")).items():
    # Only well-formed answers for the items asked about get in.
    if n not in wanted or v.get("rating") not in ("blocker", "high", "medium", "low", "none") \
            or v.get("basis") not in ("legal", "profile", "usable", "security", "hygiene") or not isinstance(v.get("note"), str):
        print(f"skipped {n}: {v}", file=sys.stderr)
        continue
    ratings[n] = {"rating": v["rating"], "basis": v["basis"], "specs": [s for s in v.get("specs", []) if s in specs][:1], "note": v["note"], "why": v.get("why", "") if isinstance(v.get("why"), str) else ""}
    print(f"rated #{n} {v['rating']} {v['basis']}: {v['note']}")
json.dump(ratings, open("ratings.json", "w"), indent=1)
