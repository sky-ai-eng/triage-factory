import type { Task } from '../types'

// SKY-412: with per-reviewer dedup (SKY-370) a single PR mints one
// `github:pr:review_requested` task per requested reviewer, distinguished
// only by dedup_key. These helpers read *who* the review was requested of
// out of that key and derive a GitHub link, so RequestedReviewerBadge can
// surface the queue's most relevant routing signal (me personally vs. a
// team I'm on). Kept out of the component file so a plain function export
// doesn't trip react-refresh's only-export-components rule.

export type RequestedReviewer =
  | { kind: 'user'; login: string }
  | { kind: 'team'; org: string; slug: string }

// parseRequestedReviewer reads the requested reviewer out of a task's
// dedup_key. The key format is owned by the events package
// (internal/domain/events/github.go: ReviewerDedupKeyUser/Team):
//   user:<login>           e.g. "user:aidanallchin"
//   team:<org>/<slug>      e.g. "team:eng/platform"
// Returns null for any non-conforming key — non-review_requested events,
// and legacy tasks predating SKY-370 whose dedup_key is empty — so the
// badge simply never renders where it doesn't apply.
export function parseRequestedReviewer(task: Task): RequestedReviewer | null {
  if (task.event_type !== 'github:pr:review_requested') return null
  if (!task.dedup_key) return null
  if (task.dedup_key.startsWith('user:')) {
    const login = task.dedup_key.slice('user:'.length)
    if (!login) return null
    return { kind: 'user', login }
  }
  if (task.dedup_key.startsWith('team:')) {
    const rest = task.dedup_key.slice('team:'.length)
    const slashAt = rest.indexOf('/')
    if (slashAt === -1) return null
    const org = rest.slice(0, slashAt)
    const slug = rest.slice(slashAt + 1)
    if (!org || !slug) return null
    return { kind: 'team', org, slug }
  }
  return null
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
