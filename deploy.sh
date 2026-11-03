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
# Each file is renamed into place, so a reload during a deploy never reads a half-written file.
FILES="index.html logo.svg logo.png demo-logo.svg account.js app.js review.js style.css items.json"
ssh $H "rm -rf $D/.upload && mkdir $D/.upload"
(cd site && scp -q $FILES $H:$D/.upload/)
ssh $H "cd $D && for f in $FILES; do mv .upload/\$f \$f; done && rmdir .upload"
# Replacing the folder drops pages of PRs that are no longer open.
COPYFILE_DISABLE=1 tar --no-xattrs -C site -czf - reviews | ssh $H "cd $D && rm -rf reviews.new && mkdir reviews.new && tar -xzf - -C reviews.new --strip-components=1 && rm -rf reviews && mv reviews.new reviews"
