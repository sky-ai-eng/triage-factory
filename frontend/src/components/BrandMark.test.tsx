import { describe, it, expect } from 'vitest'
import { render } from '@testing-library/react'
import BrandMark from './BrandMark'

// Both assertions here pin decisions that would otherwise break silently — the
// mark would still draw, just wrongly, and only in the theme or with the
// assistive tech nobody had open.
describe('BrandMark', () => {
  it('draws in the inherited ink rather than a fixed colour', () => {
    const { container } = render(<BrandMark />)
    const svg = container.querySelector('svg')!
    // A literal hex here would be a mark that vanishes into one of the two
    // grounds: the reference cuts are exactly --color-ink-1's light and dark
    // values, so currentColor is what reproduces both without a theme branch.
    expect(svg.getAttribute('stroke')).toBe('currentColor')
  })

  it('is decorative, so it adds no second announcement of the name', () => {
    const { container } = render(<BrandMark />)
    // Every placement sets the mark beside the product name in text. Without
    // this a screen reader reads the lockup as the name twice.
    expect(container.querySelector('svg')!.getAttribute('aria-hidden')).toBe('true')
  })

  it('scales the box and leaves the stroke weight to it', () => {
    const { container } = render(<BrandMark size={48} />)
    const svg = container.querySelector('svg')!
    expect(svg.getAttribute('width')).toBe('48')
    expect(svg.getAttribute('height')).toBe('48')
    // The viewBox is what keeps the weight proportional; a size that changed
    // stroke-width instead would thicken the mark as it grew.
    expect(svg.getAttribute('viewBox')).toBe('0 0 16 16')
    expect(svg.getAttribute('stroke-width')).toBe('2')
  })
})
