import { describe, it, expect } from 'vitest'
import { stepsVersion } from './blueprintWrites'

const step = (index: number, version: number) => ({
  blueprint_id: 'x',
  step_index: index,
  step_prompt_id: 'p' + index,
  brief: '',
  blueprint_version: version,
})

describe('stepsVersion', () => {
  it('names the version every step was read at', () => {
    expect(stepsVersion([step(0, 4), step(1, 4)])).toBe(4)
  })

  it('names the older version when the steps came from two reads', () => {
    // A page boundary fell inside the blueprint and a write landed between
    // the pages: the older version is the one the server refuses.
    expect(stepsVersion([step(0, 4), step(1, 5)])).toBe(4)
  })
})
