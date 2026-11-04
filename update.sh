#!/bin/sh
# Fetches from GitHub, rates new open items, reviews changed PRs, rebuilds and deploys. Run it by hand or from the daily launchd job.
set -eu
cd "$(dirname "$0")"
export PATH="/opt/homebrew/bin:/usr/bin:/bin"
date
python3 fetch.py
python3 members.py || true
python3 build.py
python3 rate.py
python3 review.py || echo "review.py failed, the page is deployed without new reviews"
python3 build.py
./deploy.sh
./push.sh || echo "push.sh failed, the new ratings and reviews stay uncommitted"
