#!/usr/bin/env python3
"""Reviews open PRs with headless Claude Code and stores the results in reviews.json.

Each review has two passes. REVIEW_MODEL writes a draft, and CHECK_MODEL checks the draft against the code and writes the final review.
Both passes answer the comments that people left on the PR.

    python3 review.py            # the most relevant PRs without a current, checked review, at most MAX per run
    python3 review.py 54522 ...  # these PRs, even if their head was reviewed already
"""
import json, os, re, shutil, subprocess, sys, threading, time
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass, field

# REVIEW_MAX raises it for one run, such as a full update after a change to the prompts.
MAX = int(os.environ.get("REVIEW_MAX", 5))
REVIEW_MODEL, REVIEW_EFFORT = "claude-opus-5-5", "high"
CHECK_MODEL, CHECK_EFFORT = "claude-fable-5-2", "high"
CHECK_TRIES = 3
PARALLEL = 3
CACHE = os.path.abspath("cache/keycloak.git")
WORK = os.path.abspath("work")
RANK = {"none": 0, "low": 1, "medium": 2, "high": 3, "blocker": 4}
STATUSES = ("open", "fixed", "disagree", "answered")
# The footer that the post service in api/main.go adds below each GitHub post.
TRACKER_POST = re.compile(r"<sub>(Posted from the \[full review\]\([^)]*/reviews/\d+\.html\) on the Keycloak OpenID4VC tracker\."
                          r"|Claude Code wrote this comment in its \[review on the Keycloak OpenID4VC tracker\]\([^)]*/reviews/\d+\.html#c\d+\)\.)</sub>")


# Parallel git commands on one repository collide on its lock files.
git_lock = threading.Lock()


def git(*args, **kw):
    with git_lock:
        return subprocess.run(["git", "-C", CACHE, *args], check=True, capture_output=True, text=True, **kw).stdout


def gh(*args):
    return json.loads(subprocess.run(["gh", *args], check=True, capture_output=True, text=True).stdout)


@dataclass
class Comment:
    author: str
    bot: bool
    date: str
    url: str
    body: str

    @property
    def tracker(self):
        """Whether someone posted it from a review on this tracker."""
        return bool(TRACKER_POST.search(self.body))


@dataclass
class Entry:
    """A comment, a review or a review thread. People's entries have a ref that the review answers to."""
    ref: str
    kind: str
    comments: list
    state: str = ""
    where: str = ""
    flags: list = field(default_factory=list)

    @property
    def human(self):
        return [c for c in self.comments if not c.bot]

    def raised_by(self, pr_author):
        """The comments in which a person raised something the review should answer.

        Posts from this tracker don't count. Nor do the PR author's replies in a thread that another person or a bot started, since they answer
        that point. Replies to the tracker's own comments do count, since they dispute an earlier review."""
        said = [c for c in self.human if c.body.strip() and not c.tracker]
        if self.kind == "thread" and not self.comments[0].tracker and self.comments[0].author != pr_author:
            said = [c for c in said if c.author != pr_author]
        return said


CONVERSATION = """query($n: Int!) { repository(owner: "keycloak", name: "keycloak") { pullRequest(number: $n) {
  title body author { login }
  comments(last: 100) { totalCount nodes { ...entry createdAt } }
  reviews(last: 100) { totalCount nodes { ...entry submittedAt state } }
  reviewThreads(last: 100) { totalCount nodes { isResolved isOutdated path line originalLine
    first: comments(first: 1) { nodes { ...entry createdAt } }
    last: comments(last: 49) { totalCount nodes { ...entry createdAt } } } } } } }
fragment entry on Comment { body author { login __typename }
  ... on IssueComment { databaseId url isMinimized }
  ... on PullRequestReview { databaseId url isMinimized }
  ... on PullRequestReviewComment { databaseId url isMinimized } }"""


def comment(node, date="createdAt"):
    # A deleted account has no author. It was a person.
    a = node["author"] or {"login": "ghost", "__typename": "User"}
    # Comments that GitHub hides as spam, off topic or outdated need no answer.
    body = "" if node["isMinimized"] else node["body"] or ""
    return Comment(a["login"], a["__typename"] == "Bot", (node[date] or "")[:10], node["url"], body)


