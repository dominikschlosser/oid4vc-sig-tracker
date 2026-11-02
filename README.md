# Keycloak OpenID4VC tracker

A static page that lists the OpenID4VC work in [keycloak/keycloak](https://github.com/keycloak/keycloak). It rates each open item by how much it matters for production EUDI use, and it adds an AI review to every open pull request. Claude Code does the rating and the reviews on your machine with your own Claude account.

The public instance runs at https://oid4vc-sig.eudi-test.dev.

## Run it yourself

You need:

- Python 3.9 or newer. The scripts use only the standard library.
- git.
- The [GitHub CLI](https://cli.github.com), signed in with `gh auth login`.
- [Claude Code](https://claude.com/claude-code), signed in with your own account. Run `claude` once to sign in.

Then:

    git clone https://github.com/dominikschlosser/oid4vc-sig-tracker.git
    cd oid4vc-sig-tracker
    ./update.sh
    python3 -m http.server -d site 8000

Open http://localhost:8000. The first run clones keycloak/keycloak into cache/ for the reviews, so it takes a while.

Without a deploy.env file, update.sh builds the page into site/ and stops there. To publish it, copy deploy.env.example to deploy.env and set the SSH target and the folder your web server serves. Any static file server works.

### Cost

Each review runs Claude Code with the model and effort set by MODEL and EFFORT in review.py. A run reviews at most five pull requests, three at a time. Only pull requests with new commits get a new review. The first run on all open pull requests takes several runs. The token use of each review goes into review-usage.log.

To save on usage, set a cheaper model or a lower effort in review.py, or lower MAX.

### Run it every day

With cron:

    30 7 * * * cd /path/to/oid4vc-sig-tracker && ./update.sh >> update.log 2>&1

On macOS a launchd job works better, because it also runs after the Mac wakes up.

## How it works

update.sh runs these steps:

1. fetch.py reads GitHub with gh and writes raw.json.
2. build.py writes site/items.json, plus unrated.json with open items that have no rating yet.
3. rate.py asks headless Claude Code (rate-prompt.md) to rate those items. It merges valid answers into ratings.json.
4. review.py reviews open pull requests whose head commit has no review yet, the most relevant first. `python3 review.py N` reviews pull request N now. For each one it checks out the head commit as a worktree of cache/keycloak.git, writes the diff and the comments into .pr/, runs headless Claude Code (review-prompt.md) and deletes the worktree. Claude may only read inside the checkout and write .pr/review.json. The results go into reviews.json.
5. build.py runs again and writes a page per review into site/reviews/. deploy.sh copies the site to your server.

Files:

- ratings.json: rating, basis, spec id and note per open item. Edit it by hand to change a rating, then run build.py.
- reviews.json: the latest review per open pull request, with the commit it covers.
- review-notes.json: design decisions a pull request author confirmed, as a list per pull request number. review.py gives them to the reviewer and reviews the pull request again when they change.
- specs.json and spec-docs.json: verified requirement quotes the ratings cite.
- gaps.json: EUDI requirements with no Keycloak issue yet.
- same.tsv: a duplicate issue, then the issue it folds into.
- demos.json: pull request number and the demo URL that runs its branch.

The ratings in this repository are the opinion of the [eudi-dev](https://eudi-test.dev) project, not of the Keycloak team. Replace ratings.json with your own if you disagree.
