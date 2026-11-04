#!/usr/bin/env python3
"""Reviews open PRs with headless Claude Code and stores the results in reviews.json.

    python3 review.py            # the most relevant unreviewed PRs, at most MAX per run
    python3 review.py 54522 ...  # these PRs, even if their head was reviewed already
"""
import json, os, re, shutil, subprocess, sys, threading, time
from concurrent.futures import ThreadPoolExecutor, as_completed

MAX = 5
MODEL = "claude-opus-5-5"
EFFORT = "high"
PARALLEL = 3
CACHE = os.path.abspath("cache/keycloak.git")
WORK = os.path.abspath("work")
RANK = {"none": 0, "low": 1, "medium": 2, "high": 3, "blocker": 4}


# Parallel git commands on one repository collide on its lock files.
git_lock = threading.Lock()


def git(*args, **kw):
    with git_lock:
        return subprocess.run(["git", "-C", CACHE, *args], check=True, capture_output=True, text=True, **kw).stdout


def gh(*args):
    return json.loads(subprocess.run(["gh", *args], check=True, capture_output=True, text=True).stdout)


def info(n):
    pr = gh("pr", "view", str(n), "-R", "keycloak/keycloak", "--json", "title,author,body,comments,reviews")
    inline = gh("api", f"repos/keycloak/keycloak/pulls/{n}/comments", "--paginate")
    out = [f"# #{n} {pr['title']}", f"Author: {pr['author']['login']}", "", pr["body"] or "", "", "## Comments"]
    out += [f"- {c['author']['login']}: {c['body']}" for c in pr["comments"]]
    out += [f"- {r['author']['login']} ({r['state'].lower()}): {r['body']}" for r in pr["reviews"] if r["body"]]
    out += [f"- {c['user']['login']} on {c['path']}:{c.get('line') or c.get('original_line')}: {c['body']}" for c in inline]
    return "\n".join(out)


def diff_lines(patch, path):
    """The diff of one file as [kind, old line, new line, text] rows."""
    rows, old, new, inside = [], 0, 0, False
    for l in patch.splitlines():
        if l.startswith("diff --git "):
            inside = l.endswith(f" b/{path}")
        elif inside and l.startswith("@@"):
            m = re.match(r"@@ -(\d+)(?:,\d+)? \+(\d+)", l)
            old, new = int(m.group(1)), int(m.group(2))
            rows.append(["@", None, None, l])
        elif inside and rows and l[:1] in (" ", "+", "-"):
            rows.append([l[0], old if l[0] != "+" else None, new if l[0] != "-" else None, l[1:]])
            old += l[0] != "+"
            new += l[0] != "-"
    return rows


def context(tree, patch, f):
    """The lines up to a finding, from the diff if it changed them, else from the file."""
    if not f.get("file") or not isinstance(f.get("line"), int):
        return []
    rows = diff_lines(patch, f["file"])
    hit = [k for k, r in enumerate(rows) if r[2] == f["line"]]
    if hit:
        k = hit[0]
        start = max(k - 5, max(j for j in range(k + 1) if rows[j][0] == "@"))
        return rows[start:k + 1]
    path = os.path.realpath(os.path.join(tree, f["file"]))
    if not path.startswith(tree + os.sep) or not os.path.isfile(path):
        return []
    lines = open(path, errors="replace").read().splitlines()
    return [[" ", None, i + 1, lines[i]] for i in range(max(0, f["line"] - 6), min(f["line"], len(lines)))]


def discussed(pr):
    """When a person last commented on or reviewed the PR. Bots don't count."""
    dates = [c["createdAt"] for c in pr.get("lastComments", {}).get("nodes", []) if (c["author"] or {}).get("__typename") == "User"]
    dates += [r["submittedAt"] or "" for r in pr.get("lastReviews", {}).get("nodes", []) if (r["author"] or {}).get("__typename") == "User"]
    return max(dates, default="")


