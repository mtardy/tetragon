#!/bin/bash
#
set -x

echo $COMMIT_REF $CACHED_COMMIT_REF

git diff $COMMIT_REF $CACHED_COMMIT_REF -- ../docs

git diff $COMMIT_REF $CACHED_COMMIT_REF .

git diff $COMMIT_REF $CACHED_COMMIT_REF .
