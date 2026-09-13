import { describe, it, expect } from 'vitest'
import { render } from '@testing-library/react'
import { CrateMark } from './CrateMark'

// The mark is drawn, not described: these pin the parts the cycle depends on
// — the pallet, eight crates each trailed by its own flash copy in painter
// order, and the state attribute the idle and reduced-motion stills key on.

describe('CrateMark', () => {
  it('draws the pallet and eight crates, each trailed by its own flash', () => {
    const { container } = render(<CrateMark />)
    const svg = container.querySelector('svg.tf-crates') as SVGSVGElement
    expect(svg.getAttribute('viewBox')).toBe('-11 -13.7 22 21')
    expect(svg.getAttribute('width')).toBe('20')
    expect(svg.getAttribute('height')).toBe('19')
    expect(svg.querySelector('path[stroke-dasharray="3 3"]')).not.toBeNull()
    const crates = svg.querySelectorAll('g.cm-crate')
    const flashes = svg.querySelectorAll('g.cm-flash')
    expect(crates).toHaveLength(8)
    expect(flashes).toHaveLength(8)
    // Painter order: a crate, then its flash, back to front — a flash belongs
    // to one layer, so the crate standing on the back cell covers that cell's
    // light and the front crate covers the standing crate.
    const groups = Array.from(svg.querySelectorAll(':scope > g'))
    for (const g of groups) {
      expect(g.children[0].classList.contains('cm-crate')).toBe(true)
      expect(g.children[1].classList.contains('cm-flash')).toBe(true)
    }
    // The three rebuild crates are the ones the idle still hides.
    expect(svg.querySelectorAll('g.cm-rebuild')).toHaveLength(3)
    expect(svg.getAttribute('data-state')).toBe('running')
    expect(svg.getAttribute('aria-hidden')).toBe('true')
  })

  it('retimes the whole cycle from one duration', () => {
    const { container } = render(<CrateMark duration={3.6} />)
    const first = container.querySelector('g.cm-crate') as SVGGElement
    expect(first.style.animation).toContain('3.6s')
  })

  it('holds the pile built when idle, and speaks only when titled', () => {
    const { container } = render(<CrateMark state="idle" title="Working" />)
    const svg = container.querySelector('svg.tf-crates') as SVGSVGElement
    expect(svg.getAttribute('data-state')).toBe('idle')
    expect(svg.getAttribute('role')).toBe('img')
    expect(svg.querySelector('title')?.textContent).toBe('Working')
    // No crate carries an inline animation: an element's own style would beat
    // the stylesheet's still, and the pile would keep cycling.
    for (const g of svg.querySelectorAll<SVGGElement>('g.cm-crate, g.cm-flash')) {
      expect(g.style.animation).toBe('')
    }
  })
})
