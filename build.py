#!/usr/bin/env python3
"""Turns raw.json into site/items.json with topics and an EUDI relevance rating."""
import html, json, os, re, shutil

raw = json.load(open("raw.json"))
labelled = set(raw["labelled"])
# Reviewed ratings of open items, with the basis and the specs.json entries behind them.
reviewed = {int(n): v for n, v in json.load(open("ratings.json")).items()}
# PRs with a running build on the Keycloak wallet demo.
demos = {int(n): u for n, u in json.load(open("demos.json")).items()}
# Claude Code reviews of open PRs, from review.py.
reviews = json.load(open("reviews.json")) if os.path.exists("reviews.json") else {}
same = dict(tuple(int(x) for x in l.split()) for l in open("same.tsv") if l.strip())
specs = {s["id"]: s for s in json.load(open("specs.json"))}


# Like eudi-dev (ADR-0010, ADR-0025), each item cites the one rule that defines it. Reviewers listed that rule first, and ARF wins over ETSI.
def cite(refs):
    if not refs:
        return []
    arf = [r for r in refs if specs[r]["doc"].startswith("ARF ")]
    return [arf[0] if specs[refs[0]]["doc"].startswith("ETSI") and arf else refs[0]]


# A short rule name such as "ARF ISSU_21" or "HAIP 1.0 §5.2".
def rule(s):
    doc = re.sub(r"^ARF 3\.0\.0 .*", "ARF", s["doc"])
    doc = re.sub(r"^ARF PID Rulebook.*", "PID Rulebook", doc)
    doc = re.sub(r"^German PID Rulebook.*", "PID-DE", doc)
    doc = re.sub(r"^CIR \(EU\) (\S+).*", r"CIR \1", doc)
    doc = re.sub(r" V\d[\d.]*$| \(editor's draft\)$", "", doc)
    ref = s["ref"].split(", ")[-1]
    ref = re.sub(r" \(.*", "", ref).replace("Section ", "§").replace("Article ", "Art. ")
    return f"{doc} {ref if len(ref) <= 32 else 'optional attributes' if doc == 'PID-DE' else ref[:30] + '…'}"

# (topic, rating, pattern) matched against the title and labels.
TOPICS = [
    ("Trust", "high", r"etsi|trust(ed)? ?list|\blote\b|\blotl\b|trust anchor|x5c|x\.?509|certificate|\bcrl\b"),
    ("Status & revocation", "high", r"status ?list|revo[ck]"),
    ("mdoc", "high", r"mdoc|mso_mdoc|18013|cbor"),
    ("Attestation", "high", r"attest|abca"),
    ("HAIP", "high", r"haip"),
    ("OID4VP", "high", r"oid4vp|openid4vp|verifiable presentation|dcql|\bverifier\b|eudi.?wallet"),
    ("Security", "high", r"cve-|replay|impersonat|vulnerab|leak|bypass|weakness|exposes|harden"),
    ("DPoP & PAR", "medium", r"dpop|\bpar\b|pushed auth"),
    ("Pre-authorized", "medium", r"pre-?auth|tx_code|transaction (code|pin)"),
    ("SD-JWT", "medium", r"sd-?jwt|disclos|key.?binding|\bvct\b"),
    ("Conformance", "medium", r"conformance|\bpass oid4vci|fapi2"),
    ("Issuance", "medium", r"metadata|nonce|proof|credential (endpoint|request|response|offer)|offer|batch|encrypt|deferred|notification|refresh|authorization.?detail|credential.?configuration|issu"),
    ("Other formats", "low", r"jwt_vc|ldp|\bld[- ]|vcdm|json-ld|linked data"),
    ("UI & docs & tests", "low", r"admin ui|admin console|account console|\bui\b|\bdocs?\b|documentation|\btests?\b|e2e|flaky|refactor|cleanup|clean up|release notes|renam|naming|polish"),
]
RANK = {"none": 0, "low": 1, "medium": 2, "high": 3, "blocker": 4}
SECURITY_LABEL = {"kind/weakness", "kind/cve"}


def size_class(n):
    return "XS" if n < 10 else "S" if n < 100 else "M" if n < 500 else "L" if n < 1000 else "XL"


