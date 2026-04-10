# Tracked Events

Todo Tinder monitors GitHub PRs and Jira issues for state changes and emits typed events when transitions are detected. Events power the triage queue, AI scoring, delegation triggers, and the dashboard.

## How it works

The tracker runs on a configurable poll interval (default: 60s). Each cycle:

1. **Discover** — search queries find new items to track
2. **Register** — new items are stored in `tracked_items` with an initial snapshot
3. **Refresh** — all tracked items are batch-fetched (GitHub via GraphQL `nodes(ids:[...])`, Jira via `key IN (...)` JQL)
4. **Diff** — current snapshot is compared against the previous snapshot
5. **Emit** — typed events are recorded in the `events` table and published to the event bus

Events are emitted once per transition, not continuously. If a PR stays in the same state across multiple cycles, no events fire.

## GitHub PR Events

### Actionable (shown in triage queue by default)

| Event | ID | Trigger |
|-------|----|---------|
| **Changes Requested** | `github:pr:changes_requested` | A reviewer's latest review state changes to `CHANGES_REQUESTED` |
| **CI Failed** | `github:pr:ci_failed` | The head commit's `statusCheckRollup` transitions to `FAILURE` or `ERROR` |
| **Review Requested** | `github:pr:review_requested` | A user/team appears in the PR's `reviewRequests` that wasn't there before. Detects both initial requests and re-requests after changes |
| **Merge Conflicts** | `github:pr:conflicts` | The PR's `mergeable` state transitions to `CONFLICTING` |
| **Ready for Review** | `github:pr:ready_for_review` | The PR's `isDraft` changes from `true` to `false` |
| **PR Approved** | `github:pr:approved` | A reviewer's latest review state changes to `APPROVED` |
| **Review Received** | `github:pr:review_received` | A reviewer's latest review state changes to `COMMENTED` or `DISMISSED` |
| **Mentioned** | `github:pr:mentioned` | PR discovered via `mentions:{user}` search. Note: new @mentions on an already-tracked PR cannot be detected without parsing comment bodies |

### Informational (hidden by default, toggleable)

| Event | ID | Trigger |
|-------|----|---------|
| **CI Passed** | `github:pr:ci_passed` | The head commit's `statusCheckRollup` transitions to `SUCCESS` |
| **Authored PR** | `github:pr:opened` | First time an authored PR is discovered |
| **PR Merged** | `github:pr:merged` | The PR's `merged` field changes to `true` |

## Jira Events

### Actionable

| Event | ID | Trigger |
|-------|----|---------|
| **Issue Assigned** | `jira:issue:assigned` | The `assignee` field changes to a non-empty value |
| **Issue Available** | `jira:issue:available` | An unassigned issue appears in the pickup queue, or an assigned issue becomes unassigned |
| **Priority Changed** | `jira:issue:priority_changed` | The `priority` field changes |
| **New Comment** | `jira:issue:commented` | The `comment.total` count increases (fires once per cycle regardless of how many comments were added) |

### Informational

| Event | ID | Trigger |
|-------|----|---------|
| **Status Changed** | `jira:issue:status_changed` | The `status` field changes (e.g. To Do → In Progress) |
| **Issue Completed** | `jira:issue:completed` | The `status` changes to Done, Closed, or Resolved |

## System Events

These are internal signals, not shown in the triage UI.

| Event | ID | Trigger |
|-------|----|---------|
| **Poll Complete** | `system:poll:completed` | A tracker refresh cycle finished and processed items |
| **Scoring Complete** | `system:scoring:completed` | AI scoring finished for a batch of tasks |
| **Delegation Complete** | `system:delegation:completed` | An agent delegation run completed successfully |
| **Delegation Failed** | `system:delegation:failed` | An agent delegation run failed |

## Snapshot fields

### GitHub PR Snapshot

The tracker stores these fields for each PR and diffs them between cycles:

- `number`, `title`, `author`, `repo`, `head_repo`, `url`
- `state` (OPEN, CLOSED, MERGED), `is_draft`, `merged`, `mergeable` (MERGEABLE, CONFLICTING, UNKNOWN)
- `head_ref`, `base_ref`, `head_sha`
- `additions`, `deletions`, `changed_files`
- `ci_state` (SUCCESS, FAILURE, PENDING, ERROR)
- `review_requests[]` — logins of users/teams with pending review requests
- `reviews[]` — latest review per reviewer (author, state, submitted_at)
- `review_count` — total number of reviews submitted
- `labels[]`, `comment_count`, `updated_at`

### Jira Issue Snapshot

- `key`, `summary`, `url`
- `status`, `assignee`, `priority`
- `labels[]`, `issue_type`, `parent_key`
- `comment_count`

## Event lifecycle

1. **First seen** — when an item is first discovered, an initial event is emitted based on its current state (e.g. `review_requested` if the PR has pending review requests, `opened` if it's an authored PR)
2. **Transitions** — subsequent cycles compare snapshots and emit events for any field changes
3. **Terminal** — when an item reaches a terminal state (merged, closed, done), it's marked with `terminal_at` and excluded from future refresh cycles
4. **Pruning** — terminal items are deleted from `tracked_items` after 30 days

## Configuration

Events can be enabled/disabled on the Event Types settings page. Disabling an event type hides it from the triage queue but does not stop the tracker from detecting it — events are still recorded and can trigger delegation rules.
