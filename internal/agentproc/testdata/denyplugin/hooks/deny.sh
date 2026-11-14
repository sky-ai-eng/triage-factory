#!/bin/sh
# PreToolUse hook of the denyplugin test fixture. It uses only the system
# shell, so it runs under the local sandbox's home mask. Any Bash call whose
# input names the probe string is denied with a reason the test looks for;
# every other call is left to the rest of the permission checks.
input=$(cat)
case "$input" in
  *tf-plugin-probe-deny*)
    printf '%s\n' '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"denyplugin refused this command: it names tf-plugin-probe-deny"}}'
    ;;
esac
exit 0
