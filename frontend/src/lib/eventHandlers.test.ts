import { describe, it, expect } from 'vitest'
import { detachPatch } from './eventHandlers'

describe('detachPatch', () => {
  it('clears the blueprint and moves a handler in automate to watch', () => {
    expect(detachPatch({ mode: 'automate' })).toEqual({ blueprint_id: null, mode: 'watch' })
  })

  it('clears only the blueprint of a handler in any other mode', () => {
    for (const mode of ['off', 'notify', 'watch'] as const) {
      expect(detachPatch({ mode })).toEqual({ blueprint_id: null })
    }
  })
})
