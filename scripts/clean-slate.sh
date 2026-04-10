#!/usr/bin/env bash
# Wipe all local state: database, config, keychain credentials.
# Use this to test the first-run experience from scratch.

set -euo pipefail

echo "Cleaning Todo Tinder local state..."

# Database
rm -f ~/.todotinder/todotinder.db ~/.todotinder/todotinder.db-wal ~/.todotinder/todotinder.db-shm
echo "  removed database"

# Config
rm -f ~/.todotinder/config.yaml
echo "  removed config"

# Keychain
for key in github_url github_pat github_username jira_url jira_pat; do
  security delete-generic-password -s todotinder -a "$key" 2>/dev/null && echo "  removed keychain: $key" || true
done

echo "Done. Restart the server for a fresh setup."
