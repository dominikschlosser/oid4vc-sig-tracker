#!/bin/sh
# Commits new ratings and reviews and pushes them, if deploy.env sets PUSH_DATA=yes.
set -eu
cd "$(dirname "$0")"
[ -f deploy.env ] && . ./deploy.env
[ "${PUSH_DATA:-}" = yes ] || exit 0
git diff --quiet HEAD -- ratings.json reviews.json sig-members.json && exit 0
git commit -q -s -m "chore: update ratings and reviews for $(date +%F)" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>" -- ratings.json reviews.json sig-members.json
git push -q
