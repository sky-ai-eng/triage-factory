#!/usr/bin/env bash
# Triage Factory — the HTTP surface as a path tree.
#
# The route table is registered in one long function and read by everyone who
# consumes it in URL order — the SPA, an agent holding a token, anyone auditing
# what is reachable. This prints it the second way, which is the shape the API
# design rules in CLAUDE.md are about: a resource missing its `GET /{id}` or its
# `POST /list` shows up here and does not show up in server.go.
#
#   ./scripts/api-tree.sh                    # the tree
#   ./scripts/api-tree.sh > api-surface.txt  # a file to review
#   ./scripts/api-tree.sh --flat all         # every route, one per line, sorted
#   ./scripts/api-tree.sh --flat /api/teams  # one subtree, flat
#
# Marks:
#   *  registered mutating — CSRF same-origin check applies
#   !  raw mount, no session wrapper: authenticates by other means (webhook
#      HMAC, OAuth state) or is deliberately open (health, config, the SPA)
#   ~  registered outside internal/server — the installing package is named,
#      which is how an EE surface is told from core
#
# Routes are read from the Go AST, so a registration wrapped across lines is
# still found and a path mentioned in a comment is not. Test files are skipped,
# and so is any nested checkout — a linked worktree or a clone inside the tree
# has a different commit out, so its routes are not this commit's surface. Each
# one skipped is named on stderr.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
exec go run ./scripts/apitree "$@"
