import { describe, expect, it } from 'vitest'

import type { Task } from '../types'
import { parseRequestedReviewer, reviewerProfileURL } from './requestedReviewer'

const task = (dedup_key: string, requested_reviewer?: string): Task =>
  ({
    event_type: 'github:pr:review_requested',
    dedup_key,
    requested_reviewer,
  }) as Task

describe('parseRequestedReviewer', () => {
  it('names a reviewer keyed by GitHub id by the handle the server sends', () => {
    expect(parseRequestedReviewer(task('user_id:5', 'alice'))).toEqual({
      kind: 'user',
      login: 'alice',
    })
    expect(parseRequestedReviewer(task('team_id:11', 'acme/backend'))).toEqual({
      kind: 'team',
      org: 'acme',
      slug: 'backend',
    })
  })

  it('renders nothing for a key by GitHub id with no handle to show', () => {
    expect(parseRequestedReviewer(task('user_id:5'))).toBeNull()
    expect(parseRequestedReviewer(task('team_id:11', 'not-a-team'))).toBeNull()
  })

  it('reads a key by handle when the server sends none', () => {
    expect(parseRequestedReviewer(task('user:aidanallchin'))).toEqual({
      kind: 'user',
      login: 'aidanallchin',
    })
    expect(parseRequestedReviewer(task('team:eng/platform'))).toEqual({
      kind: 'team',
      org: 'eng',
      slug: 'platform',
    })
  })

  it('ignores other event types and keys', () => {
    expect(
      parseRequestedReviewer({ ...task('user:a'), event_type: 'github:pr:mentioned' }),
    ).toBeNull()
    expect(parseRequestedReviewer(task('build'))).toBeNull()
    expect(parseRequestedReviewer(task(''))).toBeNull()
  })
})

describe('reviewerProfileURL', () => {
  it('links a user and a team on the pull request host', () => {
    const url = 'https://github.example.com/owner/repo/pull/1'
    expect(reviewerProfileURL(url, { kind: 'user', login: 'alice' })).toBe(
      'https://github.example.com/alice',
    )
    expect(reviewerProfileURL(url, { kind: 'team', org: 'acme', slug: 'backend' })).toBe(
      'https://github.example.com/orgs/acme/teams/backend',
    )
  })
})