def conversation(n):
    """The title, the description and every comment, review and review thread of the PR, oldest first."""
    pr = gh("api", "graphql", "-F", f"n={n}", "-f", f"query={CONVERSATION}")["data"]["repository"]["pullRequest"]
    entries = [(c.date, Entry(f"c{node['databaseId']}", "comment", [c])) for node in pr["comments"]["nodes"] for c in [comment(node)]]
    # A review without a body only holds thread comments, which come with their threads.
    entries += [(c.date, Entry(f"r{node['databaseId']}", "review", [c], state=node["state"].lower().replace("_", " ")))
                for node in pr["reviews"]["nodes"] if (node["body"] or "").strip() for c in [comment(node, "submittedAt")]]
    for t in pr["reviewThreads"]["nodes"]:
        # The comment that started the thread and its newest replies, since those settle the point.
        nodes = list({node["databaseId"]: node for node in t["first"]["nodes"] + t["last"]["nodes"]}.values())
        if not nodes:
            continue
        cs = [comment(node) for node in nodes]
        flags = ["resolved"] * t["isResolved"] + ["outdated"] * t["isOutdated"] + ["older replies left out"] * (t["last"]["totalCount"] > len(cs))
        line = t["line"] or t["originalLine"]
        entries.append((cs[0].date, Entry(f"t{nodes[0]['databaseId']}", "thread", cs, where=f"{t['path']}:{line}" if line else t["path"], flags=flags)))
    cut = [k for k in ("comments", "reviews", "reviewThreads") if pr[k]["totalCount"] > len(pr[k]["nodes"])]
    return pr, [e for _, e in sorted(entries, key=lambda x: x[0])], cut


def quote(body):
    return "\n".join(f"> {l}" for l in body.strip().splitlines()) or "> (empty)"


def who(c):
    return f"{c.author} (bot)" if c.bot else f"{c.author}, posting the tracker's earlier AI review," if c.tracker else c.author


def pr_author(pr):
    return (pr["author"] or {"login": "ghost"})["login"]


def info(n, pr, entries, cut, refs):
    out = [f"# #{n} {pr['title']}", f"Author: {pr_author(pr)}", "", "## Description", "", quote(pr["body"] or ""), "", "## Conversation", ""]
    names = {"comments": "comments", "reviews": "reviews", "reviewThreads": "review threads"}
    out += [f"Only the newest 100 {' and '.join(names[k] for k in cut)} are listed.", ""] * bool(cut)
    for e in entries:
        ref = f"[{e.ref}] " if e.ref in refs else ""
        if e.kind == "thread":
            flags = f" ({', '.join(e.flags)})" if e.flags else ""
            out.append(f"### {ref}Thread on {e.where}{flags}")
            for k, c in enumerate(e.comments):
                out += [f"{'Started by' if k == 0 else 'Reply by'} {who(c)} on {c.date}:", quote(c.body), ""]
        else:
            c = e.comments[0]
            verb = f"reviewed ({e.state})" if e.kind == "review" else "commented"
            out += [f"### {ref}{who(c)} {verb} on {c.date}", quote(c.body), ""]
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
    """When a person last commented on or reviewed the PR. Bots and posts from this tracker don't count."""
    person = lambda c: (c["author"] or {}).get("__typename") == "User" and not TRACKER_POST.search(c.get("body") or "")
    dates = [c["createdAt"] for c in pr.get("lastComments", {}).get("nodes", []) if person(c)]
    dates += [r["submittedAt"] or "" for r in pr.get("lastReviews", {}).get("nodes", []) if person(r)]
    return max(dates, default="")


