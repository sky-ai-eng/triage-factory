import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { Converge } from './Converge'

// The component contracts the unit tests on convergeParts cannot see: the
// band moving every lane through one function, fill mode's class, and the
// titleNode/decoded-span split of the headline's accessibility.

const OUTCOMES = [
  { name: 'merged', v: 6, tone: 'warm' as const },
  { name: 'running', v: 3, tone: 'cool' as const },
  { name: 'need you', v: 7, tone: 'ask' as const },
  { name: 'filtered by rules', v: 296, tone: 'quiet' as const },
]

const labelTops = () =>
  Array.from(document.querySelectorAll('.cendlab')).map((el) =>
    parseFloat((el as HTMLElement).style.top),
  )

describe('Converge', () => {
  it('names the data on the chart itself', () => {
    render(<Converge outcomes={OUTCOMES} total={312} />)
    expect(
      screen.getByRole('img', {
        name: '312 resolving into 6 merged, 3 running, 7 need you, 296 filtered by rules',
      }),
    ).toBeInTheDocument()
  })

  it('moves every endpoint together when the band narrows — one yFor, no drift', () => {
    const { unmount } = render(<Converge outcomes={OUTCOMES} height={260} />)
    const full = labelTops()
    unmount()

    render(<Converge outcomes={OUTCOMES} height={260} endpointBand={[0.4, 1]} />)
    const banded = labelTops()

    // Compression, not translation: every lane lands lower, and the spacing
    // shrinks rather than the set sliding off the floor.
    banded.forEach((top, i) => expect(top).toBeGreaterThan(full[i]))
    expect(banded[banded.length - 1]).toBeLessThan(100)
    expect(banded[1] - banded[0]).toBeLessThan(full[1] - full[0])
  })

  it('reproduces the default drawing exactly at the [0, 1] band', () => {
    const { unmount } = render(<Converge outcomes={OUTCOMES} height={260} />)
    const bare = labelTops()
    unmount()

    render(<Converge outcomes={OUTCOMES} height={260} endpointBand={[0, 1]} />)
    expect(labelTops()).toEqual(bare)
  })

  it('wears the fill class only when asked — the container owns the height there', () => {
    const { unmount } = render(<Converge outcomes={OUTCOMES} fill />)
    expect(document.querySelector('.conv')!.className).toContain('conv-fill')
    unmount()

    render(<Converge outcomes={OUTCOMES} />)
    expect(document.querySelector('.conv')!.className).not.toContain('conv-fill')
  })

  it('leaves the titleNode alone and hides the decoded words as scenery', () => {
    render(
      <Converge
        outcomes={OUTCOMES}
        kicker="SINCE MIDNIGHT"
        title="events triaged"
        titleNode={<span role="img" aria-label="312 events triaged" />}
      />,
    )
    // The self-animating figure carries the headline's accessible name; the
    // scrambling glyphs are hidden from a reader who cannot watch them settle.
    expect(screen.getByRole('img', { name: '312 events triaged' })).toBeInTheDocument()
    expect(screen.getByText('events triaged')).toHaveAttribute('aria-hidden', 'true')
  })
})
