import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { RunRows } from './RunRows'
import type { RunRowItem } from './RunRows'

// The contracts worth pinning: the two axes land as tone classes, only a
// working row scans, the queue mark speaks one string to eyes and screen
// readers alike, and a plain primary click is the only one the row intercepts.

function row(over: Partial<RunRowItem>): RunRowItem {
  return {
    id: 'r1',
    source: 'pull',
    lifecycle: 'working',
    activity: 'Replaying 6 commits onto origin/main',
    ref: 'factory-api#772',
    age: '4m',
    href: '/runs/r1',
    ...over,
  }
}

describe('RunRows', () => {
  it('renders the prose, the reference and the age on one grid row', () => {
    render(<RunRows rows={[row({})]} />)
    const el = screen.getByText('Replaying 6 commits onto origin/main').closest('a')!
    expect(el).toHaveAttribute('href', '/runs/r1')
    expect(el.querySelector('.rr-ref')).toHaveTextContent('factory-api#772')
    expect(el.querySelector('.rr-age')).toHaveTextContent('4m')
  })

  it('scans only the working row — emission is an agent acting, nothing else', () => {
    render(
      <RunRows
        rows={[
          row({}),
          row({ id: 'r2', lifecycle: 'queued', activity: 'Fix the flaking test', queue: 2 }),
        ]}
      />,
    )
    expect(screen.getByText('Replaying 6 commits onto origin/main')).toHaveAttribute('data-tone')
    expect(screen.getByText('Fix the flaking test')).not.toHaveAttribute('data-tone')
  })

  it('gives asks the warm tick whatever the lifecycle, and failure its own mark', () => {
    render(
      <RunRows
        rows={[
          row({ id: 'a', lifecycle: 'done', asks: true, activity: 'Approve the draft' }),
          row({ id: 'b', lifecycle: 'failed', activity: 'Failed on the third attempt' }),
        ]}
      />,
    )
    expect(screen.getByText('Approve the draft').closest('.rr-row')!.className).toContain('rr-warm')
    expect(screen.getByText('Failed on the third attempt').closest('.rr-row')!.className).toContain(
      'rr-alarm',
    )
  })

  it('speaks the queue mark once for eyes and assistive technology alike', () => {
    render(<RunRows rows={[row({ lifecycle: 'queued', queue: 3 })]} />)
    const mark = screen.getByRole('img', { name: '3 runs ahead of this one' })
    expect(mark.querySelector('.rr-q-n')).toHaveTextContent('3')
    // The mark is scenery inside the row's anchor: no tab stop of its own.
    expect(mark.closest('.tip-host')).not.toHaveAttribute('tabindex')
  })

  it('says "Next in the queue" at zero rather than "0 runs ahead"', () => {
    render(<RunRows rows={[row({ lifecycle: 'queued', queue: 0 })]} />)
    expect(screen.getByRole('img', { name: 'Next in the queue' })).toBeInTheDocument()
  })

  it('intercepts only a plain primary click for in-app navigation', () => {
    const onPick = vi.fn()
    render(<RunRows rows={[row({})]} onPick={onPick} />)
    const el = screen.getByText('Replaying 6 commits onto origin/main').closest('a')!

    fireEvent.click(el, { metaKey: true })
    fireEvent.click(el, { button: 1 })
    expect(onPick).not.toHaveBeenCalled()

    fireEvent.click(el, { button: 0 })
    expect(onPick).toHaveBeenCalledTimes(1)
  })

  it('renders a row with nowhere to go as inert, not as a dead link', () => {
    render(<RunRows rows={[row({ nav: false })]} />)
    const el = screen.getByText('Replaying 6 commits onto origin/main').closest('.rr-row')!
    expect(el.tagName).toBe('DIV')
    expect(el.className).toContain('rr-inert')
  })

  it('shows the empty answer, the pre-colored count, and the caller-owned more link', () => {
    render(
      <RunRows
        label="NEEDS YOU"
        count={<span style={{ color: 'var(--color-warm)' }}>0</span>}
        rows={[]}
        empty="Nothing needs you."
        more={<a href="/board">open the board</a>}
      />,
    )
    expect(screen.getByText('Nothing needs you.')).toBeInTheDocument()
    expect(screen.getByText('NEEDS YOU')).toBeInTheDocument()
    expect(document.querySelector('.rr-count')).toHaveTextContent('0')
    expect(screen.getByText('open the board').closest('.rr-more')).not.toBeNull()
  })
})
