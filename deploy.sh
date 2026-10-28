#!/bin/sh
# Copies the page, its data and the review pages to the server named in deploy.env.
set -eu
cd "$(dirname "$0")"
if [ ! -f deploy.env ]; then
	echo "no deploy.env, the page stays in site/"
	exit 0
fi
. ./deploy.env
H=$DEPLOY_HOST D=$DEPLOY_DIR
scp -q site/index.html site/logo.svg site/demo-logo.svg site/app.js site/style.css site/items.json $H:$D/
# Replacing the folder drops pages of PRs that are no longer open.
COPYFILE_DISABLE=1 tar --no-xattrs -C site -czf - reviews | ssh $H "cd $D && rm -rf reviews.new && mkdir reviews.new && tar -xzf - -C reviews.new --strip-components=1 && rm -rf reviews && mv reviews.new reviews"
