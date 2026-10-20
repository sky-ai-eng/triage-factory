# Tracked Events

Triage Factory monitors GitHub PRs, Jira issues and Linear issues for state changes and emits typed events when transitions are detected. Events power the triage queue, AI scoring, delegation triggers, and the dashboard.

## How it works

The tracker runs on a configurable poll interval (default: 5 minutes). Each cycle:

1. **Discover** — search queries find new items to track
2. **Register** — new items are stored in `tracked_items` with an initial snapshot
3. **Refresh** — all tracked items are batch-fetched (GitHub via GraphQL `nodes(ids:[...])`, Jira via `id IN (...)` JQL — `key IN (...)` for an issue whose id TF has not learned yet — and Linear via GraphQL `issues` filtered by id, 50 at a time)
4. **Diff** — current snapshot is compared against the previous snapshot
5. **Emit** — typed events are recorded in the `events` table and published to the event bus

Events are emitted once per transition, not continuously. If a PR stays in the same state across multiple cycles, no events fire.

## GitHub PR Events

The `dedup_key` column is the open-set discriminator a task is keyed on; at
most one active task exists per entity, event type and `dedup_key`.

### Actionable

| Event | ID | Trigger | `dedup_key` |
|-------|----|---------|-------------|
| **Review Requested** | `github:pr:review_requested` | A user or team appears in the PR's review requests, and it is an identity TF knows: a user's login, or a team (`org/slug`) a user belongs to. One event per such reviewer. In local mode, not emitted on a PR the local user authored, where only a team they are on can be requested. A PR seen for the first time emits it for every request already on it | `user:<login>` / `team:<org>/<slug>` |
| **Changes Requested** | `github:pr:review_changes_requested` | A review requesting changes was submitted — see [Reviews](#reviews) | — |
| **Review: Commented** | `github:pr:review_commented` | A review with comments only was submitted, a reply in a review thread included — see [Reviews](#reviews) | — |
| **CI Check Failed** | `github:pr:ci_check_failed` | A check run on the head commit completes with `failure`, `timed_out`, `cancelled` or `action_required`. One event per check run; a check run already failing last cycle, by id, does not emit again, and a re-run is a new check run | — |
| **Merge Conflicts** | `github:pr:conflicts` | `mergeable` changes to `CONFLICTING` | — |
| **Ready for Review** | `github:pr:ready_for_review` | `isDraft` changes from `true` to `false` | — |
| **Label Added** | `github:pr:label_added` | A label is added | label name |
| **Label Removed** | `github:pr:label_removed` | A label is removed | label name |
| **New Comment** | `github:pr:commented` | Someone other than TF commented in the PR's conversation since the last cycle. One event per cycle however many comments landed, naming every commenter — see [Comments](#comments). Comments in review threads arrive as reviews | — |
| **Mentioned** | `github:pr:mentioned` | A user or team TF knows is @-mentioned in a new comment or review, or by an edit that adds the mention to the PR body. One event per identity, routed to that identity's team(s) as `review_requested` is — see [Mentions](#mentions) | `user:<login>` / `team:<org>/<slug>` |

### Informational

| Event | ID | Trigger | `dedup_key` |
|-------|----|---------|-------------|
| **Review: Approved** | `github:pr:review_approved` | An approving review was submitted — see [Reviews](#reviews) | — |
| **Review: Dismissed** | `github:pr:review_dismissed` | A reviewer's latest review was dismissed — see [Reviews](#reviews) | — |
| **Review Request Removed** | `github:pr:review_request_removed` | A reviewer TF knows drops off the review requests, because they reviewed or the request was withdrawn. Closes that reviewer's `review_requested` task | `user:<login>` / `team:<org>/<slug>` |
| **CI Check Passed** | `github:pr:ci_check_passed` | A check run completes with any other conclusion (`success`, `neutral`, `skipped`, `stale`) and was not already passing | — |
| **New Commits** | `github:pr:new_commits` | The head commit changes | — |
| **PR Body Updated** | `github:pr:body_updated` | The complete PR body changes, including clearing it | — |
| **PR Merged** | `github:pr:merged` | `merged` changes to `true` | — |
| **PR Closed** | `github:pr:closed` | The PR is closed without merging | — |
| **PR Opened** | `github:pr:opened` | In the catalog; the tracker does not emit it | — |
| **PR Unreachable** | `github:pr:unreachable` | TF will no longer follow a tracked PR — see below | — |

A PR is first stored from the REST listing of open PRs, which carries its
review requests and body but no check runs, reviews or comments. Its review
requests emit then, and the GraphQL refresh that follows in the same cycle
reports the check runs, reviews and merge conflicts already on it. The comments
already on it are its baseline and emit nothing, and so are the mentions in its
body unless it was created since TF last listed its repository (see
[Mentions](#mentions)).

#### Reviews

Every submitted review emits one event, by its state: `review_approved`,
`review_changes_requested` or `review_commented`. A reviewer's second comment
review emits as their first did, and two reviews one person submitted between
cycles are two events, oldest first. A reply in a review thread posted outside
a pending review is a review of its own on GitHub, holding the reply, so it
emits `review_commented` too. Inline comments reach TF only through reviews.

Each event's metadata names the review: `reviewer` (a login, spelled
`<slug>[bot]` for a bot), `review_id` (the numeric id), `comment_count` (its
inline comments) and `paths` (the distinct files those comments are on). Its
time is the review's submission.

A review TF submitted emits nothing: its `review` artifact carries the review's
id once it is posted. Neither does a thread reply TF posted: a comment review
each of whose comments TF recorded as a `comment` artifact. As with Jira and
Linear comments, the match is on ids, not on the author. A review read in the
moment between TF's post and the artifact write counts as a person's. The PR
author's own replies emit like anyone's, and so does a bot's review.

`review_dismissed` reports a change to a review already submitted: a
reviewer's latest review turning `DISMISSED`. It carries that review's id and
comment count, and an empty `paths`.

How the new reviews are found:

- The batch read returns how many submitted reviews the PR has and the newest
  of them, stored as `review_count` and `review_marker`. A PR whose count and
  newest review did not move is not read again.
- Otherwise a follow-up query pages through the PR's submitted reviews from the
  end of GitHub's list backwards, with each review's state, author, submission
  time and its comments' ids and files. The bodies of the reviews and their
  comments are read for their mentions (see [Mentions](#mentions)) and nothing
  else is kept from them. A review submitted after the stored marker is new.
  GitHub does not document the order of that list: a review started as pending
  sits where it was started if the list is by creation, so the read goes on
  until the reviews it found make up the rise in the count.
- Pending reviews are never read. The list would otherwise return the
  credential's own unsubmitted review.
- When the follow-up read fails, or reading which reviews are TF's does, the
  snapshot keeps its previous `review_count` and `review_marker` and the next
  cycle reports the same reviews.
- A rate limit on this read or on the comment read (see [Comments](#comments))
  ends both reads for the rest of the cycle, with no further request. Every
  PR not read keeps its markers and is read next cycle; the cycle still diffs
  every PR the batch read returned.
- A snapshot stored without `review_marker`, from before it existed or seeded
  from the REST listing of open PRs, compares each reviewer's latest review
  instead, as earlier versions did: one event per reviewer whose latest review
  changed state, carrying that review's id and comment count and an empty
  `paths`. This is how a newly discovered PR reports the reviews already on it.
  The next cycle diffs against the marker that refresh stored.
- The refresh gate skips an open PR whose `updatedAt` and head commit did not
  move for up to ten minutes. A review that did not advance `updatedAt` waits
  for that floor.

#### Mentions

`github:pr:mentioned` reports a user or team TF knows being @-mentioned on a
tracked PR, one event per identity. TF knows a user whose login maps to a TF
user on the org's GitHub host, and a team (`@org/slug`) that maps to a TF team;
in local mode, the local user and the teams they are on. This is the test
`review_requested` applies to a requested reviewer, and the event is keyed and
routed the same way: its `dedup_key` is `user:<login>` or `team:<org>/<slug>`,
and its task is visible to that identity's team(s) rather than the PR author's.
A later mention of an identity whose task is still open bumps that task. The
only event that closes it is the PR's end — merged, closed or unreachable —
which closes it with the PR's other tasks.

Mentions are read from three places:

- conversation comments made since the last cycle, from the comment read (see
  [Comments](#comments));
- reviews submitted since the last cycle, from their bodies and their inline
  comments, from the review read (see [Reviews](#reviews));
- the PR body. The snapshot stores the set of users and teams the body
  mentions (`body_mentions`), not the body, and an edit that adds one to the
  set emits for it. An edit that adds none emits nothing.

Only new comments and reviews are read, so editing an older comment or review
to add a mention emits nothing.

A mention follows GitHub's pattern: `@login` or `@org/team-slug`, not preceded
by a letter, digit or underscore. It does not count when it is:

- in a fenced or indented code block, in inline code, on a quoted (`>`) line,
  in an HTML comment, or inside an `http(s)` URL;
- in a comment or review TF posted, told apart as for `commented` and the
  review events: by its `comment` or `review` artifact. A reply TF posted that
  joined a person's pending review is TF's on its own;
- a commenter's mention of themselves, and the PR author's mention of
  themselves in the body, which is taken as theirs.

A bot's comment counts like anyone's. Several mentions of one identity in a
cycle emit one event, attributed to the newest comment or review that made one,
or to the body when only the body did.

The metadata names the identity as `mentioned_login` or `mentioned_team`, in
lower case, since GitHub matches logins and team slugs without regard to case.
`mention_source` is `comment`, `review` (a review's body), `review_comment` (an
inline comment in a review) or `body`. `commenter` and `comment_id` name the
comment or review: a review's id for `review`, and both empty for `body`. The
event also carries the PR's `author`, `repo`, `pr_number`, `is_draft`,
`head_sha` and `labels`. Predicates can filter on `mentioned_login`,
`mentioned_team`, `mention_source`, `commenter_in`, `commenter`, `author_in`,
`author`, `repo_in`, `is_draft` and `has_label`; a body mention has no commenter,
so no `commenter_in` list matches it. The event's time is the comment's
creation or the review's submission. The body records no time for an edit, so a
body mention takes the PR's `updatedAt`.

A PR TF finds for the first time, created after TF's previous listing of its
repository, was opened while TF followed the repository. Its body's mentions
emit with the seed that stores it, timed at its `updatedAt`. A listing's time is
when TF sent it, and a listing counts only once TF has stored every PR it
returned: when one fails to store, the next poll lists the repository again in
full and compares against the listing before. The comparison allows five
minutes before that listing, for clock skew between GitHub and TF. Any other PR
found for the first time was already open when TF last looked, so its body is a
baseline and emits nothing, as for every other change in tracking. That covers
every PR in a repository's first listing (TF's first poll, or a repository just
added to a team's tracked set), one past the 1,000 open PRs a listing returns,
and a reopened PR. A PR TF's agent opens is recorded as an entity when
the agent records it, before a poll finds it, and that entity is seeded
without events, so its body is a baseline too. A snapshot stored before
`body_mentions` existed is likewise a baseline. A PR seeded from the REST
listing takes its comments as its baseline, and one stored without
`review_marker` reads no reviews, so neither reports mentions in them on that
refresh.

Some limits:

- The body is read line by line rather than as full CommonMark. A paragraph
  indented four spaces under a list item after a blank line reads as a code
  block, and inline code that spans a line break reads as text.
- Only a review's first 100 inline comments are read.
- An edit TF makes to the PR body counts like anyone's: no artifact records it.
  So does the body of a PR TF opened when a poll lists the PR before the
  agent's record of it lands.

#### PR Unreachable

Like the Jira and Linear events of the same name, this one does not come from
the snapshot-diff: it reports that TF will no longer follow its own subject, and
it closes the entity and every task on it the way `github:pr:closed` does. Its
`reason` says why. There is one today:

- `scope_changed`: the org's GitHub base URL now names another GitHub host.
  Every active PR from the previous host retires at the start of the next
  GitHub cycle, before discovery and without GitHub being asked: an
  `owner/repo#N` on one host says nothing about the same name on another, and a
  node id from one host means nothing to the other. The rows are not moved or
  reused. A pull request on the new host that shares an old `owner/repo#N` is a
  different pull request and gets its own entity, and pointing the org back at
  the old host finds the retired rows by key and reopens the ones still open,
  the way a reopened PR is.

Metadata is the entity's last-known state from its stored snapshot (author,
repo, number, draft flag, head SHA, labels, title), or the repo and number
parsed from its key when it was never polled, plus the `host` it was polled
from and the `reason`. There is no `dedup_key` — a PR stops being followed once.

Repositories are scoped the same way. A tracked repository belongs to the host
it was added on, so after a host change the poller, the profiler and team
tracking see only the new host's repositories, and the old host's stay on file
untouched for a switch back.

## Jira Events

### Actionable

| Event | ID | Trigger |
|-------|----|---------|
| **Issue Assigned** | `jira:issue:assigned` | The `assignee` field changes to a non-empty value |
| **Issue Available** | `jira:issue:available` | An unassigned issue appears in the pickup queue, or an assigned issue becomes unassigned |
| **Priority Changed** | `jira:issue:priority_changed` | The `priority` field changes |
| **New Comment** | `jira:issue:commented` | Someone other than TF commented since the last cycle. One event per cycle however many comments landed, naming every commenter — see [Comments](#comments) |
| **Label Added** | `jira:issue:label_added` | A label is added. `dedup_key` is the label name — see [Labels](#labels) |
| **Label Removed** | `jira:issue:label_removed` | A label is removed. `dedup_key` is the label name |

### Informational

| Event | ID | Trigger |
|-------|----|---------|
| **Status Changed** | `jira:issue:status_changed` | The `status` field changes (e.g. To Do → In Progress) |
| **Issue Completed** | `jira:issue:completed` | The `status` changes to Done, Closed, or Resolved |
| **Issue Unreachable** | `jira:issue:unreachable` | TF will no longer follow a tracked issue — see below |
| **Issue Body Updated** | `jira:issue:body_updated` | The complete issue description changes, including clearing it |
| **Key Changed** | `jira:issue:key_changed` | The issue answers under a new key: it moved to another project, or its project's key was renamed — see below |

#### Body updates

Both body-update events compare a SHA-256 fingerprint of the complete source
body, before the 2,000-codepoint `entities.description` preview is truncated.
Jira's batch refresh requests `description`, so edits are detected even when an
issue no longer matches discovery's assignment/pickup queries. Jira ADF is
fingerprinted as canonical JSON: link and formatting edits count, while object
key order and JSON whitespace do not. GitHub's REST and GraphQL reads produce
the same fingerprint for the same markdown. Explicit `null` and an empty string
both mean cleared; an omitted field preserves the last observed revision and
preview.

First observations, snapshot-less seeds, and existing snapshots without a body
fingerprint establish a baseline without emitting a body-update event. Later
transitions commit with the snapshot through the existing atomic CAS/outbox
path, so retries do not duplicate events. As with other snapshot diffs, multiple
edits between polls collapse to the final observed change.

Metadata includes `previous_body_hash`, `body_hash`, and source identity/context,
without either full body. The source's issue/PR `updated` timestamp is the
available time approximation, not a dedicated body-edit timestamp. Author and
assignee identify ownership, not who edited the body. No default task rule or
delegation trigger is added; handlers can opt in through the event catalog.

The preview remains a best-effort display/scoring mirror and can lag the event.
A consumer needing full, revision-consistent content must obtain that body and
check its fingerprint rather than treat the preview as a complete document.

#### Comments

`jira:issue:commented`, `linear:issue:commented` and `github:pr:commented`
report the comments made since the previous cycle that TF did not post, in one
event however many there are. The metadata names the newest of them
(`comment_id`, `commenter`, and `commenter_account_id` on Jira or
`commenter_user_id` on Linear) and the author of each (`commenter_account_ids`
/ `commenter_user_ids` / `commenter_logins`). `commenter_in` matches when
anyone it lists wrote one of them. A Jira author's id follows the assignee's
rule (Cloud `accountId`, else Data Center `key`, else `name`), so a
`commenter_in` list holds the same values as an `assignee_in` list. A Linear
comment an integration posted names the integration as `commenter` and has no
user id, so no `commenter_in` list matches it. On GitHub the commenter is a
login, `<slug>[bot]` for a bot, whose comments emit like anyone's; the GitHub
event also carries the PR's `author`, `repo`, `pr_number`, `is_draft`,
`head_sha` and `labels`, and predicates can filter it on `author_in`, `repo_in`,
`is_draft` and `has_label`. The event's time is the newest comment's.

GitHub's event covers the PR's conversation only. A comment in a review thread
belongs to a review and emits as one (see [Reviews](#reviews)). Edits to a
comment emit nothing.

A comment an agent posts with `jira ticket comment` or `linear issue comment`
is recorded as a `comment` artifact keyed on the comment's id, and a comment
with that artifact is TF's. So is a PR comment an agent posts through the exec
verbs or with `gh pr comment`, which records the same artifact. A cycle that finds only TF's comments emits
nothing, so a trigger whose agent replies to comments does not fire again on
its own reply. The match is on the comment's id, not its author: under a
personal credential the agent comments as the person who owns it, and
excluding that author would drop the person's own comments. A comment whose
artifact was not written (the write is best-effort), or that a cycle reads in
the moment between the post and the write, counts as a person's.

How the new comments are found:

- **Jira.** The snapshot keeps the comment count and the newest comment
  (`last_comment_id`, `last_comment_at`), and the new comments are the ones
  after it. The batch read returns a capped page of comments with each issue.
  When an issue has more, the rest are read from `GET issue/{id}/comment`, from
  the end of the list back to the stored newest comment. That read happens only
  when the issue changed since the last cycle: its comment count moved, or its
  `updated` time advanced, which Jira does when a comment is added. A snapshot
  stored without the newest comment counts the last N comments as new, N being
  the rise in the count.
- **Linear.** The batch read returns each issue's newest comment. When it
  changes, a follow-up query reads the comments created after the previous
  newest. A newest comment older than the previous one means the newest was
  deleted, and emits nothing.
- **GitHub.** The batch read returns each PR's comment count and newest
  comment, stored as `comment_count` and `comment_marker`. GitHub lists a PR's
  comments in the order they were made, so a new comment is always the newest.
  When the newest comment is newer than the marker, a follow-up query pages
  back from the end of the list to the marker, reading each comment's id,
  author and creation time, and its body for its mentions (see
  [Mentions](#mentions)) and nothing else. A newest comment older than the
  marker means the newest was deleted, and emits nothing. A snapshot without the marker that
  counted comments was stored before the marker existed, and counts the last N
  comments as new, N being the rise in the count. One that counted none may
  have been seeded from the REST listing, which carries no comments, so its
  first refresh stores the marker and emits nothing. The refresh gate skips an
  open PR whose `updatedAt` and head commit did not move for up to ten
  minutes, so a comment that did not advance `updatedAt` waits for that floor.

When a comment read fails, or reading which comments are TF's does, the
snapshot keeps its previous comment fields and the next cycle reports the same
comments. A rate limit ends the cycle's reads where it lands. On Linear the
whole cycle stops there. On GitHub the comment and review reads stop, with no
further request, and every PR not read is read next cycle, while the cycle
still diffs every PR the batch read returned.

#### Labels

`label_added` and `label_removed` exist for Jira and Linear issues as they do
for GitHub pull requests: one event per label added or removed since the last
cycle. `label_name` names the label and `labels` is the issue's whole set after
the change. Predicates can filter on `label_name`, `has_label`, `assignee_in`,
and `project` (Jira) or `linear_team_key` (Linear).

- **Jira** labels are plain strings with no id, and cannot be renamed, so they
  compare by name and the `dedup_key` is the name.
- **Linear** labels have ids and can be renamed, so they compare by id, the way
  workflow states do: a renamed label emits nothing, and the snapshot stores the
  new name. The `dedup_key` is the label's id, so a label renamed after it was
  added and then added again does not open a second task. A snapshot stored
  before label ids were recorded compares by name for its first refresh; a label
  it lost in that refresh is keyed by its name.
- A Linear label can sit in a group (a "Difficulty" group holding Easy, Medium
  and Hard), and its name is only its own, so two groups can each hold a
  "Medium". `label_group` names the group, empty for a label in none, and
  predicates can filter on it. Switching a single-select group from one value
  to another emits a `label_removed` and a `label_added` in the same cycle.
  The group is read from the team's label list, once per cycle and only when one
  of the team's issues has a label change, because selecting it on every issue
  would add to the batch read's complexity. When that read fails, the snapshot
  keeps its previous labels and the next cycle reports the change. Each read is
  a `tracker.linear.label_groups` span, with outcome `failed` or `rate_limited`
  when it did not succeed, so a read that fails every cycle (one the credential
  is not allowed to make, say) shows up there rather than only in a warning.

The labels an issue already has when TF first sees it are its baseline, like
every other field, and emit nothing. `assigned`, `available` and
`became_atomic` carry `labels` and take `has_label`, so a rule such as
"`available`, `has_label: tf-agent`" matches an issue that was labelled before
discovery. Label events are not suppressed on an issue with open subtasks or
sub-issues, as `assigned` and `available` are: someone added the label on
purpose. Removing a label does not close a `label_added` task.

#### Identity

An issue's key (`ENG-123`) is a display key: moving the issue to another
project gives it a new one, and renaming a project's key changes every key in
the project. Its numeric id never changes. So a Jira entity's `source_id` is
the key — what everything displays, searches and calls Jira with — and its
`external_id` is the id, which is what TF matches it on once it is known.
Entities are keyed within a `scope`, the org's Jira site (its base URL),
because ids and keys repeat across sites. Every Jira event's metadata carries
`issue_key`, `issue_id` and `project` — the key's project, which is what the
router's team gate reads.

When a tracked issue answers under a new key, the entity is renamed in the
same cycle: its `source_id` and `url`, and the target of every artifact
recorded against it, move to the new key, and it keeps its tasks,
conversations and memory. A Jira artifact is keyed on the issue's site and id,
so its key does not move, and an artifact on another site's issue with the
same id is a different row that the rename does not touch. An agent's write or
read naming an old key is answered by Jira under the issue's current key, so
it records against the issue's existing entity — renaming it, if TF had not
seen the move yet — and never creates a second one.

The refresh emits `key_changed` first, ahead of anything else it found (a move
to another project usually changes the issue's status too), in the same commit
as the new snapshot. Its metadata carries the usual identity fields for the
issue as it is now (`issue_key`, `issue_id`, `project`, `assignee`,
`assignee_account_id`, `issue_type`, `summary`) plus `old_issue_key` and
`old_project`, and predicates can filter it on `project` and `old_project`. It
is always the difference from the stored snapshot, so an entity with no
snapshot (one a source pause cleared) is renamed without it, the way it is
seeded without every other event. It is the one Jira event the team gate
passes for a team that tracks either the issue's current project or the one
it left, so a team still hears about an issue that moved to a project it does
not track. Its tasks stay on the entity after such a move, and every later
event goes only to the teams tracking the issue's new project.

A move is told from a project key rename by the project's id, which a rename
keeps: a key rename renames every entity in the project and retires none of
them. The teams' rules do not name the key at all. A team's tracked set
references the project's row in the `jira_projects` registry, keyed by Jira's
project id on the org's Jira site, and every Jira cycle reads each tracked
project by that id before discovery, a watched project as well as an armed
one, so a rename moves the registry row's key and nothing else: discovery
asks for the project by id, the rule lookups and the team gate match the
issues under the key they now carry, and the team's settings show that key. If no rule
configures the project an issue moved to, TF has nothing to follow it with:
the entity is renamed, `key_changed` is emitted, and it retires as
`unreachable` with reason `moved`. That event names the project the issue
left, which is the project whose teams were tracking it.

A closed issue that reopens is matched by its id too, so it reactivates its
original entity even after a move. The cycle that reactivates it renames it
and still emits `key_changed`, naming the key it was closed under.

A project deleted and recreated under the same key gives new issues the keys
of old ones. A new issue is skipped while the entity of the old issue that
held its key is still active, and gets an entity of its own once that one is
confirmed gone and retires; it never inherits the old issue's history.

An entity created before TF recorded Jira issue ids (every Jira entity of an
install upgraded from a version without them) carries no `external_id`, and
learns it from the first response that names its issue:

- An active entity is refreshed by key once, and takes the id from the answer.
  One whose issue moved before it learned its id gets no answer by key; it is
  asked about directly on its first miss, by key, and Jira's answer — under
  the issue's current key — gives it its id and renames it.
- A closed entity learns its id when discovery finds its issue under the key
  it was closed under. A closed entity whose issue moved before it learned its
  id is not reconnected: the issue's later activity gets an entity of its own,
  and the closed one keeps the history it already had.
- An issue left under two entities by a move nothing followed — one under the
  old key, one created under the new — is merged when the second one learns
  the id: the older entity survives, takes the newer one's live state, key and
  tasks, and of two active tasks for the same situation the newer entity's is
  dismissed.

#### Issue Unreachable

The one Jira event that doesn't come from the snapshot-diff, because there is no
new snapshot to diff: it reports that TF will no longer follow its own subject.
Like the terminal GitHub events it closes the entity and every task on it, so
what it takes to emit one is deliberately strict. Its `reason` says why:

- `not_found`: Jira will not resolve the issue, asked about directly.
- `moved`: the issue moved to a project no rule configures (see Identity
  above).
- `scope_changed`: the org's Jira base URL now names another site — or the
  same site under another URL. Every active issue from the previous site
  retires at the start of the next cycle, without Jira being asked: ids and
  keys repeat across sites, so nothing the new site answered would be about
  these issues. The rows are not moved or reused — an issue on the new site
  that happens to share an old key and id is a different issue and gets its
  own entity — and pointing the org at the old site again finds them by id.

**`not_found` does not mean the issue was definitely deleted.** Jira answers a
request for an issue you can't see exactly the way it answers one for an issue
that doesn't exist — a 404, deliberately, so that existence isn't disclosed.
Deletion is the usual cause, but a permission-scheme change or a
narrowed/rotated credential produce the identical answer, and nothing on our
side can tell them apart. A move does not: Jira answers a request for a moved
issue under its new key, which TF follows. If a `not_found` shows up for an
issue you can still see in the browser, check the credential's access before
concluding anything was deleted.

A tracked issue simply missing from a poll's search results is **not** enough to
emit it. An issue can drop out of a search while still perfectly readable — an
index that hasn't caught up, an archived issue or project — so absence only
starts a clock. Once an issue has gone unanswered for an hour, the poller asks
Jira about that one issue directly, by id, and emits this event **only** on a
404 from that request. An entity whose id TF has not learned yet is asked about
by key on its first miss instead, since that is how it learns the id. At most
20 issues are asked about per cycle; the rest wait for the next.

The other outcomes deliberately change nothing. An issue that resolves but never
appears in search results is logged as such and stays tracked — its entity is
being skipped by something other than unreachability, and closing it would
destroy live work. A confirmation that fails for any other reason is not evidence
either way, and is retried on a later cycle.

Metadata is the entity's last-known state (assignee, issue type, last status,
summary), since the source has nothing left to read, with the issue's current
key and id. Its `project` is the current key's, except on `moved`, where it is
the project the issue left. There is no `dedup_key` — an issue stops being
followed once.

## Linear Events

A Linear team is the tracked-set unit, the way a Jira project is. Each armed
team gets two discovery queries per cycle: unassigned issues in its pickup
states, and issues assigned to the credential's own user outside its done
states. Under an app install the credential's user is the app user, which
nothing is assigned to, so the second query returns nothing.

### Actionable

| Event | ID | Trigger | `dedup_key` |
|-------|----|---------|-------------|
| **Issue Assigned** | `linear:issue:assigned` | The assignee changes to someone, or an issue is first seen through the assigned-to-credential query | — |
| **Issue Available** | `linear:issue:available` | The assignee is cleared | — |
| **Priority Changed** | `linear:issue:priority_changed` | `priority` changes | new priority label |
| **New Comment** | `linear:issue:commented` | Someone other than TF commented since the last cycle. One event per cycle however many comments landed, naming every commenter — see [Comments](#comments) | — |
| **Issue Became Atomic** | `linear:issue:became_atomic` | The last open sub-issue closes, on an issue not itself done | — |
| **Label Added** | `linear:issue:label_added` | A label is added. Labels compare by id, so a label renamed in Linear is not a change — see [Labels](#labels) | label id |
| **Label Removed** | `linear:issue:label_removed` | A label is removed | label id |

### Informational

| Event | ID | Trigger | `dedup_key` |
|-------|----|---------|-------------|
| **Status Changed** | `linear:issue:status_changed` | The workflow state changes. A state renamed in Linear is not a change: states compare by id | new state name |
| **Issue Completed** | `linear:issue:completed` | The state enters one of the team's done states (fires beside `status_changed`) | — |
| **Issue Body Updated** | `linear:issue:body_updated` | The description changes, including clearing it | — |
| **Parent Changed** | `linear:issue:parent_changed` | The issue moves under another parent, or loses its parent | new parent's UUID, or `none` |
| **Identifier Changed** | `linear:issue:identifier_changed` | The issue answers under a new identifier: it moved to another team, or its team's key was renamed — see below | — |
| **Issue Unreachable** | `linear:issue:unreachable` | TF will no longer follow a tracked issue — see below | — |

An issue first seen through the pickup query is seeded quietly, as a Jira
issue is: discovery records it without an event, and later changes are diffed
against that seed. One first seen through the assigned-to-credential query
emits `assigned` with its seed, because being found by that query is itself
the assignment. An issue first seen already in a done state is closed without
an event.

An issue with open sub-issues is a container, not a unit of work: assignment
and unassignment emit no `assigned`/`available` while any sub-issue is open,
and `became_atomic` is the event that surfaces it once the last one closes.
A sub-issue is open when its state is not a done state of any armed team; a
sub-issue in a team nobody tracks therefore counts as open.

Every Linear event's metadata carries the issue's identity block —
`issue_identifier`, `issue_id`, `linear_team_id`, `linear_team_key`,
`assignee`, `assignee_user_id`, `creator`, `creator_user_id`, `title` — plus
the fields its event adds
(`old_status`/`new_status`, `old_priority`/`new_priority`,
`previous_body_hash`/`body_hash`, `old_parent`/`new_parent`, `final_status`,
`last_status`, `reason`, `comment_id`/`commenter`/`commenter_user_id`/
`commenter_user_ids`, `old_identifier`/`old_linear_team_id`/
`old_linear_team_key`, `label_name`/`label_group`, and `labels` on
`assigned`, `available`, `became_atomic`, `body_updated` and the label
events). `linear_team_id` is what the router's team gate reads
(with `old_linear_team_id` on `identifier_changed`, see below), and
`assignee_user_id` is what assignee-centric routing joins against a
member's bound Linear identity. `creator_user_id` is what `creator_in` matches
on `assigned`, `available` and `became_atomic`; both creator fields are empty
for an issue an integration created. `commenter_user_ids` is what
`commenter_in` matches on `commented` (see [Comments](#comments)).

#### Identity

An issue's identifier (`ENG-123`) is a display key: moving the issue to
another team gives it a new one, and renaming a team's key changes every
identifier in the team. Its UUID never changes. So a Linear entity's
`source_id` is the identifier — what everything displays — and its
`external_id` is the UUID, which is what TF matches it on. Entities are keyed
within a `scope`, the org's Linear workspace id, because identifiers repeat
across workspaces.

When a tracked issue answers under a new identifier, the entity is renamed in
the same cycle: its `source_id` and `url`, and the target of every artifact
recorded against it, move to the new identifier, and it keeps its tasks,
conversations and memory. A Linear artifact is keyed on the issue's UUID, so
its key does not move, and an artifact on another workspace's issue under the
same identifier is a different row that the rename does not touch.

The refresh emits `identifier_changed` first, ahead of anything else it found
(a move to another team usually changes the issue's workflow state too), in the
same commit as the new snapshot. Predicates can filter it on `linear_team_key`
and `old_linear_team_key`. It is always the difference from the stored
snapshot, so an entity with no snapshot (one a source pause cleared) is renamed
without it, the way it is seeded without every other event. It is the one Linear event the team gate passes for
a team that tracks either the issue's current Linear team or the one it left,
so a team still hears about an issue that moved to a team it does not track.
Its tasks stay on the entity after such a move, and every later event goes
only to the teams tracking the issue's new team.

If no rule arms the team the issue moved to, TF has nothing to follow it with:
the entity is renamed, `identifier_changed` is emitted, and it retires as
`unreachable` with reason `moved`. That event names the team the issue left,
which is the team that was tracking it. An issue that moved and was also
trashed or archived before a cycle saw it is handled the same way: renamed,
`identifier_changed`, then `unreachable` with reason `trashed` or `archived`.

A closed issue that reopens is matched by its UUID too, so it reactivates its
original entity even after a move. The cycle that reactivates it renames it
and still emits `identifier_changed`, naming the identifier and team it was
closed under.

A team key that is freed and reused can briefly name two issues: a new issue
gets `ENG-1` before TF has seen the old `ENG-1` move. The new one is skipped
until the old one's refresh renames it, and is picked up the cycle after.

#### Issue Unreachable

Like Jira's, this event closes the entity and every task on it, and it is
never inferred from an issue's absence. Its `reason` says why:

- `not_found`: Linear answers not-found for the issue. An issue missing from a
  batch read is asked about directly, by UUID, and only this answer retires
  it. Any other failure to read it is not evidence either way, and it is asked
  about again on a later cycle. At most 20 issues are asked about one at a
  time per cycle; the rest wait for the next.
- `trashed`: the issue is in the trash.
- `archived`: the issue is archived in a state outside its team's done states.
  Archived in a done state is the ordinary terminal path.
- `moved`: the issue moved to a team no rule arms (see Identity above).
- `scope_changed`: the org's Linear credential now belongs to another
  workspace. Every active issue from the previous workspace retires at the
  start of the next cycle, without Linear being asked: the credential cannot
  see the old workspace. The rows are not moved or reused — an issue in the
  new workspace that happens to share an old identifier is a different issue
  and gets its own entity — and binding the old workspace again finds them by
  UUID.

The event's metadata is the issue's last-known state. Its team comes from the
stored snapshot, or from Linear's answer when there is one, never from the
identifier's prefix. An entity with neither (no snapshot, and not found) names
no team: the event still closes the entity and its tasks, but the team gate
refuses it for every team, so no team's handlers receive it.

#### Team rules and the workspace

A team's Linear rules name team and workflow-state ids of the workspace they
were saved under. They apply only while the org's credential belongs to that
workspace: after a switch, the poller does not ask about them, the team's
settings do not list them, and the router's team gate does not read them. They
stay stored, though, and binding the old workspace again brings them back. A
save under the new workspace never touches them.

#### Rate limits

A request still rate limited after the client's 30 seconds of waiting ends the
org's cycle where it lands. Nothing further is sent to Linear that cycle, the
writes already made stand, and the poll-complete sentinel is not emitted. When
Linear said when the limit that refused the request resets, the org's next
cycle is scheduled for then instead of at its poll interval, and a settings
save does not bring it forward. That limit is the one the refusal shows with
nothing left, which can be an endpoint's own limit with a window of a minute;
a refusal that shows none spent is read as the later of the hourly request and
complexity limits.
A rate-limited request says nothing about the issues it asked for, so it never
leads to `unreachable`.

## Slack Events

Slack support is an Enterprise, multi-mode-only feature, configured per-org from **Settings → Slack** (operator setup: [self-hosting/slack.md](../self-hosting/slack.md)). Unlike GitHub and Jira, Slack events don't come from the snapshot-diff poller — they arrive over the app's Events API webhook or Socket Mode connection and are ingested as they happen.

| Event | ID | Trigger |
|-------|----|---------|
| **Message to bot** | `slack:message` | A human addressed the TF bot in a Slack channel — either an explicit @-mention, or a follow-up in a thread the bot already owns (an *engaged thread*) |

Mention-ness is metadata (`SlackMessageMetadata.Mentioned`), not a separate event type — the same taxonomy rule that only splits an event when the two cases are genuinely different situations. A handler can still narrow to explicit mentions with the predicate's `mentioned_only` flag, or to specific channels with `channel_in`, which names channels by their registry id (the `id` field of `GET /api/slack/teams/{team_id}/channels`), not by the ID Slack gives them.

A team's handlers only ever see messages from channels the team tracks, so an empty `channel_in` matches every one of them, and each entry must be a channel the handler's team tracks. Creating, editing or promoting a handler refuses any other entry, Slack's own channel ID included, with `422 INVALID_FIELD` on `scope_predicate`. An edit is checked only for the entries it adds, so a channel the team has since stopped tracking never blocks an edit that leaves it in place. The default trigger TF creates when a team first tracks a channel names no channels, so it fires on every channel the team tracks, including ones tracked later.

### Engaged threads

A Slack thread is **engaged** when the bot is the reason it exists:

- its root message @-mentioned the bot, or
- a delegated run posted the root message itself.

Engagement is encoded on the thread's entity as `kind="thread"` (contrast `kind="message"`, a mid-thread summons that @-mentions the bot inside a thread someone else started). Closing the entity ends engagement.

**Every human message in an engaged thread is ingested** and published as `slack:message` (with `mentioned=false`), so a follow-up no longer has to re-@-mention the bot to be heard — when the mention *started* the thread, requiring a second @ was confusing. Explicit @-mentions keep working everywhere, engaged or not (published with `mentioned=true`).

Follow-up ingestion (the un-mentioned `message.channels` / `message.groups` deliveries) is deliberately narrow — the vast majority of channel traffic is dropped before it's even recorded. A follow-up publishes only when all of these hold:

- it's a plain reply or a reply also broadcast to the channel (subtype `""` or `thread_broadcast`) — edits, deletions, joins, and bot messages are dropped;
- it's inside a thread (has a `thread_ts`) — root-channel chatter is never ingested;
- it wasn't authored by the bot itself or any other bot;
- it doesn't explicitly @-mention the bot — that copy is owned by the twin `app_mention` delivery, so dropping it here avoids a double publish;
- the thread's entity already exists, is `kind="thread"`, and is still active.

### Channel ID changes

TF registers every channel it sees or a team tracks, and everything that refers to a channel holds that registry row: each team's tracking of it, each thread's entity, and the `channel_in` filter of every `slack:message` handler. A thread's entity is keyed by its channel's ID and its root message's timestamp. Slack gives a private channel a new ID when it's shared with another organization through Slack Connect (`G…` becomes `C…`), announces it with `channel_id_changed`, and uses the new ID for everything after. TF follows the change in one transaction: the registry row takes the new ID, and so do the values that spell it — the thread entities' keys and permalinks, and the Slack artifacts and audit-log links that name it. Tracking and handler filters need nothing. It emits no event.

TF also records the change, because event metadata is immutable: a message recorded before the share names the old ID for good. Everything that acts on a recorded message reads its channel as the current ID — handler filters, the team gate and owner resolution, a run's task context and injected follow-ups, the exec verbs, and the bot's own reactions, replies and working indicator — and so does ingest, for a message Slack generated before the change but delivered after it.

Slack doesn't order the change against other deliveries. A message made under the new ID that arrives before the change has no thread to land in yet: an un-mentioned follow-up is dropped, and a mention registers the new ID as a channel of its own and starts a new thread entity. When the change arrives, that registry row is merged into the channel's — its tracking, threads and handler filters move onto the channel — the thread's original entity takes the key, and the one the mention started is closed beside it, keeping its own task and conversation. A change Slack delivers again, or delivers after a later change to the same channel, leaves the channel on its latest ID.

## System Events

These are internal signals, not shown in the triage UI.

| Event | ID | Trigger |
|-------|----|---------|
| **Poll Complete** | `system:poll:completed` | A tracker refresh cycle finished and processed items. For GitHub, this fires only once a cycle fully wraps its round-robin repo cursor — a cycle interrupted by a rate-limit budget exhaustion saves its resume point and stays silent, so scoring/classification/profiling don't churn on a still-partial cold-start sync |
| **Scoring Complete** | `system:scoring:completed` | AI scoring finished for a batch of tasks |
| **Delegation Complete** | `system:delegation:completed` | An agent delegation run completed successfully |
| **Delegation Failed** | `system:delegation:failed` | An agent delegation run failed |
| **Task Auto-suspended** *(deprecated)* | `system:task:auto_suspended` | Per-task breaker trip; superseded by the per-(entity, prompt) breaker below and no longer emitted |
| **Prompt Auto-suspended** | `system:prompt:auto_suspended` | The per-(entity, prompt) breaker tripped after repeated run failures |
| **Delegation Blocked: Subtasks** | `system:task:delegation_blocked_by_subtasks` | Auto-delegation was skipped for a Jira issue because its parent has open subtasks |
| **Conversation Status** | `system:conversation:status` | A delegated run's status changed (mirrors the `conversation_update` websocket event) |
| **Conversation Activity** | `system:conversation:activity` | A delegated run invoked a tool (mirrors the `message` websocket event, `tool_use` messages only) |
| **Routing Disposition** | `system:routing:disposition` | `Router.HandleEvent` finished handling one event — frozen, source turned off by an org admin, taskless (no handler/owner/unroutable), task created/bumped, or an internal error. Lets an async event source (e.g. Slack) learn synchronously-unavailable routing outcomes |

## Snapshot fields

### GitHub PR Snapshot

The entity itself carries `source_id` (`owner/repo#N`), `scope` (the org's
GitHub host) and `repository_id`, the registry row of its repository. That row
is created with the entity when TF holds none for the repository, so a pull
request in a repository no team tracks has one too, and a repository rename
leaves the reference in place while it rewrites `source_id`.

The tracker stores these fields for each PR and diffs them between cycles:

- `number`, `title`, `author`, `repo`, `head_repo`, `url`
- `state` (OPEN, CLOSED, MERGED), `is_draft`, `merged`, `mergeable` (MERGEABLE, CONFLICTING, UNKNOWN)
- `head_ref`, `base_ref`, `head_sha`
- `additions`, `deletions`, `changed_files`
- `check_runs[]` — structured per-check-run data for the current head SHA, deduped by name (latest execution wins). Each entry: `id`, `name`, `status`, `conclusion`, `completed_at`, `details_url`, `workflow_run_id`. `details_url` is GitHub's `details_url` — the CI provider's "more info" link (for Actions: `/actions/runs/N/job/M`; for third-party CI: provider-defined), not GitHub's narrower check-run page URL.
- `review_requests[]` — pending reviewer identifiers: user logins for direct requests, `org/slug` for team requests
- `reviews[]` — latest review per reviewer: `id` (node id), `database_id`, `author`, `state`, `submitted_at`, `comment_count`
- `review_count` — the number of submitted reviews
- `review_marker` — the newest submitted review TF has read, `{id, submitted_at}`; see [Reviews](#reviews)
- `labels[]`, `updated_at`
- `body_hash` — fingerprint of the complete body; the body itself is mirrored onto the entity, capped at 2,000 codepoints
- `body_mentions[]` — the users (`login`) and teams (`org/slug`) the body @-mentions, in lower case; see [Mentions](#mentions). `null` on a snapshot stored before it existed
- `comment_count` — the number of conversation comments
- `comment_marker` — the newest conversation comment TF has read, `{id, created_at}`; see [Comments](#comments)

`author` and each review's `author` are logins as GitHub's REST API spells them: a bot is `<slug>[bot]` (`dependabot[bot]`), the form event metadata carries as `author` and `reviewer` and the form a handler predicate such as `author_in` must use. GraphQL returns a bot's login without the suffix; the snapshot builder adds it.

Every GitHub event's metadata names its repository twice: `repo`, the `owner/name` it had when the event fired, and `repository_id`, the repository's registry row, which a rename does not move. A handler predicate scopes to repositories with `repo_in`, which names them by registry id (the `id` field of `POST /api/repos/list` with `team_id`), not by `owner/name`. A team's handlers only ever see events from repositories the team tracks — the router's team gate asks about the registry row the event's pull request references, never the name in its key — so an empty `repo_in` matches every one of them, and each entry must be a repository the handler's team tracks. Creating, editing or promoting a handler refuses any other entry, an `owner/name` included, with `422 INVALID_FIELD` on `scope_predicate`. An edit is checked only for the entries it adds, so a repository the team has since stopped tracking never blocks an edit that leaves it in place.

### Jira Issue Snapshot

The entity itself carries `source_id` (the key), `external_id` (the issue id,
which it is matched on once known) and `scope` (the org's Jira site).

- `id` (issue id), `key` (the entity's `source_id`), `project_id`, `summary`, `url`
- `body_hash` — fingerprint of the complete description; the description itself is mirrored onto the entity, capped at 2,000 codepoints
- `status`, `status_id` — compared by id
- `assignee` (display name), `assignee_account_id`, `priority`
- `reporter` (display name), `reporter_account_id` — the account id is derived as the assignee's is (Cloud `accountId`, else Data Center `key`, else `name`). Carried on `assigned`, `available` and `became_atomic`, where `reporter_in` matches it. A reporter change emits nothing
- `labels[]` — compared by name for `label_added` / `label_removed`
- `issue_type`, `parent_key`
- `comment_count`, `last_comment_id`, `last_comment_at` — the newest comment; empty while it is past the page the batch read returns and has not been read
- `open_subtask_count` — subtasks not in a configured done status
- `created_at`, `updated_at`

`id` and `project_id` are empty on a snapshot stored before they were recorded,
until its next refresh.

### Linear Issue Snapshot

The entity itself carries `source_id` (the identifier), `external_id` (the
UUID, which it is matched on), `scope` (the org's Linear workspace id) and
`linear_team_id` (the UUID of the Linear team the issue sits in). The team is
set when the entity is created, from the response that found the issue, and
every snapshot write moves it to the snapshot's `team_id`, so it follows a
move to another team.

- `id` (UUID), `identifier` (the entity's `source_id`), `title`, `url`
- `body_hash` — fingerprint of the raw markdown description; the description itself is mirrored onto the entity, capped at 2,000 codepoints
- `state` — `{id, name, type}`; compared by id
- `assignee` (display name), `assignee_user_id`
- `creator` (display name), `creator_user_id` — empty for an issue an integration created. Linear does not let the creator change, so it is never diffed
- `priority` (0 = none, 1 = urgent … 4 = low), `priority_label`
- `labels[]` — label names, which predicates and `body_updated` read
- `label_refs[]` — `{id, name}` for each label; the label diff compares by id. Absent on a snapshot stored before it existed, and for an issue with no labels
- `team_id`, `team_key`
- `parent_id`, `parent_identifier`
- `last_comment_id`, `last_comment_at`
- `open_child_count` — sub-issues not in an armed team's done states
- `created_at`, `updated_at`, `archived`, `trashed`

Turning Linear off for an org clears these snapshots, as it does for any
source, and leaves the entity's `linear_team_id`. Everything that needs an
issue's team reads that column: the factory belt and team activity, the claim
and requeue rules, the terminal-state checker, and the tracker, which takes an
unreachable issue's `linear_team_id` from it and compares it with a fresh read
to tell a move. So while Linear is off its issues stay on their teams' belts,
and one that goes unreachable still reaches its team's handlers. On the first
cycle after Linear is turned back on, an entity with no snapshot is read by
UUID in the same batches as the rest and seeded without events.

## Event lifecycle

1. **First seen** — when an item is first discovered, an initial event is emitted based on its current state (e.g. `review_requested` if the PR has pending review requests, and `mentioned` for the mentions in the body of a PR created since TF last listed its repository)
2. **Transitions** — subsequent cycles compare snapshots and emit events for any field changes
3. **Terminal** — when an item reaches a terminal state (merged, closed, done), it's marked with `terminal_at` and excluded from future refresh cycles. Terminal items are retained indefinitely for dashboard statistics.
4. **Reactivation** — if a terminal item reappears in discovery (e.g. a closed PR is reopened), `terminal_at` is cleared and tracking resumes

## Configuration

Events can be enabled/disabled on the Event Types settings page. Disabling an event type hides it from the triage queue but does not stop the tracker from detecting it — events are still recorded and can trigger delegation rules.
