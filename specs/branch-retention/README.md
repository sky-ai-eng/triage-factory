# Bot-branch retention

Read this before changing what Triage Factory deletes from a GitHub repository on its own, or
the `branch_pushed` / `branch_deleted` rows of the audit ledger. It records the design for
**TFAC-491**. Status: **proposed**. Every entry under **Decisions** is a proposal for review;
**Open questions** lists what the proposal does not settle. No implementation lands until the
decisions are accepted.

Scope: both run modes, core, except the org ceiling (§5.3), which is multi-mode and governance
(EE).

---

## 1. The gap

TF keeps every branch it pushes. That is deliberate: dismissing a draft pull request, deleting a
pending review and tearing a task down are all reversible, but deleting a branch can destroy
commits nothing else holds. A bare `git push` with no pull request is the clearest case. Its
only record is the `branch_pushed` ledger row. The one TF path that deletes a branch is a
person rejecting a draft pull request (`POST /api/deliverables/{id}/reject`), behind its own
confirmation.

So branches accumulate. GitHub's repository setting "Automatically delete head branches"
removes the branch of a merged pull request. What it leaves is the residue this design is
about: branches TF created that have **no open and no merged pull request**. These come from
abandoned or dismissed pull requests, bare pushes, and runs that pushed and then stopped.

## 2. Principles

1. **TF deletes only a branch it can prove holds nothing but its own commits.** TF created it,
   every push to it since is one TF recorded, and its live tip is still the last commit TF
   pushed (§4). A branch a person has pushed to stops being TF's to delete.
2. **A branch with a reason to exist is never a candidate.** That covers the head or base of an
   open pull request, a branch whose pull request merged (the repository's own setting decides
   those), a protected name, and a branch whose conversation is still working.
3. **Fail toward keeping.** Every input that cannot be read (a ledger row with no old SHA, an
   unreachable repository, a GitHub error) resolves to keeping the branch, either for now or for
   good. A wrong keep costs clutter. A wrong delete can cost work nobody can recover.
4. **No change of setting deletes a branch sooner than the notice period after the change**
   (§6.2). Lowering a retention value never deletes branches on the next pass.
5. **Every delete is on the ledger, as part of the run's artifacts, with the commit it removed.**
   A person can see that TF deleted the branch, and the recorded SHA is what a restore needs.

## 3. Vocabulary

