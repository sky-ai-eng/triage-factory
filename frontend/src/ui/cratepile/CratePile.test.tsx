import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi, beforeEach } from 'vitest'
import { CratePile } from './CratePile'
import { compactCount } from './compactCount'

// The contracts worth pinning: the cell keying that keeps an existing crate
// from remounting when the count grows, the saturation cap, the full-footprint
// empty state, and the figure's four-glyph bound.

beforeEach(() => {
  vi.stubGlobal(
    'matchMedia',
    vi.fn().mockImplementation((query: string) => ({
      matches: false,
      media: query,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    })),
  )
})

const crates = () => Array.from(document.querySelectorAll('.cp-crate'))

describe('compactCount', () => {
  it('drops precision as magnitude grows, bounded at four glyphs', () => {
    expect(compactCount(340)).toBe('340')
    expect(compactCount(999)).toBe('999')
    expect(compactCount(1249)).toBe('1.2k')
    expect(compactCount(9949)).toBe('9.9k')
    expect(compactCount(12400)).toBe('12k')
  })

  it('holds the four-glyph bound where rounding crosses a band', () => {
    // 9950 rounds to "10.0k" under its own band's format — five glyphs — so
    // the branch is chosen by the rounded value and it takes the next band.
    expect(compactCount(9950)).toBe('10k')
    expect(compactCount(999499)).toBe('999k')
    expect(compactCount(999500)).toBe('1.0m')
    expect(compactCount(9950000)).toBe('10m')
    for (const n of [999, 1000, 9949, 9950, 999499, 999500, 999999999]) {
      expect(compactCount(n).length).toBeLessThanOrEqual(4)
    }
  })
})

describe('CratePile', () => {
  it('draws one crate per item and announces the count with its noun', () => {
    render(<CratePile count={7} caption="open pull requests" />)
    expect(crates()).toHaveLength(7)
    expect(screen.getByRole('img', { name: '7 open pull requests' })).toBeInTheDocument()
  })

  it('keeps an existing crate mounted when the count grows — keyed on the cell', () => {
    const { rerender } = render(<CratePile count={18} />)
    const before = new Set(crates())

    rerender(<CratePile count={19} />)

    // One new node; every prior crate is the same DOM element. Keyed by array
    // index, the new cell's low painter sum shifted every later key and
    // remounted them all — a count going UP made existing crates blink.
    const after = crates()
    expect(after).toHaveLength(19)
    expect(after.filter((c) => !before.has(c))).toHaveLength(1)
  })

  it('saturates the drawing at 36 while the figure keeps counting', () => {
    render(<CratePile count={112} />)
    expect(crates()).toHaveLength(36)
    expect(screen.getByRole('img', { name: /112/ })).toBeInTheDocument()
  })

  it('frames every count by the pallet, so a crate never changes size', () => {
    // A tight box around the occupied cells made one crate render ~3x its
    // size in a full pile. The frame is the footprint at every count: the
    // width of the drawing's coordinate space is the pallet's, whether it
    // holds zero, one, or a full stack.
    const widthOf = (count: number) => {
      const { container, unmount } = render(<CratePile count={count} />)
      const vb = container.querySelector('.cp-svg')!.getAttribute('viewBox')!
      unmount()
      return Number(vb.split(' ')[2])
    }
    const zero = widthOf(0)
    expect(widthOf(1)).toBe(zero)
    expect(widthOf(17)).toBe(zero)
    expect(widthOf(40)).toBe(zero)
  })

  it('draws the full dashed footprint at zero — the same pallet, nothing on it', () => {
    render(<CratePile count={0} />)
    expect(crates()).toHaveLength(0)
    expect(document.querySelector('.cp-ghost')).not.toBeNull()
  })

  it('uses the singular noun at exactly one', () => {
    render(<CratePile count={1} caption="open pull requests" captionOne="open pull request" />)
    expect(screen.getByText('open pull request')).toBeInTheDocument()
  })

  it('goes inert offline: em dash, empty pallet, no last-known figure', () => {
    render(<CratePile count={23} offline />)
    expect(crates()).toHaveLength(0)
    expect(document.querySelector('.cp-ghost')).not.toBeNull()
    expect(screen.getByText('—')).toBeInTheDocument()
  })

  it('is a link with a destination and a plain block without one', () => {
    const { rerender } = render(<CratePile count={3} href="/prs" />)
    expect(document.querySelector('a.cp')).toHaveAttribute('href', '/prs')

    rerender(<CratePile count={3} />)
    expect(document.querySelector('a.cp')).toBeNull()
  })
})
