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

  it('does not flash on a mouse press — pointer focus defers to the tap, which opens', () => {
    render(<Tooltip content="what the label means">mark</Tooltip>)
    const host = screen.getByText('mark').closest('.tip-host')!

    // A real press: pointerdown, then the focus it causes, then the click.
    // Focus must NOT open here (the flash was open-on-focus + toggle-shut on
    // the click of the same press); the tap half then opens it cleanly.
    fireEvent.pointerDown(host)
    fireEvent.focus(host)
    expect(tip()).toBeNull()
    fireEvent.click(host)
    expect(tip()).not.toBeNull()
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

describe('page clamp', () => {
  // jsdom does no layout, so the page and the bubble are stubbed: a viewport
  // width on the root element, and a rect on any element carrying `.tip`.
  const page = (width: number) =>
    Object.defineProperty(document.documentElement, 'clientWidth', {
      value: width,
      configurable: true,
    })
  const rect = (o: Partial<DOMRect>) =>
    ({ left: 0, right: 0, top: 0, bottom: 0, width: 0, height: 0, x: 0, y: 0, ...o }) as DOMRect
  const bubble = (r: Partial<DOMRect>) =>
    vi.spyOn(Element.prototype, 'getBoundingClientRect').mockImplementation(function (
      this: Element,
    ) {
      return this.classList?.contains('tip') ? rect(r) : rect({})
    })
  afterEach(() => {
    vi.restoreAllMocks()
    page(0)
  })

  const open = () => {
    render(
      <Tooltip content="platform-control-plane-migrations#1184" focusable={false}>
        mark
      </Tooltip>,
    )
    fireEvent.mouseEnter(screen.getByText('mark').closest('.tip-host')!)
    act(() => vi.advanceTimersByTime(TOOLTIP_DELAY))
    return tip() as HTMLElement
  }

  it('shifts a top bubble clear of the edge, as the keyframe variable too', () => {
    page(800)
    bubble({ left: -40, right: 160, width: 200 })
    const t = open()
    // 8px clearance from a left edge at -40 is a 48px slide right.
    expect(t.style.getPropertyValue('--tip-dx')).toBe('48px')
    expect(t.style.transform).toBe('translateX(calc(-50% + 48px))')
    expect(t.dataset.side).toBe('top')
  })

  it('flips a side bubble instead of sliding it over its trigger', () => {
    page(800)
    bubble({ left: 780, right: 920, width: 140 })
    render(
      <Tooltip content="off the right edge" side="right">
        mark
      </Tooltip>,
    )
    fireEvent.mouseEnter(screen.getByText('mark').closest('.tip-host')!)
    act(() => vi.advanceTimersByTime(TOOLTIP_DELAY))
    expect((tip() as HTMLElement).dataset.side).toBe('left')
  })

  it('wraps when no position on the page can hold the line', () => {
    page(300)
    bubble({ left: -300, right: 600, width: 900 })
    const t = open()
    expect(t.style.whiteSpace).toBe('normal')
    // The page minus both clearances.
    expect(t.style.maxWidth).toBe('284px')
  })

  it('leaves a bubble that fits untouched', () => {
    page(800)
    bubble({ left: 300, right: 500, width: 200 })
    const t = open()
    expect(t.style.getPropertyValue('--tip-dx')).toBe('')
    expect(t.style.transform).toBe('translateX(-50%)')
  })

  it('treats an unmeasurable page as fitting — jsdom has no layout to clamp against', () => {
    const t = open()
    expect(t.style.getPropertyValue('--tip-dx')).toBe('')
    expect(t.dataset.side).toBe('top')
  })
})
