import { render, screen, act } from '@testing-library/react'
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import Countdown from './Countdown'

// Two things worth pinning, both invisible in a still: that the dial DRAINS
// rather than fills, and that reduced motion coarsens it instead of removing
// it. The second is the interesting one — this is the only place in the system
// that takes a stutter on purpose, because here the motion IS the information
// and deleting it would be information loss dressed as an accommodation.

function filled() {
  // Each crate is an outline plus, when it has anything left, a filled rect.
  return document.querySelectorAll('rect[fill]:not([fill="none"])').length
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

describe('Countdown', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    setReducedMotion(false)
  })
  afterEach(() => {
    vi.useRealTimers()
  })

  it('holds its footprint at zero', () => {
    const { rerender } = render(<Countdown frac={1} steps={3} />)
    const outlines = document.querySelectorAll('rect[fill="none"]').length
    expect(outlines).toBe(3)

    rerender(<Countdown frac={0} steps={3} />)
    // Empty crates stay drawn, so the mark never collapses and nothing shifts
    // around it.
    expect(document.querySelectorAll('rect[fill="none"]').length).toBe(3)
    expect(filled()).toBe(0)
  })

  it('drains from the top down as the window closes', () => {
    const { rerender } = render(<Countdown frac={1} steps={3} />)
    expect(filled()).toBe(3)

    rerender(<Countdown frac={0.5} steps={3} />)
    expect(filled()).toBeLessThan(3)

    rerender(<Countdown frac={0} steps={3} />)
    expect(filled()).toBe(0)
  })

  it('coarsens under reduced motion instead of removing the drain', () => {
    setReducedMotion(true)
    const onDone = vi.fn()
    render(<Countdown seconds={3} onDone={onDone} />)

    // Stepping once a second, not once a frame — and still closing on time.
    act(() => {
      vi.advanceTimersByTime(1000)
    })
    expect(filled()).toBeGreaterThan(0)

    act(() => {
      vi.advanceTimersByTime(2100)
    })
    expect(onDone).toHaveBeenCalledTimes(1)
  })

  it('names itself for a reader', () => {
    render(<Countdown frac={0.5} label="undo window" />)
    expect(screen.getByRole('img', { name: 'undo window' })).toBeInTheDocument()
  })
})