| Term               | Meaning                                                                                                                                                |
| ------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------ |
| **TF branch**      | A branch whose current lifetime began with a TF push that created it.                                                                                  |
| **Lifetime**       | The ledger rows of one branch (same `scope`, `repository_id`, `object_key`) from its most recent creating push onward.                                 |
| **Push chain**     | The lifetime's `branch_pushed` rows, each pointing at the previous one through its `old_sha`.                                                          |
| **Origin**         | What kind of conversation pushed the branch, read from `conversations.type`: `delegation` (task work) or `interactive` (a person's session, TFAC-378). |
| **Retention**      | How long a TF branch is kept after its clock starts: a whole number of days, or never.                                                                 |
| **Clock start**    | When retention starts counting (§6.1).                                                                                                                 |
| **Ceiling**        | An org admin's maximum retention for an origin (§5.3).                                                                                                 |
| **Notice period**  | The minimum time between a branch being scheduled for deletion and the deletion (§6.2).                                                                |
| **Retention item** | The work row that schedules, re-checks and performs one branch's deletion (§7).                                                                        |

## 4. What may be deleted

A branch is deleted only when every condition below holds. The scan (§7.1) checks the ledger
conditions. The worker (§7.2) checks all of them again just before the delete, including the
live ones, because the ledger only records what TF did.

**From the ledger and TF's own tables:**

1. The branch has a lifetime: a `branch_pushed` row with `new = true`, performed by an agent,
   after the branch's latest `branch_deleted` row, if it has one.
2. The lifetime's latest row is a `branch_pushed`, not a `branch_deleted`.
3. **The push chain is unbroken.** Each `branch_pushed` row after the creating one carries an
   `old_sha` equal to the `sha` of the row before it. A gap means something other than TF moved
   the branch between two TF pushes, possibly a person adding commits that TF then built on. A
   lifetime with exactly one push needs no `old_sha`. A row recorded before `old_sha` existed
   (§8.1) breaks the chain, so that branch is kept.
4. The branch is in a repository the org still holds (a live `repositories` row), on the org's
   current GitHub host (`scope = org_settings.github_base_url`).
5. Every conversation that pushed in the lifetime is **settled** (`Conversation.Settled`: failed
   or concluded). A conversation still queued, running, or parked and waiting on a person may
   push again.
6. The clock start plus the retention (§5, §6) has passed.

**Live, from GitHub, at execution:**

7. The branch name is not in the repository's protected set as `pushpolicy.ProtectedBranches`
   computes it (`main`, `master`, the default branch, and the configured base branch). This
   check ignores the team's base-branch push policy: a team that lets agents push to `main` has
   not let TF delete it.
8. The ref exists, and its tip is the `sha` of the lifetime's last push. Any other tip means
   someone else pushed, and the branch is no longer TF's.
9. No open pull request has the branch as its **head** (draft pull requests count as open, so a
   deliverable still awaiting a verdict is never touched).
10. No open pull request has the branch as its **base**. Deleting the base of a stacked pull
    request closes that pull request.
11. No merged pull request has the branch as its head. The repository's "automatically delete
    head branches" setting decides those. TF does not override a repository that chose to keep
    merged branches.
12. If a closed, unmerged pull request has the branch as its head, its `closed_at` becomes part
    of the clock start (§6.1), and condition 6 is checked again.

Nothing is inferred from the branch's name. `team_settings.branch_template` is prompt guidance,
not enforcement, so a name proves nothing about who made the branch.

## 5. Policy

### 5.1 Origin is the primary axis

Retention is chosen by the origin of the conversations that pushed the branch:

- **`delegation`**: task work. Its branches are by-products of a run. Default **14 days**.
- **`interactive`**: a person's session (TFAC-378), which is a workspace the person keeps
  working in. Its branches are deliberate. Default **never**.
- **Any other type**, including a future `subagent:<kind>`, resolves to **never** until a
  decision gives it a value.

When conversations of more than one origin, or of more than one team, pushed in one lifetime,
the **longest** retention among them applies, and never beats any number of days. The branch
belongs to all of them, and principle 3 picks the reading that keeps it.

### 5.2 The team setting

Retention is a team setting, because a team owns its conversations (`conversations.team_id`).
It is core and works in both modes.

| Field                              | Type            | Default               | Who writes it |
| ---------------------------------- | --------------- | --------------------- | ------------- |
| `delegation_branch_retention_days` | integer or null | 14 for new teams (§9) | team admin    |

- Written through `PATCH /api/teams/{team_id}/settings`: absent keeps, an integer sets, explicit
  `null` clears to **never**. This is the same shape as the spend caps, where null means no cap.
- Valid values are 1 through 365. Anything else is a 400 naming the field. Nothing is clamped.
- `GET /api/teams/{team_id}/settings` returns the stored value and
  `delegation_branch_retention_effective_days`, the value after the org ceiling (§5.3), so a
  team admin can see when the org has clamped their choice.

The interactive origin's field, `interactive_branch_retention_days` (default null), ships with
the interactive conversation surface and not before it. Until then nothing mints an interactive
conversation, and a field that controls nothing would be an untested promise. The rule in §5.1
already gives interactive branches the right answer (never) without the field.

### 5.3 The org ceiling (governance)

An org admin can set a maximum retention per origin. Repositories are org-wide and shared
between teams, so one team's "never" leaves branches in repositories other teams also work in.
A ceiling is the org's way to say "no team keeps bot branches longer than this."

| Field                 | Type            | Default           |
| --------------------- | --------------- | ----------------- |
| `delegation_max_days` | integer or null | null (no ceiling) |

- `GET` / `PATCH /api/orgs/{org_id}/branch-retention`. Org admin, gated on
  `entitlements.FeatureGovernance`, 404 when the org is not entitled (the same posture as the
  per-team spend caps). Multi-mode only: local has one team, so the ceiling would be a second
  control for the same person.
- Stored in its own row (`org_branch_retention`, keyed by `org_id`), not in `org_settings`, for
  the reason `org_event_sources` is: `org_settings` is a whole-row write behind a `version`
  token, and a one-column governance write would conflict with it.
- Valid values are 1 through 365, or null.
- **Effective retention = min(team value, ceiling)**, where a team value of never counts as
  unbounded. A ceiling turns a team's "never" into the ceiling. It is evaluated on every read and
  every execution. Nothing rewrites a team row when the ceiling changes, the same rule
  `TeamModelSet` follows.
- When the entitlement lapses, stored ceilings are ignored at evaluation and kept in storage.
  This matches how the per-team spend caps behave.

The ceiling is one-sided. See **Open questions** for a floor.

## 6. Timing

### 6.1 Clock start

A branch's clock starts at the latest of:

- the `occurred_at` of the lifetime's last push;
- the time each conversation that pushed in the lifetime settled;
- the `closed_at` of the most recently closed, unmerged pull request with the branch as its
  head (live, condition 12).

A new push moves the clock. A pull request closed after a long time open gets the full
retention from its close, not from the push that preceded it.

### 6.2 Notice period

A branch is never deleted less than **72 hours** after its retention item was scheduled with
that date. Each time the worker sets an item's `delete_after` (at admission, or when a deferral
recomputes the due time), it sets it to `max(due, now + 72h)`.

