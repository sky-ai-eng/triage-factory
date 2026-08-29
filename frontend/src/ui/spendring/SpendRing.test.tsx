import { fireEvent, render } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { SpendRing } from './SpendRing'
import { shortModelName } from './shortModelName'

// The contracts worth pinning: the hole is the only readout (no legend
// exists to go blank), hover swaps the readout rather than adding one, the
// money rule drops precision instead of characters, and offline is a dash.

const hole = () => document.querySelector('.sr-total')
const cap = () => document.querySelector('.sr-cap')

describe('shortModelName', () => {
  it('strips the vendor prefix and the build date, nothing else', () => {
    expect(shortModelName('claude-sonnet-4-20250514')).toBe('sonnet-4')
    expect(shortModelName('claude-opus-5')).toBe('opus-5')
    expect(shortModelName('anthropic-haiku-3.5')).toBe('haiku-3.5')
    expect(shortModelName('gpt-5')).toBe('gpt-5')
  })
})

describe('SpendRing', () => {
  const MODELS = [
    { name: 'claude-opus-5', v: 24.8 },
    { name: 'claude-sonnet-5', v: 13.1 },
  ]

  it('shows the whole figure over the standing caption, with no legend anywhere', () => {
    render(<SpendRing models={MODELS} />)
    expect(hole()).toHaveTextContent('$37.90')
    expect(cap()).toHaveTextContent('SPENT TODAY')
    // The split lives in the hole alone: two bands, no list of rows.
    expect(document.querySelectorAll('.sr-seg')).toHaveLength(2)
  })

  it('swaps the hole for the hovered model and restores it on leave', () => {
    render(<SpendRing models={MODELS} />)
    const bands = document.querySelectorAll('.sr-seg')

    fireEvent.mouseEnter(bands[0])
    expect(hole()).toHaveTextContent('$24.80')
    expect(cap()).toHaveTextContent('OPUS-5')
    expect(cap()).toHaveAttribute('data-open')

    fireEvent.mouseLeave(bands[0])
    expect(hole()).toHaveTextContent('$37.90')
    expect(cap()).toHaveTextContent('SPENT TODAY')
  })

  it('drops cents at $100, never characters', () => {
    render(<SpendRing models={[{ name: 'opus', v: 1249.4 }]} />)
    expect(hole()).toHaveTextContent('$1,249')
  })

  it('reads $0.00 with no bands on a zero-spend day — quiet, not broken', () => {
    render(<SpendRing models={[]} />)
    expect(hole()).toHaveTextContent('$0.00')
    expect(document.querySelectorAll('.sr-seg')).toHaveLength(0)
  })

  it('goes inert offline: a dash and an empty track, not the last figure', () => {
    render(<SpendRing models={MODELS} offline />)
    expect(hole()).toHaveTextContent('--')
    expect(document.querySelectorAll('.sr-seg')).toHaveLength(0)
  })

  it('is a link with a destination and a plain block without one', () => {
    const { rerender } = render(<SpendRing models={MODELS} href="/usage" />)
    expect(document.querySelector('a.sr')).toHaveAttribute('href', '/usage')

    rerender(<SpendRing models={MODELS} />)
    expect(document.querySelector('a.sr')).toBeNull()
  })

  it('scales both readouts from size, flooring the caption at 7.5px', () => {
    render(<SpendRing models={MODELS} size={104} />)
    const el = document.querySelector('.sr') as HTMLElement
    expect(el.style.getPropertyValue('--sr-fig')).toBe('13px')
    expect(el.style.getPropertyValue('--sr-cap')).toBe('7.5px')
  })

  it('honors a caller-supplied shortener for display names', () => {
    render(<SpendRing models={[{ name: 'Claude Opus 5', v: 5 }]} shorten={(s) => s} />)
    fireEvent.mouseEnter(document.querySelector('.sr-seg')!)
    expect(cap()).toHaveTextContent('CLAUDE OPUS 5')
  })
})