def review(n, sha, base, notes):
    git("fetch", "-q", "origin", f"+refs/heads/{base}:refs/heads/{base}", f"+refs/pull/{n}/head:refs/pr/{n}")
    if git("rev-parse", f"refs/pr/{n}").strip() != sha:
        raise RuntimeError("the head moved since fetch.py ran")
    tree = os.path.join(WORK, str(n))
    shutil.rmtree(tree, ignore_errors=True)
    git("worktree", "prune")
    git("worktree", "add", "-q", "--detach", tree, sha)
    try:
        # A PR may add Claude Code settings or MCP servers, which must not apply to its own review.
        for p in (".claude", ".mcp.json"):
            path = os.path.join(tree, p)
            shutil.rmtree(path) if os.path.isdir(path) else os.path.exists(path) and os.remove(path)
        os.makedirs(os.path.join(tree, ".pr"))
        mb = git("merge-base", base, sha).strip()
        patch = git("diff", mb, sha)
        with open(os.path.join(tree, ".pr/diff.patch"), "w") as f:
            f.write(patch)
        with open(os.path.join(tree, ".pr/files.txt"), "w") as f:
            f.write(subprocess.run(["git", "-C", tree, "ls-files"], check=True, capture_output=True, text=True).stdout)
        with open(os.path.join(tree, ".pr/info.md"), "w") as f:
            f.write(info(n))
        if notes:
            with open(os.path.join(tree, ".pr/notes.md"), "w") as f:
                f.write("\n".join(f"- {x}" for x in notes))
        with open(os.path.join(tree, ".pr/review.json"), "w") as f:
            f.write("{}")
        prompt = open("review-prompt.md").read().replace("{n}", str(n)).replace("{base}", base)
        # The checkout is untrusted. Claude may only read inside it and write its answer file.
        # User settings only, so the checkout's settings, hooks and MCP servers don't load.
        out = subprocess.run([
            "claude", "-p", prompt, "--model", MODEL, "--effort", EFFORT, "--output-format", "json",
            "--setting-sources", "user", "--strict-mcp-config",
            "--allowedTools", "Read(./**)", "Edit(.pr/review.json)",
        ], check=True, cwd=tree, stdin=subprocess.DEVNULL, timeout=3600, capture_output=True, text=True).stdout
        run = json.loads(out)
        u = run.get("usage", {})
        with git_lock, open("review-usage.log", "a") as f:
            f.write(json.dumps({"n": n, "date": time.strftime("%Y-%m-%dT%H:%M"), "cost_usd": run.get("total_cost_usd"), "turns": run.get("num_turns"),
                                "input": u.get("input_tokens"), "cache_read": u.get("cache_read_input_tokens"),
                                "cache_write": u.get("cache_creation_input_tokens"), "output": u.get("output_tokens")}) + "\n")
        r = json.load(open(os.path.join(tree, ".pr/review.json")))
        if not r:
            raise RuntimeError(f"no answer file, Claude said: {str(run.get('result'))[:500]}")
        for f in r.get("findings", []):
            if isinstance(f, dict):
                f["context"] = context(os.path.realpath(tree), patch, f)
        return r
    finally:
        git("worktree", "remove", "--force", tree)
        git("update-ref", "-d", f"refs/pr/{n}")


def clean(r):
    s = lambda v, k=2000: v[:k] if isinstance(v, str) else ""
    if r.get("verdict") not in ("approve", "comment", "changes") or not s(r.get("summary")):
        raise ValueError(f"malformed review: {str(r)[:300]}")
    findings = [{"severity": f["severity"], "file": s(f.get("file"), 300), "line": f["line"] if isinstance(f.get("line"), int) else None,
                 "title": s(f.get("title"), 300), "impact": s(f.get("impact"), 500), "detail": s(f.get("detail")), "fix": s(f.get("fix")),
                 "context": [[k, o, w, t[:300]] for k, o, w, t in f.get("context", [])]}
                for f in r.get("findings", []) if isinstance(f, dict) and f.get("severity") in ("major", "minor", "followup", "nit") and s(f.get("title"))]
    return {"verdict": r["verdict"], "headline": s(r.get("headline"), 200), "summary": s(r["summary"]), "eudi": s(r.get("eudi")),
            "good": [s(g, 300) for g in r.get("good", []) if s(g)][:3], "findings": findings[:30]}


def main():
    raw = {i["number"]: i for i in json.load(open("raw.json"))["items"]}
    reviews = json.load(open("reviews.json")) if os.path.exists("reviews.json") else {}
    # Design decisions the PR author confirmed.
    notes = json.load(open("review-notes.json"))
    open_prs = {n: i for n, i in raw.items() if i["__typename"] == "PullRequest" and i["state"] == "OPEN"}
    if sys.argv[1:]:
        todo = [int(a) for a in sys.argv[1:]]
    else:
        # A PR inherits the relevance of the issues it closes.
        items = {it["n"]: it for it in json.load(open("site/items.json"))["items"]}
        rank = lambda n: max([RANK[items[n]["rating"]] if n in items else 0] + [RANK[items[l]["rating"]] for l in items.get(n, {}).get("links", []) if l in items])
        # A new commit, a changed note or a new comment by a person means a new review.
        done = lambda n: (reviews.get(str(n), {}).get("sha"), reviews.get(str(n), {}).get("notes", []), reviews.get(str(n), {}).get("discussed", "")) == (
            open_prs[n]["headRefOid"], notes.get(str(n), []), discussed(open_prs[n]))
        todo = [n for n in open_prs if not done(n)]
        todo = sorted(todo, key=lambda n: (open_prs[n]["isDraft"], -rank(n), -n))[:MAX]
    start = time.time()
    with ThreadPoolExecutor(PARALLEL) as pool:
        jobs = {pool.submit(review, n, open_prs[n]["headRefOid"], open_prs[n]["baseRefName"], notes.get(str(n), [])): n for n in todo}
        for job in as_completed(jobs):
            n = jobs[job]
            try:
                r = clean(job.result())
            except Exception as e:
                print(f"#{n} failed: {e}", file=sys.stderr, flush=True)
                continue
            reviews[str(n)] = {"sha": open_prs[n]["headRefOid"], "notes": notes.get(str(n), []), "discussed": discussed(open_prs[n]), "date": time.strftime("%Y-%m-%d"),
                          "model": MODEL, "effort": EFFORT, **r}
            json.dump(reviews, open("reviews.json", "w"), indent=1)
            print(f"#{n} {r['verdict']} with {len(r['findings'])} findings after {int(time.time() - start)} s", flush=True)


main()
