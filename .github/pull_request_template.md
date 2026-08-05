<!--
TITLE (set the PR title, not here) — conventional-commit format:
  Ticketed:    type(scope): imperative summary (TFAC-NNN)
               e.g. feat(authz): revoke a removed member's org-scoped sessions on removal (TFAC-487)
  Unticketed:  type(scope): imperative summary        (no ticket suffix)
               e.g. fix(board): guard against undefined run id in card seed
  Types: feat · fix · perf · refactor · ci · docs · chore · test
         feat, fix, and perf show up in release notes
-->

## Problem

<!--
Why this change exists, written for a reviewer who has NOT read the ticket.
State the user- or system-visible symptom, the root cause, and — when it
matters — the blast radius and the cost of not doing it now. If it's
security-adjacent, say plainly whether it's an actual access hole or
defense-in-depth / hygiene.
-->

## Change

<!--
What you actually did. Lead with the shape of the change, then the details as
bullets. Use code fences for new payloads / API shapes / data structures.
Call out anything deliberately left OUT of scope.
-->

<details>
<summary><strong>Scope / verification</strong></summary>

<!--
Required. The exact commands you ran and what passed, then what the new tests
cover. Suggested baseline (matches ./scripts/lint.sh):
  Go:        ./scripts/lint.sh  ·  go test ./...
  Frontend:  (in ./scripts/lint.sh) prettier  ·  eslint  ·  tsc -b --noEmit
             (tests) cd frontend && pnpm exec vitest
  If local docker was unavailable, Postgres-touching changes will run under the
  pgtest testcontainer. A one-line note is fine in this case.
CHANGELOG is left to release-please — don't hand-edit it.
-->

</details>

<details>
<summary><strong>Line breakdown</strong></summary>

<!--
Changed lines in this PR, split by kind. Don't count these by hand:

  ./scripts/pr-lines.sh            # prints this table, filled in
  ./scripts/pr-lines.sh --explain  # ...plus how every file was bucketed

It resolves the base the same way GitHub does (the merge base of your branch
and the PR's base branch), so a branch that predates recent work on main still
reports only its own lines. Buckets are exclusive, in this order:
documentation > comments > tests > code. Lock files and blank lines are
excluded and reported separately. Add --worktree to count uncommitted work.

If you counted by hand or estimated instead, say which.
-->

| Kind          | Added | Removed |
| ------------- | ----- | ------- |
| Code          |       |         |
| Tests         |       |         |
| Documentation |       |         |
| Comments      |       |         |

</details>

<!-- ───────────────────────────────────────────────────────────────────────
Optional sections — add any that apply, delete the rest:

## Docs            — docs/help text/agent prompts you updated
## Behavior notes  — edge cases, before/after, explicit out-of-scope items
## Tests           — call out coverage separately when it's substantial

──────────────────────────────────────────────────────────────────────── -->

<!--
FOOTER — keep exactly ONE of the two lines below:
  • Ticketed:   the Resolves line (fix the ID + URL)
  • Unticketed: the literal word "Unticketed"
-->

Resolves [TFAC-NNN](https://linear.app/sky-ai-eng/issues/TFAC-NNN)
Unticketed
