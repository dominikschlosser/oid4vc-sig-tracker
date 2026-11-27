#!/bin/sh
# Finds new items in the EUDI scope, fetches from GitHub, rates new open items and re-checks older ratings, reviews changed PRs, rebuilds and deploys. Run it by hand or from the daily launchd job.
set -eu
cd "$(dirname "$0")"
export PATH="/opt/homebrew/bin:/usr/bin:/bin"
date
# Two runs at once delete each other's checkouts in work/, so a run that finds another one going stops.
# mkdir either creates the lock or fails, in one step. A run that was killed leaves the lock behind: remove
# cache/update.lock by hand after checking with ps that no update.sh runs.
mkdir -p cache
if ! mkdir cache/update.lock 2>/dev/null; then
	echo "another update.sh is still running, see cache/update.lock"
	exit 0
fi
trap 'rmdir cache/update.lock' EXIT
python3 scope.py || echo "scope.py failed, new items outside the search words wait for the next run"
python3 fetch.py
python3 members.py || true
python3 build.py
python3 rate.py
python3 review.py || echo "review.py failed, the page is deployed without new reviews"
python3 build.py
./deploy.sh
./push.sh || echo "push.sh failed, the new ratings and reviews stay uncommitted"
