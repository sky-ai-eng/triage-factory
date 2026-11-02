import { render, screen, act, fireEvent } from '@testing-library/react'
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import NotifyStrip, { ARRIVAL_MS, type StripItem } from './NotifyStrip'
import { installFakeAnimations, type FakeAnimation } from '../shared/fakeAnimation'

// What gets pinned is what the strip promises a reader: an urgent condition
// stays until it resolves and draws full-size until it is read, and a high
// arrival is announced for its 8s of attention, no more and no less.
//
// jsdom implements no Web Animations, so Element.prototype.animate is
// FakeAnimation, whose `finished` resolves on the fake timers.

const FADE = 120
const CLOSE = 160

let animations: FakeAnimation[] = []

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

/** Advance the clocks and let every promise and render they trigger settle. */
async function tick(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms)
  })
}

function item(id: string, extra: Partial<StripItem> = {}): StripItem {
  return { id, scope: 'ORG', title: `title ${id}`, since: '2d', read: false, ...extra }
}

function urgentLine() {
  return document.querySelector('.ns-urgent')
}

describe('NotifyStrip', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    setReducedMotion(false)
    animations = installFakeAnimations()
    // jsdom's document never has focus; the strip would read every test as
    // the viewer being away and hold the clock.
    vi.spyOn(document, 'hasFocus').mockReturnValue(true)
  })
  afterEach(() => {
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  describe('the urgent line', () => {
    it('draws the full band while unread and the line once read', () => {
      const { rerender } = render(<NotifyStrip urgent={[item('a')]} onOpen={() => {}} />)
      expect(urgentLine()).toHaveAttribute('data-full')

      rerender(<NotifyStrip urgent={[item('a', { read: true })]} onOpen={() => {}} />)
      expect(urgentLine()).not.toHaveAttribute('data-full')
    })

    it('starts on the first unread item, never behind a read one', () => {
      render(
        <NotifyStrip
          urgent={[item('a', { read: true }), item('b'), item('c')]}
          onOpen={() => {}}
        />,
      )
      expect(screen.getByRole('button', { name: 'title b' })).toBeInTheDocument()
      expect(screen.getByText('2 of 3')).toBeInTheDocument()
    })

    it('steps between items within its bounds', () => {
      render(<NotifyStrip urgent={[item('a'), item('b')]} onOpen={() => {}} />)
      const prev = screen.getByRole('button', { name: 'Previous urgent notification' })
      const next = screen.getByRole('button', { name: 'Next urgent notification' })
      expect(prev).toHaveAttribute('aria-disabled', 'true')

      fireEvent.click(next)
      expect(screen.getByRole('button', { name: 'title b' })).toBeInTheDocument()
      expect(next).toHaveAttribute('aria-disabled', 'true')
      expect(prev).not.toHaveAttribute('aria-disabled')

      // An end is a no-op, not a wrap.
      fireEvent.click(next)
      expect(screen.getByRole('button', { name: 'title b' })).toBeInTheDocument()
    })

    it('keeps the stepper while any item is unread, so each stays reachable', () => {
      render(<NotifyStrip urgent={[item('a'), item('b', { read: true })]} onOpen={() => {}} />)
      fireEvent.click(screen.getByRole('button', { name: 'Next urgent notification' }))
      expect(screen.getByRole('button', { name: 'title b' })).toBeInTheDocument()
      expect(urgentLine()).toHaveAttribute('data-full')

      fireEvent.click(screen.getByRole('button', { name: 'Previous urgent notification' }))
      expect(screen.getByRole('button', { name: 'title a' })).toBeInTheDocument()
    })

    it('stays on the item the viewer stepped to when another arrives', () => {
      const { rerender } = render(<NotifyStrip urgent={[item('a'), item('b')]} onOpen={() => {}} />)
      fireEvent.click(screen.getByRole('button', { name: 'Next urgent notification' }))

      rerender(<NotifyStrip urgent={[item('c'), item('a'), item('b')]} onOpen={() => {}} />)
      expect(screen.getByRole('button', { name: 'title b' })).toBeInTheDocument()
      expect(screen.getByText('3 of 3')).toBeInTheDocument()
    })

    it('draws a condition that takes the line as new content, for the live region', () => {
      const { rerender } = render(<NotifyStrip urgent={[item('a')]} onOpen={() => {}} />)
      const before = screen.getByRole('button', { name: 'title a' })

      rerender(<NotifyStrip urgent={[item('c'), item('a')]} onOpen={() => {}} />)
      const after = screen.getByRole('button', { name: 'title c' })
      // A rewritten text node is a change the polite region does not announce;
      // a new node is an addition it does.
      expect(after).not.toBe(before)
    })

    it('counts the rest instead of stepping once read', () => {
      render(
        <NotifyStrip
          urgent={[item('a', { read: true }), item('b', { read: true })]}
          onOpen={() => {}}
        />,
      )
      expect(screen.getByText('· +1 urgent')).toBeInTheDocument()
      expect(screen.queryByRole('button', { name: /urgent notification/ })).not.toBeInTheDocument()
    })

    it('opens an item from its title, and runs its action from the button', () => {
      const onOpen = vi.fn()
      const onSelect = vi.fn()
      render(
        <NotifyStrip
          urgent={[item('a', { action: { label: 'Open GitHub access', onSelect } })]}
          onOpen={onOpen}
        />,
      )
      fireEvent.click(screen.getByRole('button', { name: 'title a' }))
      expect(onOpen).toHaveBeenCalledExactlyOnceWith('a')
      fireEvent.click(screen.getByRole('button', { name: 'Open GitHub access' }))
      expect(onSelect).toHaveBeenCalledTimes(1)
    })

    it('takes no press on an action already out', () => {
      const onSelect = vi.fn()
      render(
        <NotifyStrip
          urgent={[item('a', { action: { label: 'Retrying…', onSelect, busy: true } })]}
          onOpen={vi.fn()}
        />,
      )
      const button = screen.getByRole('button', { name: 'Retrying…' })
      expect(button).toHaveAttribute('aria-disabled', 'true')
      fireEvent.click(button)
      expect(onSelect).not.toHaveBeenCalled()
    })

    it('leaves showing what it was when its last condition resolves', async () => {
      const { rerender } = render(<NotifyStrip urgent={[item('a')]} onOpen={() => {}} />)

      rerender(<NotifyStrip urgent={[]} onOpen={() => {}} />)
      expect(screen.getByText('title a')).toBeInTheDocument()
      await tick(FADE + CLOSE)
      expect(screen.queryByText('title a')).not.toBeInTheDocument()
    })

    it('cancels a close when a condition opens again before it has closed', async () => {
      const { rerender } = render(<NotifyStrip urgent={[item('a')]} onOpen={() => {}} />)
      rerender(<NotifyStrip urgent={[]} onOpen={() => {}} />)
      await tick(FADE)

      rerender(<NotifyStrip urgent={[item('b')]} onOpen={() => {}} />)
      await tick(CLOSE)
      expect(screen.getByRole('button', { name: 'title b' })).toBeInTheDocument()
      // The close's held opacity and height went with it.
      const closes = animations.filter((a) => a.keyframes.at(-1)?.opacity === 0)
      expect(closes.every((a) => a.playState === 'idle')).toBe(true)
    })
  })

  describe('an arrival', () => {
    it('is announced for 8s, then closes, then is reported done', async () => {
      const onArrivalDone = vi.fn()
      render(
        <NotifyStrip
          urgent={[]}
          arrival={item('h')}
          onArrivalDone={onArrivalDone}
          onOpen={() => {}}
        />,
      )
      await tick(ARRIVAL_MS)
      // The caller clears the arrival at onArrivalDone, so it must not fire
      // while the line is still closing.
      await tick(FADE + CLOSE - 1)
      expect(onArrivalDone).not.toHaveBeenCalled()
      await tick(1)
      expect(onArrivalDone).toHaveBeenCalledExactlyOnceWith('h')
    })

    it('holds its clock while the pointer is over the strip or focus is inside it', async () => {
      const onArrivalDone = vi.fn()
      render(
        <NotifyStrip
          urgent={[]}
          arrival={item('h')}
          onArrivalDone={onArrivalDone}
          onOpen={() => {}}
        />,
      )
      const strip = screen.getByRole('region', { name: 'Notifications' })

      await tick(3000)
      fireEvent.pointerEnter(strip)
      await tick(60_000)
      fireEvent.pointerLeave(strip)
      await tick(0)
      expect(onArrivalDone).not.toHaveBeenCalled()

      const open = screen.getByRole('button', { name: 'open' })
      act(() => open.focus())
      await tick(60_000)
      act(() => open.blur())
      await tick(0)
      expect(onArrivalDone).not.toHaveBeenCalled()

      // It resumes with the time it had left, not a fresh 8s.
      await tick(ARRIVAL_MS - 3000)
      await tick(FADE + CLOSE)
      expect(onArrivalDone).toHaveBeenCalledExactlyOnceWith('h')
    })

    it('holds its clock while the tab is hidden', async () => {
      const onArrivalDone = vi.fn()
      const visibility = vi.spyOn(document, 'visibilityState', 'get')
      render(
        <NotifyStrip
          urgent={[]}
          arrival={item('h')}
          onArrivalDone={onArrivalDone}
          onOpen={() => {}}
        />,
      )

      visibility.mockReturnValue('hidden')
      act(() => {
        document.dispatchEvent(new Event('visibilitychange'))
      })
      await tick(60_000)
      expect(onArrivalDone).not.toHaveBeenCalled()

      visibility.mockReturnValue('visible')
      act(() => {
        document.dispatchEvent(new Event('visibilitychange'))
      })
      await tick(ARRIVAL_MS)
      await tick(FADE + CLOSE)
      expect(onArrivalDone).toHaveBeenCalledExactlyOnceWith('h')
    })

    it('holds its clock while the strip cannot be seen', async () => {
      const onArrivalDone = vi.fn()
      const { rerender } = render(
        <NotifyStrip
          urgent={[]}
          arrival={item('h')}
          onArrivalDone={onArrivalDone}
          onOpen={() => {}}
          held
        />,
      )
      await tick(60_000)
      expect(onArrivalDone).not.toHaveBeenCalled()

      rerender(
        <NotifyStrip
          urgent={[]}
          arrival={item('h')}
          onArrivalDone={onArrivalDone}
          onOpen={() => {}}
        />,
      )
      await tick(ARRIVAL_MS)
      await tick(FADE + CLOSE)
      expect(onArrivalDone).toHaveBeenCalledExactlyOnceWith('h')
    })

    it('opens, then closes early, when chosen', async () => {
      const onOpen = vi.fn()
      const onArrivalDone = vi.fn()
      render(
        <NotifyStrip
          urgent={[]}
          arrival={item('h')}
          onArrivalDone={onArrivalDone}
          onOpen={onOpen}
        />,
      )
      fireEvent.click(screen.getByRole('button', { name: 'open' }))
      expect(onOpen).toHaveBeenCalledExactlyOnceWith('h')
      await tick(FADE + CLOSE)
      expect(onArrivalDone).toHaveBeenCalledExactlyOnceWith('h')
    })
  })

  it('changes nothing while held, and catches up when the hold ends', async () => {
    const onOpen = () => {}
    const { rerender } = render(<NotifyStrip urgent={[item('u')]} onOpen={onOpen} />)

    // Held: the resolved line stays, and the arrival does not open.
    rerender(<NotifyStrip urgent={[]} arrival={item('h')} onOpen={onOpen} held />)
    await tick(FADE + CLOSE)
    expect(urgentLine()).not.toBeNull()
    expect(document.querySelector('.ns-high')).toBeNull()

    rerender(<NotifyStrip urgent={[]} arrival={item('h')} onOpen={onOpen} />)
    await tick(FADE + CLOSE)
    expect(urgentLine()).toBeNull()
    expect(document.querySelector('.ns-high')).not.toBeNull()
  })

  it('renders no buttons when read-only', () => {
    render(
      <NotifyStrip
        urgent={[item('a', { action: { label: 'Fix it', onSelect: () => {} } }), item('b')]}
        arrival={item('h')}
        onOpen={() => {}}
        interactive={false}
      />,
    )
    expect(screen.queryAllByRole('button')).toEqual([])
    expect(screen.getByText('1 of 2')).toBeInTheDocument()
  })

  it('under reduced motion, animates no height and steps the clock once a second', () => {
    setReducedMotion(true)
    render(<NotifyStrip urgent={[item('a')]} arrival={item('h')} onOpen={() => {}} />)

    expect(animations.some((a) => a.keyframes.some((k) => 'height' in k))).toBe(false)
    const clock = animations.find((a) => a.keyframes.some((k) => 'transform' in k))
    expect(clock?.options.easing).toBe(`steps(${ARRIVAL_MS / 1000})`)
  })

  it('is a polite live region before any line arrives, and a named region once one does', () => {
    const { container, rerender } = render(<NotifyStrip urgent={[]} onOpen={() => {}} />)
    const strip = container.querySelector('.ns')
    expect(strip).toHaveAttribute('aria-live', 'polite')
    expect(screen.queryByRole('region')).not.toBeInTheDocument()

    rerender(<NotifyStrip urgent={[item('a')]} onOpen={() => {}} />)
    expect(screen.getByRole('region', { name: 'Notifications' })).toBe(strip)
  })
})
