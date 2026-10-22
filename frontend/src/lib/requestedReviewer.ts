import type { Task } from '../types'

// With per-reviewer dedup a single PR mints one
// `github:pr:review_requested` task per requested reviewer, distinguished
// only by dedup_key. These helpers read *who* the review was requested of
// out of that key and derive a GitHub link, so RequestedReviewerBadge can
// surface the queue's most relevant routing signal (me personally vs. a
// team I'm on). Kept out of the component file so a plain function export
// doesn't trip react-refresh's only-export-components rule.

export type RequestedReviewer =
  | { kind: 'user'; login: string }
  | { kind: 'team'; org: string; slug: string }

// parseRequestedReviewer reads the requested reviewer off a task. The
// dedup_key says whether the request named a user or a team; its format is
// owned by the events package (internal/domain/events/github.go:
// ReviewerDedupKey*):
//   user_id:<id>           a user, by GitHub's id
//   team_id:<id>           a team, by GitHub's id
//   user:<login>           a user read without an id, e.g. "user:aidanallchin"
//   team:<org>/<slug>      a team read without an id, e.g. "team:eng/platform"
// The handle shown is the task's requested_reviewer, the login or org/slug the
// request named, and for a key by handle the key's own when the server sent
// none. Returns null for any non-conforming key — non-review_requested events,
// and legacy tasks whose dedup_key is empty — so the badge simply never
// renders where it doesn't apply.
export function parseRequestedReviewer(task: Task): RequestedReviewer | null {
  if (task.event_type !== 'github:pr:review_requested') return null
  if (!task.dedup_key) return null
  const key = task.dedup_key
  let kind: RequestedReviewer['kind']
  let handle = task.requested_reviewer ?? ''
  if (key.startsWith('user_id:')) {
    kind = 'user'
  } else if (key.startsWith('team_id:')) {
    kind = 'team'
  } else if (key.startsWith('user:')) {
    kind = 'user'
    handle ||= key.slice('user:'.length)
  } else if (key.startsWith('team:')) {
    kind = 'team'
    handle ||= key.slice('team:'.length)
  } else {
    return null
  }
  if (kind === 'user') return handle ? { kind, login: handle } : null
  const slashAt = handle.indexOf('/')
  if (slashAt === -1) return null
  const org = handle.slice(0, slashAt)
  const slug = handle.slice(slashAt + 1)
  if (!org || !slug) return null
  return { kind, org, slug }
}

// reviewerProfileURL derives the GitHub link from the task's own
// source_url (the PR URL, e.g. https://github.example.com/owner/repo/pull/1)
// rather than fetching org settings — the host is the same one the PR
// lives on, and using the URL as the source of truth keeps the badge
// correct even across hosts. Returns null when source_url won't parse.
export function reviewerProfileURL(sourceURL: string, r: RequestedReviewer): string | null {
  try {
    const u = new URL(sourceURL)
    if (r.kind === 'user') return `${u.origin}/${r.login}`
    return `${u.origin}/orgs/${r.org}/teams/${r.slug}`
  } catch {
    return null
  }
}
