import { describe, it, expect } from 'vitest'
import { approvalAction, approvalKicker } from './approval'

// The kicker/action pair is shared by the board card's attention row and the
// run-station dock, so the copy is pinned here once for both surfaces.

describe('approvalKicker', () => {
  it('keeps the per-kind breakdown for a mixed set', () => {
    expect(approvalKicker({ pr: 2, review: 1, total: 3 })).toBe('2 PRs · 1 review ready')
    expect(approvalKicker({ pr: 1, review: 2, total: 3 })).toBe('1 PR · 2 reviews ready')
  })

  it('names a single-kind set by its kind', () => {
    expect(approvalKicker({ pr: 1, review: 0, total: 1 })).toBe('PR ready to open')
    expect(approvalKicker({ pr: 3, review: 0, total: 3 })).toBe('3 PRs ready to open')
    expect(approvalKicker({ pr: 0, review: 1, total: 1 })).toBe('Review ready')
    expect(approvalKicker({ pr: 0, review: 2, total: 2 })).toBe('2 reviews ready')
  })

  it('degrades to the bare lead-in when the counts are absent', () => {
    // The transient window: has_unresolved_artifacts true, counts omitted.
    expect(approvalKicker({ pr: 0, review: 0, total: 0 })).toBe('Awaiting approval')
  })
})

describe('approvalAction', () => {
  it('keeps the direct verb for a single item and the chooser verb for a set', () => {
    expect(approvalAction({ pr: 1, review: 0, total: 1 })).toBe('Open PR')
    expect(approvalAction({ pr: 0, review: 1, total: 1 })).toBe('Review')
    expect(approvalAction({ pr: 1, review: 1, total: 2 })).toBe('Review 2 items')
    expect(approvalAction({ pr: 0, review: 0, total: 0 })).toBe('Review')
  })
})
