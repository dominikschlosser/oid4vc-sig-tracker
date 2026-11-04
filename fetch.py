#!/usr/bin/env python3
"""Collects OpenID4VC related issues and PRs of keycloak/keycloak into items.json."""
import json, re, subprocess, sys, time

REPO = "keycloak/keycloak"
QUERIES = [
    "label:area/oid4vc",
    *[f'"{t}"' for t in ["oid4vc", "oid4vci", "oid4vp", "openid4vc", "openid4vci", "openid4vp",
                         "verifiable credential", "verifiable credentials", "sd-jwt", "mdoc", "credential offer",
                         "credential issuer", "status list", "key attestation", "wallet attestation", "dcql",
                         "credential_configuration", "pre-authorized", "abca", "attestation-based"]],
]
TITLE_RE = re.compile(r"oid4v|openid4v|openid ?for ?verifiable|verifiable credential|sd-?jwt|mdoc|mso_mdoc|credential offer|"
                      r"credential issuer|credential[_ ]configuration|status ?list|key attestation|wallet attestation|dcql|"
                      r"pre-?authori[sz]ed|\bvci\b|\bvp\b|\bvc\b|issuer metadata|credential endpoint|nonce endpoint|"
                      r"proof of possession|haip|eudi|deferred|batch[ _]credential|credential[ -]request|credential scope|"
                      r"verifiable presentation|identity wallet|x5c", re.I)
BODY_RE = re.compile(r"oid4v|openid4v|openid ?for ?verifiable|verifiable credential|sd-jwt vc|credential offer", re.I)


def gh(*args):
    for attempt in range(8):
        p = subprocess.run(["gh", *args], capture_output=True, text=True)
        if p.returncode == 0:
            return json.loads(p.stdout)
        # The search API allows 30 requests per minute.
        print(p.stderr.strip()[:200], "retrying", file=sys.stderr)
        time.sleep(15 * (attempt + 1))
    raise SystemExit(p.stderr)


def search(q):
    nums = set()
    for kind in ("is:issue", "is:pr"):
        for page in range(1, 11):
            r = gh("api", "-X", "GET", "search/issues", "-f", f"q=repo:{REPO} {kind} {q}", "-f", "per_page=100", "-f", f"page={page}")
            nums |= {i["number"] for i in r["items"]}
            if len(r["items"]) < 100:
                break
    return nums


FIELDS = """
 __typename
 ... on Issue { number title url state stateReason createdAt updatedAt closedAt author{login} body
   labels(first:30){nodes{name}} comments{totalCount} reactions{totalCount} milestone{title}
   assignees(first:5){nodes{login}}
   closedByPullRequestsReferences(first:10, includeClosedPrs:true){nodes{number}}
   parent{number title} subIssuesSummary{total completed} }
 ... on PullRequest { number title url state createdAt updatedAt closedAt mergedAt author{login} body isDraft
   additions deletions changedFiles labels(first:30){nodes{name}} comments{totalCount} reactions{totalCount}
   milestone{title} reviewDecision headRefOid baseRefName assignees(first:5){nodes{login}}
   closingIssuesReferences(first:10){nodes{number}}
   latestReviews(first:10){nodes{state author{__typename login}}}
   lastComments:comments(last:10){nodes{createdAt author{__typename}}}
   lastReviews:reviews(last:10){nodes{submittedAt author{__typename}}} }
"""


def details(nums):
    out = {}
    nums = sorted(nums)
    for i in range(0, len(nums), 40):
        chunk = nums[i:i + 40]
        q = "query{repository(owner:\"keycloak\",name:\"keycloak\"){" + "".join(
            f"n{n}:issueOrPullRequest(number:{n}){{{FIELDS}}}" for n in chunk) + "}}"
        r = gh("api", "graphql", "-f", f"query={q}")
        for k, v in r["data"]["repository"].items():
            if v:
                out[v["number"]] = v
        print(f"{len(out)}/{len(nums)}", file=sys.stderr)
    return out


def main():
    cand = set()
    labelled = search(QUERIES[0])
    cand |= labelled
    for q in QUERIES[1:]:
        cand |= search(q)
    print(f"{len(cand)} candidates", file=sys.stderr)
    d = details(cand)
    keep = {n for n, v in d.items() if n in labelled or TITLE_RE.search(v["title"]) or BODY_RE.search(v["body"] or "")}
    # Pull in issues fixed by kept PRs and PRs that close kept issues.
    linked = set()
    via_epic = set()
    for n in keep:
        v = d[n]
        for ref in (v.get("closingIssuesReferences") or v.get("closedByPullRequestsReferences") or {"nodes": []})["nodes"]:
            linked.add(ref["number"])
    # Epics hold tasks whose titles name no search term.
    for n in [n for n in keep if (d[n].get("subIssuesSummary") or {}).get("total")]:
        r = gh("api", "graphql", "-f", f"query={{repository(owner:\"keycloak\",name:\"keycloak\"){{issue(number:{n}){{subIssues(first:100){{nodes{{number}}}}}}}}}}")
        subs = {x["number"] for x in r["data"]["repository"]["issue"]["subIssues"]["nodes"]}
        linked |= subs
        via_epic.update(subs)
    extra = details(linked - set(d))
    d.update(extra)
    keep |= linked
    items = [d[n] for n in sorted(keep) if n in d]
    # GraphQL hides requested teams without org access, REST lists them.
    for v in items:
        if v["__typename"] == "PullRequest" and v["state"] == "OPEN":
            r = gh("api", f"repos/{REPO}/pulls/{v['number']}/requested_reviewers")
            v["requested"] = [u["login"] for u in r["users"]] + ["team:" + t["slug"] for t in r["teams"]]
    json.dump({"fetched": subprocess.run(["date", "-u", "+%Y-%m-%dT%H:%M:%SZ"], capture_output=True, text=True).stdout.strip(),
               "labelled": sorted(labelled | via_epic), "items": items}, open("raw.json", "w"))
    print(f"{len(items)} kept", file=sys.stderr)


main()