raw_by_n = {i["number"]: i for i in raw["items"]}
items = []
for i in raw["items"]:
    if i["createdAt"] < "2023" and i["number"] not in labelled:
        continue
    pr = i["__typename"] == "PullRequest"
    labels = [l["name"] for l in i["labels"]["nodes"]]
    text = i["title"] + " " + " ".join(labels)
    # Many feature PRs carry a flaky-test label, so tests and docs count only when the title names them.
    topics = [(t, r) for t, r, p in TOPICS if re.search(p, i["title"] if t == "UI & docs & tests" else text, re.I)]
    if SECURITY_LABEL & set(labels) and not any(t == "Security" for t, _ in topics):
        topics.append(("Security", "high"))
    direct = i["number"] in labelled or re.search(r"oid4v|openid4v|verifiable|sd-?jwt|mdoc|credential|haip|eudi|dcql|abca|attest", i["title"], re.I)
    names = [t for t, _ in topics]
    basis, refs, why = "", [], ""
    if i["number"] in reviewed:
        v = reviewed[i["number"]]
        rating, note, basis, refs = v["rating"], v["note"], v["basis"], cite(v["specs"])
        why = v.get("why", "")
    else:
        strong = [r for t, r in topics if t != "UI & docs & tests"]
        rating = max(strong, key=RANK.get, default="low")
        if "UI & docs & tests" in names and "Security" not in names:
            rating = "low"
        if "Other formats" in names:
            rating = "low"
        if not direct and rating != "high" and "DPoP & PAR" not in names:
            rating = "none"
        note = ""
    state = "merged" if i.get("mergedAt") else i["state"].lower()
    if state == "closed" and not pr and i.get("stateReason") == "COMPLETED":
        state = "done"
    # Items rated as unrelated to EUDI stay out of the list.
    if state == "closed" or not direct or rating == "none":
        continue
    links = [r["number"] for r in (i.get("closingIssuesReferences") or i.get("closedByPullRequestsReferences") or {"nodes": []})["nodes"]]
    it = {
        "n": i["number"], "pr": pr, "title": i["title"].strip(), "url": i["url"], "state": state,
        "draft": i.get("isDraft", False), "author": (i["author"] or {}).get("login", "ghost"),
        "created": i["createdAt"][:10], "updated": i["updatedAt"][:10], "closed": (i["closedAt"] or "")[:10],
        "parent": (i.get("parent") or {}).get("number"), "parentTitle": (i.get("parent") or {}).get("title", ""),
        "subs": [i["subIssuesSummary"]["completed"], i["subIssuesSummary"]["total"]] if (i.get("subIssuesSummary") or {}).get("total") else None,
        "assignees": [a["login"] for a in i["assignees"]["nodes"]], "labels": [l for l in labels if not l.startswith(("team/", "status/"))], "comments": i["comments"]["totalCount"],
        "reactions": i["reactions"]["totalCount"], "milestone": (i["milestone"] or {}).get("title", ""),
        "topics": names or ["General"], "rating": rating, "note": note, "why": why, "basis": basis, "specs": refs, "direct": bool(direct), "links": links,
    }
    if i["number"] in demos:
        it["demo"] = demos[i["number"]]
    if pr:
        it.update(add=i["additions"], del_=i["deletions"], files=i["changedFiles"], review=i.get("reviewDecision") or "")
        it["size"] = size_class(it["add"] + it["del_"])
        author = it["author"]
        humans = lambda a: a and a["__typename"] == "User" and a["login"] != author and "copilot" not in a["login"].lower()
        it["reviews"] = [[r["author"]["login"], r["state"].lower()] for r in (i.get("latestReviews") or {"nodes": []})["nodes"] if humans(r["author"])]
        reviewers = {r[0] for r in it["reviews"]}
        # CODEOWNERS requests teams on every PR, so only requested people say something.
        it["reviews"] += [[x, "requested"] for x in i.get("requested", []) if x not in reviewers and not x.startswith("team:")]
        rv = reviews.get(str(i["number"]))
        if rv and state == "open":
            count = lambda sev: sum(f["severity"] == sev for f in rv["findings"])
            level = "major" if count("major") else "minor" if count("minor") else "clean"
            it["ai"] = {"level": level, "headline": rv.get("headline") or re.split(r"(?<=\.) ", rv["summary"])[0][:140],
                        "major": count("major"), "minor": count("minor"), "followup": count("followup"), "stale": rv["sha"] != i.get("headRefOid")}
    items.append(it)