This is what makes principle 4 hold. Lowering a team's retention from 30 days to 7, or setting
a ceiling for the first time, schedules every branch that is now due, and none of them is
deleted before 72 hours have passed. During that window the schedule is visible (§8.3), and a
person can change the setting back or open a pull request on the branch.

## 7. Execution

A background pass rather than on-demand cleanup. The accumulation happens steadily, not at a
moment anyone would think to clean up, and an on-demand button applies the same rules anyway.
A "clean up now" action that admits items through the same scan is a later addition (§11).

Both halves run in the background brain: under the leader lease in multi mode, and directly in
local mode. The work row is a `workitem` kind, so a dead holder's items are recovered by lease
expiry with no sweeper, as the mergeability probes are.

### 7.1 The scan

Hourly, per org:

1. Read the org's TF branches from the ledger, grouped by branch identity, and keep the ones
   that pass conditions 1 through 6 using only stored inputs. Without the live inputs, the scan's
   due time is never later than the real one. The scan may admit a branch early, and the worker
   defers it. The scan never misses a branch that is due.
2. Skip a branch that already has a retention item for the same last push with a final outcome
   (§7.3).
3. Admit a `branch_retention_items` row for each remaining branch, with `delete_after =
max(due, now + 72h)`.

The read is bounded per pass with keyset paging over `(repository_id, object_key)`. It needs an
index on `external_actions` covering `(org_id, object_kind, repository_id, object_key,
occurred_at)`. Implementation should confirm whether the artifact reads already have one.

### 7.2 The worker

The worker claims items whose `delete_after` has passed, using a kind-owned claim filter the way
the mergeability probe worker uses `probe_after`, interleaved per org. For each item:

1. Re-read the policy and recompute the due time. If the due time is now later, defer to
   `max(due, now + 72h)`. If retention is now never, settle with outcome `retained`, a non-final
   outcome, so a later policy change can schedule the branch again.
2. Re-check conditions 1 through 5 against the ledger. A new push since admission means the item
   is stale: settle `superseded`, and the next scan admits the new push state.
3. Read the ref, then the pull requests with the branch as head (`state=all`) and as base
   (`state=open`). Apply conditions 7 through 12.
4. Set `delete_sent_at` on the item under its receipt.
5. **Compare-and-delete.** Delete through GraphQL `updateRefs` with `beforeOid` set to the
   recorded tip and `afterOid` set to zero. GitHub applies the delete only if the branch still
   points at the commit TF checked, so a push that lands between steps 3 and 5 makes the delete
   fail rather than destroy that push. The REST `DELETE /git/refs` used by the reject verb has
   no such precondition.
