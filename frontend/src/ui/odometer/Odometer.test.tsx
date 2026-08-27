import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi, beforeEach } from 'vitest'
import { Odometer, Ticker } from './Odometer'

// The contracts worth pinning are the ones a screenshot cannot show: where a
// ramp STOPS, what a screen reader hears instead of thirty digits, and that
// removing the motion leaves the answer rather than the frame it started from.

function setReducedMotion(reduced: boolean) {
  vi.stubGlobal(
    'matchMedia',
    vi.fn().mockImplementation((query: string) => ({
      matches: reduced && query.includes('prefers-reduced-motion'),
      media: query,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    })),
  )
}

const ramps = () => Array.from(document.querySelectorAll('.odo-ramp')) as HTMLElement[]

describe('Odometer', () => {
  beforeEach(() => setReducedMotion(false))

  it('stops each digit on its own face, one ramp per digit', () => {
    render(<Odometer value={164} />)

    // 22px a line, landing on the third pass: digit d sits at -(20 + d) × 22.
    expect(ramps().map((r) => r.style.getPropertyValue('--roll'))).toEqual([
      '-462px', // 1
      '-572px', // 6
      '-528px', // 4
    ])
  })

  it('starts each digit one beat behind the one to its left', () => {
    render(<Odometer value={380} at={0.44} />)

    expect(ramps().map((r) => r.style.getPropertyValue('--at'))).toEqual([
      '0.44s',
      '0.50s',
      '0.56s',
    ])
  })

  it('announces the reading, and hides the ramp that spells it', () => {
    render(<Odometer value={40} suffix="s" label="40 seconds" />)

    // Without this the page's primary statistics read as 0123456789… to a
    // screen reader and to anyone copying them.
    expect(screen.getByRole('img', { name: '40 seconds' })).toBeInTheDocument()
    expect(document.querySelector('.odo-digits')).toHaveAttribute('aria-hidden', 'true')
  })

  it('draws a dash for a measurement nobody has taken, and claims no role', () => {
    render(<Odometer value={null} />)

    expect(screen.getByText('—')).toBeInTheDocument()
    expect(screen.queryByRole('img')).not.toBeInTheDocument()
    expect(ramps()).toHaveLength(0)
  })

  it('writes the end state itself under reduced motion', () => {
    setReducedMotion(true)
    render(<Odometer value={7} />)

    // The blanket rule kills the animation, and an animation filling `both`
    // that never runs leaves the ramp on the digit ZERO. So the end state is
    // stated rather than animated to.
    expect(document.querySelector('.odo')).toHaveAttribute('data-still')
  })
})

describe('Ticker', () => {
  beforeEach(() => setReducedMotion(false))

  it('rolls up from nothing on arrival', () => {
    render(<Ticker value={22} />)

    const cells = Array.from(document.querySelectorAll('.tick-v')).map((n) => n.textContent)
    expect(cells).toEqual(['0', '22'])
  })

  it('travels from the old value to the new one, and remounts to re-run', () => {
    const { rerender } = render(<Ticker value={22} />)
    const first = document.querySelector('.tick-win')

    rerender(<Ticker value={23} />)

    expect(Array.from(document.querySelectorAll('.tick-v')).map((n) => n.textContent)).toEqual([
      '22',
      '23',
    ])
    // A new node, not the same one re-styled: a remount is what makes the
    // animation run a second time.
    expect(document.querySelector('.tick-win')).not.toBe(first)
  })

  it('does not flash the row reading on arrival, only on a change', () => {
    const { rerender } = render(<Ticker value={22} variant="row" />)
    // The first frame a reader sees is not news.
    expect(document.querySelector('.tick-frame')).toBeNull()

    rerender(<Ticker value={23} variant="row" />)
    expect(document.querySelector('.tick-frame')).not.toBeNull()
  })

  it('draws a dash while there is nothing to report', () => {
    render(<Ticker value={null} />)

    expect(screen.getByText('—')).toBeInTheDocument()
    expect(document.querySelector('.tick-win')).toBeNull()
  })
})
