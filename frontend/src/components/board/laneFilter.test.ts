import { describe, it, expect } from 'vitest'
import { emptyLaneFilter, filterIsActive, laneFilterFields } from './laneFilter'
import { facetsBody, queueListBody } from '../../lib/taskList'

describe('lane kind filter', () => {
  it('sends a kind as entity_kinds, on the list and its facets alike', () => {
    const f = { ...emptyLaneFilter, kind: 'vulnerable_package' as const }
    expect(filterIsActive(f)).toBe(true)
    const fields = laneFilterFields(f)
    expect(fields.entity_kinds).toEqual(['vulnerable_package'])
    expect(facetsBody(queueListBody([], false, fields)).entity_kinds).toEqual([
      'vulnerable_package',
    ])
  })

  it('sends no entity_kinds for every kind', () => {
    expect(laneFilterFields(emptyLaneFilter)).not.toHaveProperty('entity_kinds')
    expect(filterIsActive(emptyLaneFilter)).toBe(false)
  })
})