# PRs often name their issue without a closing keyword, which GitHub doesn't link.
# Only the title and phrases like "Part of #N" count, since bodies also mention related issues.
ISSUE_REF = r"(?:#|https://github\.com/keycloak/keycloak/issues/)(\d{4,6})\b"
LINK_PHRASE = re.compile(r"(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?|part of|implements|addresses|issue|for)\s*:?\s*" + ISSUE_REF, re.I)
by_n = {it["n"]: it for it in items}
for it in items:
    if not it["pr"]:
        continue
    body = raw_by_n[it["n"]]["body"] or ""
    refs = [int(n) for n in re.findall(ISSUE_REF, it["title"])] + [int(m.group(1)) for m in LINK_PHRASE.finditer(body)]
    for n in refs:
        issue = by_n.get(n)
        if issue and not issue["pr"]:
            if n not in it["links"]:
                it["links"].append(n)
            if it["n"] not in issue["links"]:
                issue["links"].append(it["n"])

# Duplicates fold into the issue that carries the work.
for dup, target in same.items():
    if dup in by_n and target in by_n:
        t = by_n[target]
        t["same"] = t.get("same", []) + [dup]
        t["links"] += [n for n in by_n[dup]["links"] if n not in t["links"]]
items = [it for it in items if it["n"] not in same or same[it["n"]] not in by_n]

def findings_label(rv):
    major, minor, follow = (sum(f["severity"] == s for f in rv["findings"]) for s in ("major", "minor", "followup"))
    parts = [major and f"{major} must fix", minor and f"{minor} should fix", follow and f"{follow} {'follow-up' if follow == 1 else 'follow-ups'}"]
    verdict = "Merge as is" if not (major or minor) else "Fix first"
    return " · ".join([verdict] + [", ".join(x for x in parts if x)] * any(parts))


def level(rv):
    sev = {f["severity"] for f in rv["findings"]}
    return "major" if "major" in sev else "minor" if "minor" in sev else "clean"


SEVERITY = {"major": "Must fix", "minor": "Should fix", "followup": "Follow-up", "nit": "Nit"}
MODEL_NAME = {"claude-opus-5-5": "Claude Opus 5.5"}
REVIEW_STATE = {"major": "requested changes", "minor": "commented", "clean": "approved these changes"}


def text(s):
    return re.sub(r"`([^`]+)`", r"<code>\1</code>", html.escape(s))


def diff_table(rows):
    out = []
    for kind, old, new, line in rows:
        if kind == "@":
            out.append(f'<tr class="d-hunk"><td colspan="3">{html.escape(line)}</td></tr>')
        else:
            cls = {"+": "d-add", "-": "d-del"}.get(kind, "")
            out.append(f'<tr class="{cls}"><td class="ln">{old or ""}</td><td class="ln">{new or ""}</td>'
                       f'<td class="code"><span class="mark">{kind if kind != " " else ""}</span>{html.escape(line)}</td></tr>')
    return f'<table class="diff">{"".join(out)}</table>' if out else ""


def in_diff(f):
    """GitHub takes inline comments only on lines of the PR diff."""
    last = f.get("context") or [[None, None, None]]
    kind, old, new = last[-1][:3]
    return new == f["line"] and (kind == "+" or (kind == " " and old is not None))


def sorted_findings(rv):
    order = {s: k for k, s in enumerate(SEVERITY)}
    return sorted(rv["findings"], key=lambda f: (order[f["severity"]], f["file"], f["line"] or 0))


def markdown(f):
    parts = [f'**{SEVERITY[f["severity"]]}** · {f["title"]}']
    parts += [f'**Impact:** {f["impact"]}'] * bool(f.get("impact"))
    parts += [f["detail"]] * bool(f["detail"])
    parts += [f'**Suggested fix:** {f["fix"]}'] * bool(f["fix"])
    return "\n\n".join(parts)