6. Complete the item and record the `branch_deleted` ledger row in one transaction (§8.2).

It uses the org's system GitHub credential, which already needs contents write access to push.
GitHub calls are bounded per pass. A rate budget near exhaustion defers the remaining items
instead of failing them.

**Crash window.** If a worker dies after step 5 and before step 6, the next attempt finds the
ref gone with `delete_sent_at` set. It records the delete as TF's, with detail `unconfirmed:
true`. The other explanation, a person deleting the branch during the same few seconds, is not
worth a second mechanism. This is the external-action limit the durable-work contract already
accepts (`specs/durable-work`, §3).

### 7.3 Outcomes

| Outcome      | When                                                                        | Final for this push state |
| ------------ | --------------------------------------------------------------------------- | ------------------------- |
| `deleted`    | The compare-and-delete succeeded.                                           | yes                       |
| `gone`       | The ref was already gone, and TF had not sent a delete.                     | yes                       |
| `foreign`    | The tip moved, or the chain is broken (conditions 3, 8).                    | yes                       |
| `merged`     | A merged pull request has the branch as head (condition 11).                | yes                       |
| `protected`  | The name is protected (condition 7).                                        | yes                       |
| `refused`    | GitHub refused the delete: a ruleset, or a permission.                      | yes                       |
| `superseded` | TF pushed again after admission.                                            | no                        |
| `retained`   | The policy is now never.                                                    | no                        |
| deferred     | An open pull request, a moved clock, a working conversation, a rate budget. | (item stays scheduled)    |

Final outcomes are final for one **push state**, which is the item's unique key: `(repository_id,
object_key, id of the last branch_pushed row)`. A new TF push creates a new push state, which is
evaluated from scratch. The kind uses `UniqueWhileUnsettled`, and the scan's skip rule (§7.1,
step 2) keeps a settled push state from being admitted again.

A repository the credential can no longer reach is a transient failure. The item retries with
backoff and parks when its attempts run out. It does not record an outcome.

## 8. Recording

### 8.1 `old_sha` on `branch_pushed`

`branch_pushed.detail_json` gains `old_sha`, the remote tip the push replaced (empty for a
creating push). Both push observers already see it: the git proxy parses `<old-sha> <new-sha>
<ref>` from the receive-pack command and currently discards the old SHA, and the pre-push hook
receives the remote SHA on stdin. `domain.NewBranchPushAction` takes it as a parameter.

Rows written before this change have no `old_sha`. Under condition 3, a lifetime with more than
one push where a later push lacks it is kept. TF does not try to prove those chains some other
way.

### 8.2 The delete row

The worker writes a `branch_deleted` row in the shape the reject verb's `branchDeletedAction`
uses, with these differences:

- `performed_by = system`;
- `conversation_id` and `team_id` are those of the lifetime's last push;
- `detail_json` carries `branch`, `sha` (the commit the branch pointed at), `reason:
"retention"`, `origin`, the effective `retention_days`, and `unconfirmed` when §7.2's crash
  window applies.

`domain.ArtifactPerformers` excludes system rows today, so the run's artifact list would still
show this branch as pushed. The artifact reads (`db.ArtifactPerformerSQL`) admit one more case:
a `system` row whose action is `branch_deleted` and that names a conversation. The admission is
by action, so the board mirrors and teardown rows stay out of the run's artifacts.

### 8.3 Showing the schedule

A branch artifact with an unsettled retention item carries `scheduled_delete_at` (the item's
`delete_after`) on the artifact reads. This is the only new read in the first slice. It makes
the notice period visible in the place a person already looks at a run's branches.

## 9. Rollout

Default 14 days is for **new** teams. The migration that adds `delegation_branch_retention_days`
sets every existing team's value to null (never). Upgrading must not schedule the deletion of
every branch an existing team has accumulated. Turning retention on is a team admin's choice,
and the notice period still applies when they make it.

## 10. Decisions

| #   | Decision                                                                                                              | Reason                                                                                                                                              |
| --- | --------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1   | Origin is `conversations.type`. `delegation` defaults to 14 days, `interactive` to never, and anything else to never. | The type is stored at mint and never changes. A session's branches are deliberate, and a delegation's are by-products of a run.                     |
| 2   | Never is a valid value, written as null.                                                                              | A team that does not want TF deleting branches has to be able to say so. Null-means-unbounded is the spend caps' shape.                             |
| 3   | The team field is core, in both modes. The org ceiling is governance (EE), multi only.                                | Retention is the team's choice about its own runs. Overriding teams is a governance control, and the ceiling has nothing to override in local mode. |
| 4   | The ceiling is a maximum, and min(team, ceiling) is evaluated at read time.                                           | Branches live in org-wide repositories, so the org's interest is in limiting accumulation. Evaluating at read is the `TeamModelSet` pattern.        |
| 5   | The interactive field ships with interactive conversations.                                                           | Until then it would control nothing. §5.1 already gives those branches the right answer.                                                            |
| 6   | Deletion runs as a background pass on a `workitem` kind.                                                              | Accumulation is continuous, and nobody remembers to run an on-demand cleanup. Lease recovery and per-org fairness come with the contract.           |
| 7   | A branch is TF's only through an unbroken push chain and a matching live tip.                                         | That is what proves the branch has no one else's commits. Creation alone does not: a person can push to a TF branch and the agent can build on top. |
| 8   | Merged pull requests' branches are left alone.                                                                        | The repository's own setting covers them, and a repository that keeps merged branches has decided to.                                               |
| 9   | The delete is a compare-and-delete (GraphQL `updateRefs` with `beforeOid`).                                           | It closes the race between the tip check and the delete.                                                                                            |
| 10  | There is a 72-hour notice period, and every reschedule restarts it.                                                   | No setting change deletes branches immediately, and the schedule is visible before it runs.                                                         |
| 11  | The delete is a `system` row the artifact reads admit by action.                                                      | A run's artifacts must show that its branch was deleted, and by whom.                                                                               |
| 12  | Existing teams start at never.                                                                                        | An upgrade must not delete accumulated branches without someone choosing it.                                                                        |

## 11. Later

- **Keep a branch.** A person exempts one branch from retention, written as a hold on its
  retention item. The hold settles the item with a final outcome `held` for that push state.
- **Restore a deleted branch.** Re-create the ref at the recorded `sha`. This works while GitHub
  still has the commit, which is usually a long time but not guaranteed. Until the verb exists,
  the recorded SHA lets a person restore the branch by hand.
- **Clean up now.** An org or team action that runs the scan immediately. The notice period
  still applies.
- **A notice before deletion** through durable notifications (`specs/durable-notifications.md`):
  "N branches in owner/repo will be deleted on <date>".
- **`interactive_branch_retention_days`** and its ceiling, with TFAC-378. That work also decides
  whether a session that stays open indefinitely should block its branches' clock the way an
  unsettled delegation does (condition 5).

## 12. Open questions

1. **A floor.** Should an org admin also be able to set a minimum retention, so a team cannot
   choose 1 day? The proposal has only the 1-day validation minimum, because a team's branches
   are its own runs' work.
2. **Defaults.** 14 days for delegations is a guess. 7 days is closer to how quickly an abandoned
   run is forgotten. GitHub's branches page calls a branch stale after three months without
   commits.
3. **Notice period.** Is 72 hours right, and should it be configurable?
4. **GitHub Enterprise Server without `updateRefs`.** The mutation appears in the GHES GraphQL
   docs for current releases. Before implementation, confirm the oldest GHES version TF supports.
   For a host without it, the proposal is to keep the branch and record why, not to fall back to
   an unconditional REST delete.
5. **Branches of conversations whose task was dismissed by a person.** Should a person closing a
   task start the clock (it does today, through the conversation settling), or should it also
   shorten retention?
