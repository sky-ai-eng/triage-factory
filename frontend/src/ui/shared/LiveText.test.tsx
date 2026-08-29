import { act, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { LiveText } from './LiveText'
import { useLiveText } from './useLiveText'

// The contracts here are the performance ones: one shared timer however many
// leaves, re-render only when the string changes, and a clean stop when the
// last leaf leaves. Formatting itself belongs to the callers' own tests.

const seconds = (t0: number) => (now: number) => `${Math.floor((now - t0) / 1000)}s`
const hours = (t0: number) => (now: number) => `${Math.floor((now - t0) / 3600_000)}h`

describe('LiveText', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => vi.useRealTimers())

  it('renders the computed text and advances it as the clock ticks', () => {
    render(<LiveText compute={seconds(Date.now())} />)
    expect(screen.getByText('0s')).toBeInTheDocument()
    act(() => vi.advanceTimersByTime(1000))
    expect(screen.getByText('1s')).toBeInTheDocument()
    act(() => vi.advanceTimersByTime(3000))
    expect(screen.getByText('4s')).toBeInTheDocument()
  })

  it('does not re-render a subscriber whose text has not changed', () => {
    let renders = 0
    function Probe({ compute }: { compute: (now: number) => string }) {
      renders++
      return <>{useLiveText(compute)}</>
    }
    render(<Probe compute={hours(Date.now())} />)
    expect(screen.getByText('0h')).toBeInTheDocument()
    const after = renders
    // Ten ticks inside the same hour: the snapshot string never changes, so
    // the store bails before React schedules anything.
    act(() => vi.advanceTimersByTime(10_000))
    expect(renders).toBe(after)
    // The hour turning is the one tick that renders.
    act(() => vi.advanceTimersByTime(3600_000))
    expect(screen.getByText('1h')).toBeInTheDocument()
    expect(renders).toBe(after + 1)
  })

  it('shares one interval across every mounted leaf and stops it with the last', () => {
    const t0 = Date.now()
    const a = render(<LiveText compute={seconds(t0)} />)
    const b = render(<LiveText compute={hours(t0)} />)
    expect(vi.getTimerCount()).toBe(1)
    a.unmount()
    expect(vi.getTimerCount()).toBe(1)
    b.unmount()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('reads a fresh clock on mount after an idle stretch, not the stalled one', () => {
    const t0 = Date.now()
    const first = render(<LiveText compute={seconds(t0)} />)
    first.unmount()
    // No subscribers: the ticker is stopped while the wall clock moves on.
    vi.advanceTimersByTime(90_000)
    render(<LiveText compute={seconds(t0)} />)
    expect(screen.getByText('90s')).toBeInTheDocument()
  })
})
