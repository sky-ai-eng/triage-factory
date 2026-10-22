import { describe, expect, it } from 'vitest'

import type { Task } from '../types'
import {
  parseRequestedReviewer,
  reviewerProfileURL,
  type RequestedReviewer,
} from './requestedReviewer'

const task = (dedup_key: string, requested_reviewer?: string): Task =>
  ({
    event_type: 'github:pr:review_requested',
    dedup_key,
    requested_reviewer,
  }) as Task

const alice: RequestedReviewer = { kind: 'user', login: 'alice' }
const backend: RequestedReviewer = { kind: 'team', org: 'acme', slug: 'backend' }

describe('parseRequestedReviewer', () => {
  it.each([
    // A key by GitHub id is named by the handle the server sends, or not at all.
    ['user_id:5', 'alice', alice],
    ['team_id:11', 'acme/backend', backend],
    ['user_id:5', undefined, null],
    ['team_id:11', 'not-a-team', null],
    // A key by handle reads its own when the server sends none.
    ['user:alice', undefined, alice],
    ['team:acme/backend', undefined, backend],
    ['build', undefined, null],
    ['', undefined, null],
  ])('reads %j with handle %j', (key, handle, want) => {
    expect(parseRequestedReviewer(task(key, handle))).toEqual(want)
  })

  it('ignores other event types', () => {
    const mentioned = { ...task('user:alice'), event_type: 'github:pr:mentioned' }
    expect(parseRequestedReviewer(mentioned)).toBeNull()
  })
})

describe('reviewerProfileURL', () => {
  it('links a user and a team on the pull request host', () => {
    const url = 'https://github.example.com/owner/repo/pull/1'
    expect(reviewerProfileURL(url, alice)).toBe('https://github.example.com/alice')
    expect(reviewerProfileURL(url, backend)).toBe(
      'https://github.example.com/orgs/acme/teams/backend',
    )
  })
})
