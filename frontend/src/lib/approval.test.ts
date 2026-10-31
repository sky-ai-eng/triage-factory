import { describe, it, expect } from 'vitest'
import { approvalAction } from './approval'

describe('approvalAction', () => {
  it('keeps the direct verb for a single item and the chooser verb for a set', () => {
    expect(approvalAction({ pr: 1, review: 0, total: 1 })).toBe('Open PR')
    expect(approvalAction({ pr: 0, review: 1, total: 1 })).toBe('Review')
    expect(approvalAction({ pr: 1, review: 1, total: 2 })).toBe('Review 2 items')
    expect(approvalAction({ pr: 0, review: 0, total: 0 })).toBe('Review')
  })
})
