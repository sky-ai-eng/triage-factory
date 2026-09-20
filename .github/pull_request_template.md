<!--
TITLE (set the PR title, not here) — conventional-commit format:
  Ticketed:    type(scope): imperative summary (TFAC-NNN)
               e.g. feat(authz): revoke a removed member's org-scoped sessions on removal (TFAC-487)
  Unticketed:  type(scope): imperative summary        (no ticket suffix)
               e.g. fix(board): guard against undefined run id in card seed
  Types: feat · fix · perf · refactor · ci · docs · chore · test
         feat, fix, and perf show up in release notes

BODY — keep it short. Say what is wrong, what you did,
and how you checked it, then stop. The diff already says how; don't restate
it, and don't narrate the path that led here or the alternatives you passed
over. Highlight anything that could be missing or problematic with this
PR in the same body.
-->

## Problem

<!--
Why this change exists, for a reviewer who has NOT read the ticket. A few
sentences: the symptom, the root cause, and the blast radius only when it
changes how carefully this should be reviewed. If it's security-adjacent, say
plainly whether it's an actual access hole or defense-in-depth / hygiene.
-->

## Change

<!--
What you did. One sentence for the shape of the change, then a short bullet
per decision a reviewer could disagree with. Skip anything the diff shows on
its own. Use code fences for new payloads / API shapes / data structures.
Call out anything deliberately left OUT of scope.
-->

<details>
<summary><strong>Scope / verification</strong></summary>

<!--
Required. The commands you ran and what passed, then what the new tests cover
in a line or two. Suggested baseline (matches ./scripts/lint.sh):
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
documentation > comments > tests > code, with comments then split by whether
they sit in a test file. Lock files and blank lines are excluded and reported
separately. Add --worktree to count uncommitted work.

If you counted by hand or estimated instead, say which.
-->

| Kind          | Added | Removed |
| ------------- | ----- | ------- |
| Code          |       |         |
| Tests         |       |         |
| Documentation |       |         |
| Code comments |       |         |
| Test comments |       |         |

</details>

<!-- ───────────────────────────────────────────────────────────────────────
Optional sections — add one only when it says something the sections above
don't, delete the rest:

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
