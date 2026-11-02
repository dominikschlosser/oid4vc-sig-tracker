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

Without a deploy.env file, update.sh builds the page into site/ and stops there. To publish it, copy deploy.env.example to deploy.env and set the SSH target and the folder your web server serves. Any static file server works. With PUSH_DATA=yes, each run also commits new ratings and reviews and pushes them to your fork.

### Cost

Each review runs Claude Code with the model and effort set by MODEL and EFFORT in review.py. A run reviews at most five pull requests, three at a time. Only pull requests with new commits get a new review. The first run on all open pull requests takes several runs. The token use of each review goes into review-usage.log.

To save on usage, set a cheaper model or a lower effort in review.py, or lower MAX.

### Run it every day

With cron:

    30 7 * * * cd /path/to/oid4vc-sig-tracker && ./update.sh >> update.log 2>&1

On macOS a launchd job works better, because it also runs after the Mac wakes up.

## Post reviews to GitHub

Each review page has a panel that posts the summary and the picked comments as a review on the pull request. People sign in with GitHub and post in their own name. Without the post service the panel stays hidden.

The service is in api/. It is a small Go program with no dependencies. To run it:

1. Create a GitHub OAuth App at https://github.com/settings/applications/new. Set the callback URL to `https://<your host>/auth/callback`.
2. Build it with `cd api && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o api .` and run it next to your web server.
3. Set GITHUB_CLIENT_ID, GITHUB_CLIENT_SECRET and PUBLIC_URL (such as `https://<your host>`). DATA_DIR is the folder with the published page, and the service reads reviews/N.json from it.
4. Send `/auth/*` and `/api/*` to the service, which listens on port 8080. Serve everything else as static files.

GitHub has no narrower scope for review comments than `public_repo`. The service keeps each token in memory and revokes it when the person signs out. It posts only text from the published review and checks first that the pull request still has the reviewed commit.

## How it works

update.sh runs these steps:

1. fetch.py reads GitHub with gh and writes raw.json.
2. build.py writes site/items.json, plus unrated.json with open items that have no rating yet.
3. rate.py asks headless Claude Code (rate-prompt.md) to rate those items. It merges valid answers into ratings.json.
4. review.py reviews open pull requests whose head commit has no review yet, the most relevant first. `python3 review.py N` reviews pull request N now. For each one it checks out the head commit as a worktree of cache/keycloak.git, writes the diff and the comments into .pr/, runs headless Claude Code (review-prompt.md) and deletes the worktree. Claude may only read inside the checkout and write .pr/review.json. The results go into reviews.json.
5. build.py runs again and writes a page per review into site/reviews/. deploy.sh copies the site to your server.
6. push.sh commits ratings.json and reviews.json when they changed and pushes them, if deploy.env sets PUSH_DATA=yes.

Files:

- ratings.json: rating, basis, spec id and note per open item. Edit it by hand to change a rating, then run build.py.
- reviews.json: the latest review per open pull request, with the commit it covers.
- review-notes.json: design decisions a pull request author confirmed, as a list per pull request number. review.py gives them to the reviewer and reviews the pull request again when they change.
- specs.json and spec-docs.json: verified requirement quotes the ratings cite.
- gaps.json: EUDI requirements with no Keycloak issue yet.
- same.tsv: a duplicate issue, then the issue it folds into.
- demos.json: pull request number and the demo URL that runs its branch.

The ratings in this repository are the opinion of the [eudi-dev](https://eudi-test.dev) project, not of the Keycloak team. Replace ratings.json with your own if you disagree.