def review_export(rv):
    """What the post service turns into a GitHub review. The page sends only the ids it picked."""
    lv, effort = level(rv), f'{rv["effort"]} effort' if rv["effort"] else "default effort"
    intro = [f"### AI review: {findings_label(rv)}", rv["summary"]] + [f'**For EUDI:** {rv["eudi"]}'] * bool(rv["eudi"])
    intro.append(f'<sub>Claude Code wrote this review of {rv["sha"][:7]} with {MODEL_NAME.get(rv["model"], rv["model"])} at {effort}. '
                 "It read the code without building or running it.</sub>")
    return {"sha": rv["sha"], "level": lv, "intro": "\n\n".join(intro),
            "findings": [{"id": k, "severity": f["severity"], "path": f["file"], "line": f["line"], "inDiff": bool(f["file"]) and in_diff(f),
                          "body": markdown(f)} for k, f in enumerate(sorted_findings(rv))]}


POST_DEFAULT = {"major", "minor", "followup"}


def post_panel(it, rv):
    counts = {s: sum(f["severity"] == s for f in rv["findings"]) for s in SEVERITY}
    boxes = "".join(f'<label><input type="checkbox" data-sev="{s}"{" checked" if s in POST_DEFAULT else ""}> {SEVERITY[s]} <span class="muted">{n}</span></label>'
                    for s, n in counts.items() if n)
    return f'''<section class="post" id="post" data-pr="{it["n"]}" hidden>
<div class="post-head"><b>Post to GitHub</b><span class="muted" id="post-user"></span></div>
<p class="muted">Posts the summary and the picked comments as a review on <a href="{it["url"]}">#{it["n"]}</a>, in your name.</p>
{f'<div class="post-sev">{boxes}</div>' if boxes else ""}
<div class="post-actions"><a class="btn" id="post-login" href="#" hidden>Sign in with GitHub</a><button class="btn" id="post-send" type="button" hidden></button><button class="link-btn" id="post-logout" type="button" hidden>Sign out</button></div>
<p class="post-msg" id="post-msg" role="status"></p>
</section>'''


def comment(f, k):
    fix = f'<div class="suggest"><div class="suggest-head">Suggested fix</div><p>{text(f["fix"])}</p></div>' if f["fix"] else ""
    pick = f'<label class="pick" hidden><input type="checkbox" data-id="{k}" data-sev="{f["severity"]}"{" checked" if f["severity"] in POST_DEFAULT else ""}> Post</label>'
    return f'''<div class="cmt" id="c{k}"><div class="cmt-head"><span class="avatar">C</span><b>Claude</b><span class="sev sev-{f["severity"]}">{SEVERITY[f["severity"]]}</span>{pick}</div>
<div class="cmt-body"><p class="cmt-title">{text(f["title"])}</p>{f'<p class="impact"><b>Impact</b> {text(f["impact"])}</p>' if f.get("impact") else ""}<p>{text(f["detail"])}</p>{fix}</div></div>'''


