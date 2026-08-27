import { render, screen, act } from '@testing-library/react'
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import Acquire from './Acquire'

// The contracts worth pinning are all about TIME, not appearance: the sequence
// runs on its own clock regardless of when the value lands, and under reduced
// motion it does not run at all. Both are the kind of thing that looks fine in
// a screenshot and is wrong in use.

const SEQUENCE_MS = 70 * (2 + 1.2 + 4.3 + 3.15) // every beat, start to finish

function box() {
  // The component renders no role of its own, so reach for it structurally.
  return document.querySelector('.acq') as HTMLElement
}

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

describe('Acquire', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    setReducedMotion(false)
  })
  afterEach(() => {
    vi.useRealTimers()
  })

  it('runs every beat in order before showing a value it already has', () => {
    render(<Acquire value="164" />)

    // The value is in hand from the first render and still must not appear:
    // a figure arriving during the flash makes the whole gesture read as a glitch.
    expect(box()).toHaveAttribute('data-phase', 'frame')
    expect(screen.queryByText('164')).not.toBeInTheDocument()

    const seen = ['frame']
    for (let t = 0; t < SEQUENCE_MS + 50; t += 10) {
      act(() => {
        vi.advanceTimersByTime(10)
      })
      const phase = box().getAttribute('data-phase')!
      if (phase !== seen[seen.length - 1]) seen.push(phase)
    }

    expect(seen).toEqual(['frame', 'flash', 'gap', 'cross', 'done'])
    expect(screen.getByText('164')).toBeInTheDocument()
  })

  it('hands off to the answer-shaped skeleton when the call outlives the sequence', () => {
    render(<Acquire value={null} skeleton="$--.--" />)
    act(() => {
      vi.advanceTimersByTime(SEQUENCE_MS + 50)
    })

    // Not the reserved box carried over — a rectangle that persists past the
    // sequence reads as the sequence stalling.
    expect(box()).toHaveAttribute('data-phase', 'wait')
    expect(screen.getByText('$--.--')).toBeInTheDocument()
  })

  it('lands a value that arrives after the sequence has already settled', () => {
    const { rerender } = render(<Acquire value={null} skeleton="--" />)
    act(() => {
      vi.advanceTimersByTime(SEQUENCE_MS + 50)
    })
    expect(box()).toHaveAttribute('data-phase', 'wait')

    rerender(<Acquire value="41" />)
    expect(box()).toHaveAttribute('data-phase', 'done')
    expect(screen.getByText('41')).toBeInTheDocument()
  })

  it('distinguishes failed from waiting, and marks only waiting as busy', () => {
    render(<Acquire value={null} failed skeleton="$--.--" />)
    act(() => {
      vi.advanceTimersByTime(SEQUENCE_MS + 50)
    })

    expect(box()).toHaveAttribute('data-phase', 'failed')
    // Inert, because nothing is still happening — that difference is the only
    // thing separating "slow" from "gone".
    expect(box()).not.toHaveAttribute('aria-busy')
  })

  it('mounts straight into the end state under reduced motion', () => {
    setReducedMotion(true)
    render(<Acquire value="164" />)

    // No beats at all — not a shortened sequence. A sequence at double speed is
    // still a sequence.
    expect(box()).toHaveAttribute('data-phase', 'done')
    expect(screen.getByText('164')).toBeInTheDocument()
  })

  it('mounts into the skeleton under reduced motion when the value is absent', () => {
    setReducedMotion(true)
    render(<Acquire value={null} skeleton="--" />)

    expect(box()).toHaveAttribute('data-phase', 'wait')
    expect(screen.getByText('--')).toBeInTheDocument()
  })
})