def claude(n, tree, prompt, model, effort, step):
    """Runs one headless pass in the checkout and returns what it wrote into .pr/review.json."""
    with open(os.path.join(tree, ".pr/review.json"), "w") as f:
        f.write("{}")
    # The checkout is untrusted. Claude may only read inside it and write its answer file.
    # User settings only, so the checkout's settings, hooks and MCP servers don't load.
    out = subprocess.run([
        "claude", "-p", prompt, "--model", model, "--effort", effort, "--output-format", "json",
        "--setting-sources", "user", "--strict-mcp-config",
        "--allowedTools", "Read(./**)", "Edit(.pr/review.json)",
    ], check=True, cwd=tree, stdin=subprocess.DEVNULL, timeout=3600, capture_output=True, text=True).stdout
    run = json.loads(out)
    u = run.get("usage", {})
    with git_lock, open("review-usage.log", "a") as f:
        f.write(json.dumps({"n": n, "step": step, "model": model, "date": time.strftime("%Y-%m-%dT%H:%M"), "cost_usd": run.get("total_cost_usd"),
                            "turns": run.get("num_turns"), "input": u.get("input_tokens"), "cache_read": u.get("cache_read_input_tokens"),
                            "cache_write": u.get("cache_creation_input_tokens"), "output": u.get("output_tokens")}) + "\n")
    r = json.load(open(os.path.join(tree, ".pr/review.json")))
    if not r:
        raise RuntimeError(f"{step} wrote no answer file, Claude said: {str(run.get('result'))[:500]}")
    return r


def prompt(name, n, base):
    return open(name).read().replace("{rules}", open("review-rules.md").read()).replace("{n}", str(n)).replace("{base}", base)


def as_written(rv):
    """A stored review in the answer format, without what review.py added to it."""
    out = {k: rv[k] for k in ("verdict", "headline", "summary", "eudi", "good")}
    out["findings"] = [{k: v for k, v in f.items() if k != "context"} for f in rv["findings"]]
    out["discussion"] = [{k: d[k] for k in ("ref", "status", "point", "answer")} for d in rv.get("discussion", [])]
    return out


def review(n, sha, base, notes, draft=None):
    """Reviews the PR at sha and checks the review. With a draft, only checks the draft.

    Returns the final review and the model and effort of each pass. When the check fails, the draft is the final review and the check is None."""
    git("fetch", "-q", "origin", f"+refs/heads/{base}:refs/heads/{base}", f"+refs/pull/{n}/head:refs/pr/{n}")
    if git("rev-parse", f"refs/pr/{n}").strip() != sha:
        raise RuntimeError("the head moved since fetch.py ran")
    pr, entries, cut = conversation(n)
    author = pr_author(pr)
    refs = {e.ref: (e, e.raised_by(author)) for e in entries if e.raised_by(author)}
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
            f.write(info(n, pr, entries, cut, refs))
        if notes:
            with open(os.path.join(tree, ".pr/notes.md"), "w") as f:
                f.write("\n".join(f"- {x}" for x in notes))
        if draft is None:
            draft = clean(claude(n, tree, prompt("review-prompt.md", n, base), REVIEW_MODEL, REVIEW_EFFORT, "review"), refs)
            del draft["changes"]
            draft.update(model=REVIEW_MODEL, effort=REVIEW_EFFORT)
        with open(os.path.join(tree, ".pr/draft.json"), "w") as f:
            json.dump(as_written(draft), f, indent=1)
        try:
            final = clean(claude(n, tree, prompt("check-prompt.md", n, base), CHECK_MODEL, CHECK_EFFORT, "check"), refs)
            final.update(model=draft["model"], effort=draft["effort"], check={"model": CHECK_MODEL, "effort": CHECK_EFFORT, "changes": final.pop("changes")})
        except Exception as e:
            print(f"#{n} check failed, keeping the unchecked review: {e}", file=sys.stderr, flush=True)
            final = {**as_written(draft), "discussion": draft.get("discussion", []), "model": draft["model"], "effort": draft["effort"], "check": None,
                     "check_failures": draft.get("check_failures", 0) + 1}
        for f in final["findings"]:
            f["context"] = context(os.path.realpath(tree), patch, f)
        return final
    finally:
        git("worktree", "remove", "--force", tree)
        git("update-ref", "-d", f"refs/pr/{n}")


