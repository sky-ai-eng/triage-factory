#!/usr/bin/env bash
# Upsert a bot-filed tracking issue: comment on the open one if it already
# exists, otherwise create it. Prints the issue number on stdout.
#
#   ./scripts/ci-upsert-issue.sh <label> <title> <body-file>
#
# Identity is the LABEL, not a title search. `gh issue list --search "$TITLE
# in:title"` reads like the obvious way to do this and silently does not work:
# gh splits a search keyword on its FIRST colon and quotes the remainder, so a
# title containing one — "pricing schema drift: unmodeled cost field(s)" — goes
# up as the qualifier `pricing schema drift:" unmodeled cost field(s) in:title"`.
# GitHub matches nothing, gh exits 0 with an empty list, and the caller files a
# fresh duplicate on every run. Filtering by label goes through the plain REST
# list endpoint instead: no query parsing to get wrong, and no search-index lag
# on an issue opened minutes ago.
#
# Callers run this with GH_TOKEN + GH_REPO already in the environment.
set -euo pipefail

if [ "$#" -ne 3 ]; then
  echo "usage: $0 <label> <title> <body-file>" >&2
  exit 2
fi

LABEL="$1"
TITLE="$2"
BODY_FILE="$3"

# The label is the dedup key, so it has to exist before it can be filtered on
# or attached. --force makes this idempotent (creates, or updates in place).
gh label create "$LABEL" \
  --color ededed \
  --description "Filed automatically by CI" \
  --force >&2

existing="$(gh issue list --label "$LABEL" --state open --limit 100 --json number --jq '.[0].number // empty')"

if [ -n "$existing" ]; then
  gh issue comment "$existing" --body-file "$BODY_FILE" >&2
  number="$existing"
else
  url="$(gh issue create --title "$TITLE" --body-file "$BODY_FILE" --label "$LABEL")"
  number="${url##*/}"
fi

echo "$number"
