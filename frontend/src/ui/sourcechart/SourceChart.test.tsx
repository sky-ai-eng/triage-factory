import { render, screen, fireEvent } from '@testing-library/react'
import { describe, expect, it, vi, afterEach } from 'vitest'
import SourceChart from './SourceChart'
import type { SourcePoint } from './SourceChart'

// The chart's one claim is the GAP between its two lines, so what these pin is
// the honesty around that claim: one scale for both series, a null series that
// says "no answer" rather than drawing a zero fortnight, and a hover readout
// that names the day and both figures.

const SERIES: SourcePoint[] = [
  { events: 10, tasks: 5 },
  { events: 20, tasks: 10 },
]

const chart = (series: SourcePoint[] | null) => (
  <SourceChart
    title="WHAT THE REPOSITORIES SENT"
    keys={['events', 'became tasks']}
    units={['events', 'tasks']}
    series={series}
  />
)

afterEach(() => vi.restoreAllMocks())

describe('SourceChart', () => {
  it('draws a null series as a chart waiting for its answer, not a flat zero', () => {
    const { container } = render(chart(null))

    // The empty label and the baseline, and nothing else: no legend keys (a
    // legend for lines that are not there), no lines, no axis.
    expect(screen.getByText('no daily series yet')).toBeInTheDocument()
    expect(container.querySelector('.sc-base')).not.toBeNull()
    expect(container.querySelector('.sc-key')).toBeNull()
    expect(container.querySelector('.sc-line')).toBeNull()
    expect(container.querySelector('.sc-axis')).toBeNull()
  })

  it('draws tasks against the events maximum — one scale, so the gap is real', () => {
    const { container } = render(chart(SERIES))

    const [outer, inner] = Array.from(container.querySelectorAll('.sc-line'))
    // Peak events day reaches the top of the box (y=8); the same day's tasks
    // sit at half the drawn height — against the EVENTS max, not their own.
    expect(outer.getAttribute('points')).toBe('0.0,46.0 320.0,8.0')
    expect(inner.getAttribute('points')).toBe('0.0,65.0 320.0,46.0')
  })

  it('names the day and both figures on hover, and clears on leave', () => {
    vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockReturnValue({
      left: 0,
      width: 320,
      top: 0,
      right: 320,
      bottom: 96,
      height: 96,
      x: 0,
      y: 0,
      toJSON: () => ({}),
    })
    const { container } = render(chart(SERIES))
    const plot = container.querySelector('.sc-plot')!

    fireEvent.mouseMove(plot, { clientX: 320 })
    expect(screen.getByText('today · 20 events · 10 tasks')).toBeInTheDocument()

    fireEvent.mouseMove(plot, { clientX: 0 })
    expect(screen.getByText('1d ago · 10 events · 5 tasks')).toBeInTheDocument()

    fireEvent.mouseLeave(plot)
    expect(screen.queryByText(/events ·/)).not.toBeInTheDocument()
  })

  it('survives a fortnight of zeros without dividing by it', () => {
    const { container } = render(
      chart([
        { events: 0, tasks: 0 },
        { events: 0, tasks: 0 },
      ]),
    )

    for (const line of container.querySelectorAll('.sc-line')) {
      expect(line.getAttribute('points')).toBe('0.0,84.0 320.0,84.0')
      expect(line.getAttribute('points')).not.toMatch(/NaN/)
    }
  })

  it('keeps two charts on one page from sharing a gradient', () => {
    const { container } = render(
      <>
        {chart(SERIES)}
        {chart(SERIES)}
      </>,
    )

    const ids = Array.from(container.querySelectorAll('linearGradient')).map((g) => g.id)
    expect(ids).toHaveLength(4)
    expect(new Set(ids).size).toBe(4)
  })
})
