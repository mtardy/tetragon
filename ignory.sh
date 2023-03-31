#!/bin/bash
#
set -x

git log --since yesterday
git fetch --dry-run
git status

echo $COMMIT_REF $CACHED_COMMIT_REF

git diff $COMMIT_REF $CACHED_COMMIT_REF -- ../docs

git diff $COMMIT_REF $CACHED_COMMIT_REF -- ../

git diff $COMMIT_REF $CACHED_COMMIT_REF .

git diff main

! git diff main --name-only | grep ^docs/
