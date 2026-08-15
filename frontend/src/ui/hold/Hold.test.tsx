import { render, screen, act, fireEvent } from '@testing-library/react'
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import Hold from './Hold'

// The gesture is a safety mechanism, so what gets pinned is the ways it could
// quietly stop being one: firing early, firing without being held, or naming
// itself something other than what it does.
//
// jsdom has no layout, so getBoundingClientRect returns zeroes and the ring
// geometry is degenerate — that is fine. None of these assert on the rings;
// they assert on when onConfirm fires and what the control announces.

const MS = 850
const SETTLE = 220

function press(el: Element) {
  fireEvent.pointerDown(el, { button: 0, clientX: 0, clientY: 0 })
}

describe('Hold', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => vi.useRealTimers())

  it('does not fire until the whole gesture has been paid', () => {
    const onConfirm = vi.fn()
    render(<Hold label="Remove" ms={MS} onConfirm={onConfirm} />)
    const btn = screen.getByRole('button')

    press(btn)
    act(() => {
      vi.advanceTimersByTime(MS * 0.8)
    })
    expect(onConfirm).not.toHaveBeenCalled()

    act(() => {
      vi.advanceTimersByTime(MS * 0.3 + SETTLE)
    })
    expect(onConfirm).toHaveBeenCalledTimes(1)
  })

  it('abandons the gesture on release, with nothing committed', () => {
    const onConfirm = vi.fn()
    render(<Hold label="Remove" ms={MS} onConfirm={onConfirm} />)
    const btn = screen.getByRole('button')

    press(btn)
    act(() => {
      vi.advanceTimersByTime(MS * 0.6)
    })
    fireEvent.pointerUp(btn)
    act(() => {
      vi.advanceTimersByTime(MS * 2)
    })

    expect(onConfirm).not.toHaveBeenCalled()
  })

  it('survives the pointer leaving, which is not a release', () => {
    const onConfirm = vi.fn()
    render(<Hold label="Remove" ms={MS} onConfirm={onConfirm} />)
    const btn = screen.getByRole('button')

    // The label changes width mid-hold, so the button resizes under the cursor.
    // If leaving cancelled, the gesture would work or fail depending on where
    // it was started.
    press(btn)
    fireEvent.pointerLeave(btn)
    act(() => {
      vi.advanceTimersByTime(MS + SETTLE)
    })

    expect(onConfirm).toHaveBeenCalledTimes(1)
  })

  it('still plays the gesture at ms=0 rather than firing bare', () => {
    const onConfirm = vi.fn()
    render(<Hold label="Save" ms={0} onConfirm={onConfirm} />)

    press(screen.getByRole('button'))
    // The rings arrive at once and settle. An action that applies with nothing
    // on screen reads as a mis-click.
    expect(onConfirm).not.toHaveBeenCalled()

    act(() => {
      vi.advanceTimersByTime(SETTLE)
    })
    expect(onConfirm).toHaveBeenCalledTimes(1)
  })

  it('can be held from the keyboard', () => {
    const onConfirm = vi.fn()
    render(<Hold label="Remove" ms={MS} onConfirm={onConfirm} />)
    const btn = screen.getByRole('button')

    // A pointer-only hold is a commit a keyboard user can stage and never make,
    // which is worse than a plain button.
    fireEvent.keyDown(btn, { key: ' ' })
    act(() => {
      vi.advanceTimersByTime(MS + SETTLE)
    })
    expect(onConfirm).toHaveBeenCalledTimes(1)
  })

  it('names the gesture, not just the verb', () => {
    render(<Hold label="Remove from team" ms={MS} />)
    expect(
      screen.getByRole('button', { name: 'Remove from team — press and hold to confirm' }),
    ).toBeInTheDocument()
  })

  describe('surface mode', () => {
    it('claims no name of its own, leaving the triggers to label themselves', () => {
      render(
        <Hold variant="surface" trigger="[data-remove]" ms={MS}>
          <span>2 selected</span>
          <span data-remove aria-label="Remove from team — press and hold to confirm">
            Remove from team
          </span>
        </Hold>,
      )

      // It used to render as a tabIndex={-1} <button aria-label="Hold to save">
      // over a bar whose visible verb read "Remove from team" — WCAG 2.5.3, and
      // on an irreversible verb a plain lie about what the gesture does.
      expect(screen.queryByRole('button', { name: /Hold to save/ })).not.toBeInTheDocument()
      expect(
        screen.getByLabelText('Remove from team — press and hold to confirm'),
      ).toBeInTheDocument()
    })

    it('ignores a press that misses the trigger', () => {
      const onConfirm = vi.fn()
      render(
        <Hold variant="surface" trigger="[data-remove]" ms={MS} onConfirm={onConfirm}>
          <span data-testid="inert">2 selected</span>
          <span data-remove>Remove</span>
        </Hold>,
      )

      press(screen.getByTestId('inert'))
      act(() => {
        vi.advanceTimersByTime(MS + SETTLE)
      })
      expect(onConfirm).not.toHaveBeenCalled()
    })

    it('lets a trigger set its own price', () => {
      const onConfirm = vi.fn()
      render(
        <Hold variant="surface" trigger="[data-verb]" ms={900} onConfirm={onConfirm}>
          <span data-verb data-hold-ms="0" data-testid="cheap">
            Change role
          </span>
        </Hold>,
      )

      // One surface, a removal that must be leaned on and an ordinary verb at
      // zero — without a second Hold.
      press(screen.getByTestId('cheap'))
      act(() => {
        vi.advanceTimersByTime(SETTLE)
      })
      expect(onConfirm).toHaveBeenCalledTimes(1)
    })
  })
})
