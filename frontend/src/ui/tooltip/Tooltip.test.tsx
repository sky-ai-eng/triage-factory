import { fireEvent, render, screen } from '@testing-library/react'
import { act } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { Tooltip, TOOLTIP_DELAY } from './Tooltip'

// The contracts worth pinning: one delay for pointers and none for keyboards,
// the two accessibility modes, and the tap that must not navigate the link it
// sits inside.

beforeEach(() => vi.useFakeTimers())
afterEach(() => vi.useRealTimers())

const tip = () => document.querySelector('.tip')

describe('Tooltip', () => {
  it('waits one project delay under a pointer, and opens', () => {
    render(<Tooltip content="what the label means">mark</Tooltip>)

    fireEvent.mouseEnter(screen.getByText('mark').closest('.tip-host')!)
    expect(tip()).toBeNull()

    act(() => vi.advanceTimersByTime(TOOLTIP_DELAY))
    expect(tip()).toHaveTextContent('what the label means')
  })

  it('opens on focus with no delay at all — a keyboard never crosses anything', () => {
    render(<Tooltip content="definition">mark</Tooltip>)
    const host = screen.getByText('mark').closest('.tip-host')!

    fireEvent.focus(host)

    const t = tip()!
    expect(t).toHaveAttribute('role', 'tooltip')
    // The trigger is the interactive thing: tab stop + described-by.
    expect(host).toHaveAttribute('tabindex', '0')
    expect(host.getAttribute('aria-describedby')).toBe(t.getAttribute('id'))
  })

  it('hides on Escape', () => {
    render(<Tooltip content="definition">mark</Tooltip>)
    const host = screen.getByText('mark').closest('.tip-host')!

    fireEvent.focus(host)
    expect(tip()).not.toBeNull()

    fireEvent.keyDown(host, { key: 'Escape' })
    expect(tip()).toBeNull()
  })

  it('renders scenery in focusable={false} mode: no tab stop, aria-hidden hint', () => {
    render(
      <Tooltip content="3 runs ahead of this one" focusable={false}>
        <span role="img" aria-label="3 runs ahead of this one">
          3
        </span>
      </Tooltip>,
    )
    const host = screen.getByRole('img').closest('.tip-host')!

    // The host takes neither focus nor description — the row it sits inside is
    // the interactive thing, and the mark's own aria-label carries the words.
    expect(host).not.toHaveAttribute('tabindex')

    fireEvent.mouseEnter(host)
    act(() => vi.advanceTimersByTime(TOOLTIP_DELAY))
    const t = tip()!
    expect(t).toHaveAttribute('aria-hidden', 'true')
    expect(t).not.toHaveAttribute('role')
  })

  it('toggles on tap and swallows the click, so a hint inside a link never navigates', () => {
    const navigated = vi.fn()
    render(
      <a href="/run" onClick={navigated}>
        <Tooltip content="definition" focusable={false}>
          <span>mark</span>
        </Tooltip>
      </a>,
    )
    const host = screen.getByText('mark').closest('.tip-host')!

    fireEvent.click(host)
    expect(tip()).not.toBeNull()
    expect(navigated).not.toHaveBeenCalled()

    fireEvent.click(host)
    expect(tip()).toBeNull()
  })

  it('stays inert with no content: no tab stop, no hint, no swallowed clicks', () => {
    const through = vi.fn()
    render(
      <a href="/run" onClick={(e) => (e.preventDefault(), through())}>
        <Tooltip>
          <span>mark</span>
        </Tooltip>
      </a>,
    )
    const host = screen.getByText('mark').closest('.tip-host')!

    expect(host).not.toHaveAttribute('tabindex')
    fireEvent.mouseEnter(host)
    act(() => vi.advanceTimersByTime(TOOLTIP_DELAY))
    expect(tip()).toBeNull()
    // An inert host must not stand between the link and its click.
    fireEvent.click(host)
    expect(through).toHaveBeenCalled()
  })
})
