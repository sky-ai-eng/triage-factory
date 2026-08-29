import { describe, it, expect } from 'vitest'
import { allocate, tones } from './convergeParts'

// The allocation is the honesty mechanism: strands are structure, not data,
// and the square-root scale is what keeps a healthy (mostly-filtered) day a
// picture instead of one combed bundle. These pins are the reason it can't
// quietly regress to proportional.

describe('allocate', () => {
  it('keeps a picture on the 95%-filtered day proportional allocation destroys', () => {
    // The design doc's own example figures: merged 6, running 4, need-you 7,
    // filtered 296, over 28 strands. Proportional hands filtered 22 of 28;
    // the sqrt scale lands on a fan a reader can still see into.
    const n = allocate([{ v: 6 }, { v: 4 }, { v: 7 }, { v: 296 }], 28)
    expect(n).toEqual([3, 2, 3, 20])
    expect(n.reduce((a, b) => a + b, 0)).toBe(28)
  })

  it('floors a small outcome at two strands so it reads as a bundle', () => {
    const n = allocate([{ v: 0 }, { v: 1 }, { v: 400 }], 28)
    expect(n[0]).toBeGreaterThanOrEqual(2)
    expect(n[1]).toBeGreaterThanOrEqual(2)
    expect(n.reduce((a, b) => a + b, 0)).toBe(28)
  })

  it('always spends exactly the strand budget', () => {
    for (const outcomes of [
      [{ v: 1 }, { v: 1 }, { v: 1 }, { v: 1 }],
      [{ v: 100 }, { v: 100 }],
      [{ v: 0 }, { v: 0 }, { v: 0 }, { v: 5 }],
    ]) {
      const n = allocate(outcomes, 28)
      expect(n.reduce((a, b) => a + b, 0)).toBe(28)
    }
  })
})

describe('tones', () => {
  it('maps the vocabulary to the stylesheet letters, quiet by default', () => {
    expect(tones('ask')).toBe('a')
    expect(tones('cool')).toBe('c')
    expect(tones('warm')).toBe('w')
    expect(tones('quiet')).toBe('q')
    expect(tones(undefined)).toBe('q')
  })
})
