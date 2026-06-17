#!/usr/bin/env bash
# Wipe all local state: database, config, and keychain credentials. Use
# this to test the first-run experience from scratch.
#
# Bare repo clones under ~/.triagefactory/repos/ are deliberately
# preserved — they're expensive to re-fetch and aren't part of the
# first-run flow this script targets. Wipe them manually if you need
# to.

set -euo pipefail

echo "Cleaning Triage Factory local state..."

# Database
rm -f ~/.triagefactory/triagefactory.db ~/.triagefactory/triagefactory.db-wal ~/.triagefactory/triagefactory.db-shm
echo "  removed database"

# Encrypted secret bag — the headless (no-keychain) secret backend's store.
# Desktop installs keep secrets in the OS keychain (swept below); headless
# installs keep them here. Harmless to remove either way.
rm -f ~/.triagefactory/secrets.enc
echo "  removed encrypted secret bag (if present)"

# Config (settings now live in the DB above; this only removes a stale
# pre-DB config.yaml left behind by ancient installs).
if [ -f ~/.triagefactory/config.yaml ]; then
  rm -f ~/.triagefactory/config.yaml
  echo "  removed legacy config.yaml"
fi

# Project knowledge dirs — the Curator materializes per-project repo
# worktrees at ~/.triagefactory/projects/<id>/repos/<owner>/<repo>/
# (and writes knowledge/summary files alongside). Project rows just
# got wiped with the database, so the disk state is orphaned. Worse:
# each repo subdir is a registered worktree of the bare clone in
# ~/.triagefactory/repos/, holding its branch as "checked out." A
# subsequent run that tries to `git fetch` that branch (e.g. the
# delegate path's `workspace add`) fails with "refusing to fetch into
# branch ... checked out at <stale path>" until the registrations
# get pruned. Wiping projects/ now and re-pruning each bare's
# worktrees/ tracker below closes that loop.
if [ -d ~/.triagefactory/projects ]; then
  # The Curator runs Claude Code with cwd =
  # ~/.triagefactory/projects/<id>/, which makes Claude Code create
  # ~/.claude/projects/<encoded(<cwd>)>/<sessionID>.jsonl. Walk each
  # project ID dir and delete its encoded session entry BEFORE removing
  # the projects tree (mirrors removeClaudeProjectsForCurator in
  # cmd/uninstall/uninstall.go — keep the two in sync).
  for dir in ~/.triagefactory/projects/*; do
    [ -d "$dir" ] || continue
    resolved=$(cd "$dir" && pwd -P) || continue
    encoded=$(printf '%s' "$resolved" | tr '/.' '-')
    rm -rf ~/.claude/projects/"$encoded"
  done
  rm -rf ~/.triagefactory/projects
  echo "  removed projects dir and any curator session JSONLs"
fi

# Prune stale worktree registrations from every preserved bare. The
# bare clones themselves (~/.triagefactory/repos/) stay — they're
# expensive to refetch and not part of the first-run flow — but their
# internal worktrees/ tracker now points at directories we just
# deleted (projects/, /tmp/triagefactory-runs/). Pruning is
# idempotent and cheap. Without this, the next `git worktree add`
# / `git fetch` against any of these bares would hit the stale-
# registration errors described in the projects-dir comment above.
if [ -d ~/.triagefactory/repos ]; then
  pruned=0
  while IFS= read -r bare; do
    git -C "$bare" worktree prune 2>/dev/null || true
    pruned=$((pruned + 1))
  done < <(find ~/.triagefactory/repos -type d -name '*.git' 2>/dev/null)
  if [ "$pruned" -gt 0 ]; then
    echo "  pruned worktrees from $pruned bare clone(s)"
  fi
fi

# Keychain — keep this list in sync with integrations.AllKeys() in
# internal/integrations/creds.go (the canonical list `triagefactory
# uninstall` sweeps via auth.SweepKeychain). Drift between the two means
# stale entries linger after clean-slate; jira_display_name was the most
# recent miss.
for key in github_url github_pat github_username jira_url jira_pat jira_display_name; do
  security delete-generic-password -s triagefactory -a "$key" 2>/dev/null && echo "  removed keychain: $key" || true
done

echo "Done. Restart the server for a fresh setup."