def review_page(it, rv, head):
    sha, lv = rv["sha"], level(rv)
    findings = sorted_findings(rv)
    files = {}
    for k, f in enumerate(findings):
        files.setdefault(f["file"] or "", []).append((k, f))
    toc, blocks = [], []
    for path, fs in files.items():
        fs.sort(key=lambda x: x[1]["line"] or 0)
        url = f"https://github.com/keycloak/keycloak/blob/{sha}/{html.escape(path)}"
        sevs = "".join(f'<span class="dot sev-{f["severity"]}" title="{SEVERITY[f["severity"]]}"></span>' for _, f in fs)
        name = html.escape(path.rsplit("/", 1)[-1]) if path else "General"
        toc.append(f'<li><a href="#c{fs[0][0]}">{sevs}<span class="toc-name">{name}</span></a><span class="toc-dir">{html.escape(path.rsplit("/", 1)[0]) if "/" in path else ""}</span></li>')
        body = "".join((diff_table(f.get("context", [])) + comment(f, k)) for k, f in fs)
        head_bar = f'<a href="{url}">{html.escape(path)}</a>' if path else "General"
        blocks.append(f'<section class="file"><div class="file-head">{head_bar}<span class="muted">{len(fs)} {"comment" if len(fs) == 1 else "comments"}</span></div>{body}</section>')
    stale = "" if sha == head else '<p class="stale">The PR has new commits since this review. The next daily run reviews them.</p>'
    good = "".join(f"<li>{text(g)}</li>" for g in rv["good"])
    return f'''<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Review of #{it["n"]}</title>
<link rel="icon" href="../logo.svg" type="image/svg+xml">
<link rel="stylesheet" href="../style.css">
<script src="../review.js" defer></script>
</head>
<body class="review-page">
<header class="top"><div class="wrap narrow-wrap">
<a class="back" href="../">← Keycloak OpenID4VC Tracker</a>
<h1>{html.escape(it["title"])} <a class="pr-n" href="{it["url"]}">#{it["n"]}</a></h1>
<p class="sub"><span class="badge s-open">Open</span> {html.escape(it["author"])} wants to merge <code>{it["add"]:,}</code> added and <code>{it["del_"]:,}</code> deleted lines in {it["files"]} files</p>
</div></header>
<main class="wrap narrow-wrap">
{stale}
<section class="rv-box rv-{lv}">
<div class="rv-head"><span class="avatar">C</span><b>Claude</b> {REVIEW_STATE[lv]} on <a href="https://github.com/keycloak/keycloak/pull/{it["n"]}/commits/{sha}"><code>{sha[:7]}</code></a> · {rv["date"]} · {MODEL_NAME.get(rv["model"], rv["model"])}, {f'{rv["effort"]} effort' if rv["effort"] else "default effort"}<span class="rv-count">{findings_label(rv)}</span></div>
<div class="rv-body"><p>{text(rv["summary"])}</p>{f'<p><b>For EUDI</b> {text(rv["eudi"])}</p>' if rv["eudi"] else ""}{f'<p class="good-head">Done well</p><ul class="good">{good}</ul>' if good else ""}</div>
</section>
{post_panel(it, rv) if sha == head else ""}
{f'<nav class="toc"><div class="toc-head">Files with comments</div><ul>{"".join(toc)}</ul></nav>' if toc else ""}
{"".join(blocks)}
</main>
<footer class="wrap narrow-wrap foot">Claude Code wrote this review with {MODEL_NAME.get(rv["model"], rv["model"])} at {f'{rv["effort"]} effort' if rv["effort"] else "its default effort"}. It read the code but did not build or run it, so treat each comment as a lead to check. This is not a review by the Keycloak team.</footer>
</body>
</html>
'''


shutil.rmtree("site/reviews", ignore_errors=True)
os.makedirs("site/reviews")
for it in items:
    if "ai" in it:
        rv = reviews[str(it["n"])]
        open(f"site/reviews/{it['n']}.html", "w").write(review_page(it, rv, raw_by_n[it["n"]].get("headRefOid")))
        json.dump(review_export(rv), open(f"site/reviews/{it['n']}.json", "w"), indent=1)

gaps = json.load(open("gaps.json"))
for g in gaps:
    g["specs"] = cite(g["specs"])
used = sorted({r for it in items for r in it["specs"]} | {r for g in gaps for r in g["specs"]})
json.dump({"fetched": raw["fetched"], "items": items, "specs": {r: {"rule": rule(specs[r]), **{k: specs[r][k] for k in ("doc", "ref", "url", "text")}} for r in used},
           "docs": json.load(open("spec-docs.json")), "gaps": gaps}, open("site/items.json", "w"), separators=(",", ":"))
# Open work without a hand rating, for rate.py. Epics and PRs attached to an open issue take no rating of their own.
parents = {it["parent"] for it in items if it["parent"]}
attached = {n for it in items if not it["pr"] and it["state"] == "open" for n in it["links"]}
unrated = [it for it in items if it["state"] == "open" and not it["basis"] and it["n"] not in parents and not (it["pr"] and it["n"] in attached)]
json.dump([{"n": it["n"], "kind": "PR" if it["pr"] else "issue", "title": it["title"], "labels": it["labels"],
            "body": (raw_by_n[it["n"]]["body"] or "")[:1500], "epic": it["parentTitle"] or None} for it in unrated], open("unrated.json", "w"), indent=1)
from collections import Counter
print(len(items), Counter(i["rating"] for i in items), Counter(i["state"] for i in items))