def clean(r, refs):
    s = lambda v, k=2000: v[:k] if isinstance(v, str) else ""
    items = lambda k: r[k] if isinstance(r.get(k), list) else []
    if r.get("verdict") not in ("approve", "comment", "changes") or not s(r.get("summary")):
        raise ValueError(f"malformed review: {str(r)[:300]}")
    findings = [{"severity": f["severity"], "file": s(f.get("file"), 300), "line": f["line"] if isinstance(f.get("line"), int) else None,
                 "title": s(f.get("title"), 300), "impact": s(f.get("impact"), 500), "detail": s(f.get("detail")), "fix": s(f.get("fix"))}
                for f in items("findings") if isinstance(f, dict) and f.get("severity") in ("major", "minor", "followup", "nit") and s(f.get("title"))]
    # Who wrote a comment and where it is come from GitHub, never from the answer.
    answers = {}
    for d in items("discussion"):
        if isinstance(d, dict) and isinstance(d.get("ref"), str) and d["ref"] in refs and d["ref"] not in answers and d.get("status") in STATUSES and s(d.get("answer")):
            e, said = refs[d["ref"]]
            answers[d["ref"]] = {"ref": d["ref"], "status": d["status"], "point": s(d.get("point"), 300), "answer": s(d.get("answer"), 1500),
                                 "kind": e.kind, "where": e.where, "authors": sorted({c.author for c in said}),
                                 "url": e.comments[0].url, "date": e.comments[0].date}
    return {"verdict": r["verdict"], "headline": s(r.get("headline"), 200), "summary": s(r["summary"]), "eudi": s(r.get("eudi")),
            "good": [s(g, 300) for g in items("good") if s(g)][:3], "findings": findings[:30],
            "discussion": sorted(answers.values(), key=lambda d: d["date"]), "changes": [s(c, 500) for c in items("changes") if s(c)][:20]}


def main():
    raw = {i["number"]: i for i in json.load(open("raw.json"))["items"]}
    reviews = json.load(open("reviews.json")) if os.path.exists("reviews.json") else {}
    # Design decisions the PR author confirmed.
    notes = json.load(open("review-notes.json"))
    open_prs = {n: i for n, i in raw.items() if i["__typename"] == "PullRequest" and i["state"] == "OPEN"}
    # A new commit, a changed note or a new comment by a person means a new review.
    current = lambda n: (reviews.get(str(n), {}).get("sha"), reviews.get(str(n), {}).get("notes", []), reviews.get(str(n), {}).get("discussed", "")) == (
        open_prs[n]["headRefOid"], notes.get(str(n), []), discussed(open_prs[n]))
    # A current review that CHECK_MODEL has not checked yet only needs the check. After CHECK_TRIES failed checks it stays unchecked.
    checked = lambda n: (reviews.get(str(n), {}).get("check") or {}).get("model") == CHECK_MODEL or reviews.get(str(n), {}).get("check_failures", 0) >= CHECK_TRIES
    if sys.argv[1:]:
        todo = [int(a) for a in sys.argv[1:]]
        drafts = {}
    else:
        # A PR inherits the relevance of the issues it closes.
        items = {it["n"]: it for it in json.load(open("site/items.json"))["items"]}
        rank = lambda n: max([RANK[items[n]["rating"]] if n in items else 0] + [RANK[items[l]["rating"]] for l in items.get(n, {}).get("links", []) if l in items])
        todo = [n for n in open_prs if not (current(n) and checked(n))]
        # New reviews come before checks of earlier ones.
        todo = sorted(todo, key=lambda n: (current(n), open_prs[n]["isDraft"], -rank(n), -n))[:MAX]
        drafts = {n: reviews[str(n)] for n in todo if current(n)}
    start = time.time()
    with ThreadPoolExecutor(PARALLEL) as pool:
        jobs = {pool.submit(review, n, open_prs[n]["headRefOid"], open_prs[n]["baseRefName"], notes.get(str(n), []), drafts.get(n)): n for n in todo}
        for job in as_completed(jobs):
            n = jobs[job]
            try:
                r = job.result()
            except Exception as e:
                print(f"#{n} failed: {e}", file=sys.stderr, flush=True)
                continue
            # A check of an earlier review keeps the date of that review.
            date = drafts[n]["date"] if n in drafts else time.strftime("%Y-%m-%d")
            reviews[str(n)] = {**r, "sha": open_prs[n]["headRefOid"], "notes": notes.get(str(n), []), "discussed": discussed(open_prs[n]), "date": date}
            json.dump(reviews, open("reviews.json", "w"), indent=1)
            check = f"checked by {CHECK_MODEL} with {len(r['check']['changes'])} changes" if r["check"] else "not checked"
            print(f"#{n} {r['verdict']} with {len(r['findings'])} findings and {len(r['discussion'])} answers, {check}, after {int(time.time() - start)} s", flush=True)


if __name__ == "__main__":
    main()
