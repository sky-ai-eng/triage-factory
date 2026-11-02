import { useState } from 'react'
import { render, screen, act, fireEvent, within } from '@testing-library/react'
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import ToastLog, { type ToastLevel, type ToastLogItem } from './ToastLog'

// The clock line is the timer, so what gets pinned is when a row leaves and
// what holds it: its level's lifetime, then the exit, then onDone — and
// nothing in between that dismisses early or holds forever.
//
// jsdom implements no Web Animations, so Element.prototype.animate is a fake
// whose `finished` resolves on the (fake) timers: it pauses, resumes with the
// time it had left, and rejects on cancel, which is all the component asks of
// a real one. Every animation it creates is kept, so a test can ask what was
// animated as well as when.

const LIFETIME: Record<ToastLevel, number> = {
  success: 3000,
  info: 3000,
  warning: 6000,
  error: 10000,
}
const FADE = 120
const CLOSE = 160
const EXIT = FADE + CLOSE

class FakeAnimation {
  playState: AnimationPlayState = 'running'
  readonly finished: Promise<FakeAnimation>
  private resolve!: (a: FakeAnimation) => void
  private reject!: (e: unknown) => void
  private left: number
  private since = 0
  private timer: ReturnType<typeof setTimeout> | undefined
  readonly keyframes: Keyframe[]
  readonly options: KeyframeAnimationOptions

  constructor(keyframes: Keyframe[], options: KeyframeAnimationOptions) {
    this.keyframes = keyframes
    this.options = options
    this.left = Number(options.duration ?? 0)
    this.finished = new Promise((res, rej) => {
      this.resolve = res
      this.reject = rej
    })
    // A real animation's finished promise is marked handled, so cancelling one
    // nobody awaited is not an unhandled rejection.
    this.finished.catch(() => {})
    this.run()
  }

  private run() {
    this.since = Date.now()
    this.timer = setTimeout(() => {
      this.playState = 'finished'
      this.resolve(this)
    }, this.left)
  }

  pause() {
    if (this.playState !== 'running') return
    clearTimeout(this.timer)
    this.left -= Date.now() - this.since
    this.playState = 'paused'
  }

  play() {
    if (this.playState !== 'paused') return
    this.playState = 'running'
    this.run()
  }

  cancel() {
    if (this.playState === 'idle' || this.playState === 'finished') return
    clearTimeout(this.timer)
    this.playState = 'idle'
    this.reject(new DOMException('cancelled', 'AbortError'))
  }
}

let animations: FakeAnimation[] = []

/** Exits begun so far: an exit is the only animation that fades 1 → 0. */
function exits() {
  return animations.filter(
    (a) => a.keyframes[0]?.opacity === 1 && a.keyframes.at(-1)?.opacity === 0,
  ).length
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

/** Advance the clocks and let every promise and render they trigger settle. */
async function tick(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms)
  })
}

function item(id: string, level: ToastLevel = 'success', extra: Partial<ToastLogItem> = {}) {
  return { id, level, body: `body ${id}`, at: Date.UTC(2026, 10, 1, 14, 32), ...extra }
}

function escape(target: Element | Window = window) {
  fireEvent.keyDown(target, { key: 'Escape' })
}

/** The store's side of the contract: a row is removed at onDone, and only then. */
function Store({ initial, onDone }: { initial: ToastLogItem[]; onDone: (id: string) => void }) {
  const [items, setItems] = useState(initial)
  return (
    <ToastLog
      items={items}
      onDone={(id) => {
        onDone(id)
        setItems((xs) => xs.filter((x) => x.id !== id))
      }}
    />
  )
}

describe('ToastLog', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    setReducedMotion(false)
    animations = []
    Element.prototype.animate = function (keyframes, options) {
      const a = new FakeAnimation(keyframes as Keyframe[], options as KeyframeAnimationOptions)
      animations.push(a)
      return a as unknown as Animation
    }
    // jsdom's document never has focus; the log would read every test as the
    // viewer being away and hold every clock.
    vi.spyOn(document, 'hasFocus').mockReturnValue(true)
  })
  afterEach(() => {
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  it('draws the newest row on top', () => {
    render(<ToastLog items={[item('a'), item('b'), item('c')]} onDone={() => {}} />)
    expect(screen.getAllByRole('listitem').map((li) => li.textContent)).toEqual([
      expect.stringContaining('body c'),
      expect.stringContaining('body b'),
      expect.stringContaining('body a'),
    ])
  })

  it.each(Object.entries(LIFETIME) as Array<[ToastLevel, number]>)(
    'runs a %s row for %ims, then its exit, then reports it done',
    async (level, lifetime) => {
      const onDone = vi.fn()
      render(<ToastLog items={[item('a', level)]} onDone={onDone} />)

      await tick(lifetime - 1)
      expect(exits()).toBe(0)
      await tick(1)
      expect(exits()).toBe(1)

      // The store removes the row at onDone, so it must not fire while the
      // row is still fading or closing.
      await tick(EXIT - 1)
      expect(onDone).not.toHaveBeenCalled()
      await tick(1)
      expect(onDone).toHaveBeenCalledExactlyOnceWith('a')
    },
  )

  describe('holds the clock', () => {
    it('while the pointer is over the panel', async () => {
      render(<ToastLog items={[item('a', 'error')]} onDone={() => {}} />)
      const panel = screen.getByRole('region', { name: 'Messages' })

      await tick(4000)
      fireEvent.pointerEnter(panel)
      await tick(60_000)
      expect(exits()).toBe(0)

      // It resumes with the time it had left, not a fresh lifetime.
      fireEvent.pointerLeave(panel)
      await tick(5999)
      expect(exits()).toBe(0)
      await tick(1)
      expect(exits()).toBe(1)
    })

    it('while focus is inside the panel', async () => {
      render(
        <ToastLog
          items={[item('a', 'warning', { action: { label: 'open run', href: '/runs/7f3a21' } })]}
          onDone={() => {}}
        />,
      )
      const link = screen.getByRole('link', { name: 'open run' })

      act(() => link.focus())
      await tick(60_000)
      expect(exits()).toBe(0)

      act(() => link.blur())
      await tick(LIFETIME.warning)
      expect(exits()).toBe(1)
    })

    it('while the tab is hidden', async () => {
      const visibility = vi.spyOn(document, 'visibilityState', 'get')
      render(<ToastLog items={[item('a')]} onDone={() => {}} />)

      visibility.mockReturnValue('hidden')
      act(() => {
        document.dispatchEvent(new Event('visibilitychange'))
      })
      await tick(60_000)
      expect(exits()).toBe(0)

      visibility.mockReturnValue('visible')
      act(() => {
        document.dispatchEvent(new Event('visibilitychange'))
      })
      await tick(LIFETIME.success)
      expect(exits()).toBe(1)
    })
  })

  it('sends the oldest row out early once a sixth arrives', async () => {
    const onDone = vi.fn()
    const five = ['a', 'b', 'c', 'd', 'e'].map((id) => item(id))
    const { rerender } = render(<ToastLog items={five} onDone={onDone} />)

    rerender(<ToastLog items={[...five, item('f')]} onDone={onDone} />)
    await tick(EXIT)
    expect(onDone).toHaveBeenCalledExactlyOnceWith('a')
  })

  it('dismisses the newest row on Escape, the next on a second press, and never on a click', async () => {
    const onDone = vi.fn()
    render(<Store initial={[item('a'), item('b'), item('c')]} onDone={onDone} />)

    escape()
    await tick(EXIT)
    expect(onDone.mock.calls).toEqual([['c']])

    escape()
    await tick(EXIT)
    expect(onDone.mock.calls).toEqual([['c'], ['b']])

    // A click has to stay free for selecting the text.
    fireEvent.click(screen.getByText('body a'))
    await tick(EXIT)
    expect(onDone.mock.calls).toEqual([['c'], ['b']])
  })

  describe('leaves Escape alone', () => {
    it('when something nearer already handled it', async () => {
      const onDone = vi.fn()
      render(<ToastLog items={[item('a')]} onDone={onDone} />)

      const e = new KeyboardEvent('keydown', { key: 'Escape', cancelable: true })
      e.preventDefault()
      act(() => {
        window.dispatchEvent(e)
      })
      await tick(EXIT)
      expect(onDone).not.toHaveBeenCalled()
    })

    it('when it was typed in a field', async () => {
      const onDone = vi.fn()
      render(
        <>
          <input aria-label="Search" />
          <ToastLog items={[item('a')]} onDone={onDone} />
        </>,
      )

      escape(screen.getByRole('textbox', { name: 'Search' }))
      await tick(EXIT)
      expect(onDone).not.toHaveBeenCalled()
    })

    it('when the log is read-only, which also takes away the hover hold', async () => {
      const onDone = vi.fn()
      render(<ToastLog items={[item('a')]} onDone={onDone} interactive={false} />)

      escape()
      await tick(EXIT)
      expect(onDone).not.toHaveBeenCalled()

      fireEvent.pointerEnter(screen.getByRole('region', { name: 'Messages' }))
      await tick(LIFETIME.success - EXIT)
      expect(exits()).toBe(1)
    })
  })

  it('under reduced motion, animates no height and steps the clock once a second', async () => {
    setReducedMotion(true)
    const onDone = vi.fn()
    render(<ToastLog items={[item('a', 'success')]} onDone={onDone} />)

    escape()
    // An opacity change and nothing after it: no height to close.
    await tick(FADE)
    expect(onDone).toHaveBeenCalledExactlyOnceWith('a')

    expect(animations.some((a) => a.keyframes.some((k) => 'height' in k))).toBe(false)
    const clock = animations.find((a) => a.keyframes.some((k) => 'transform' in k))
    expect(clock?.options.easing).toBe('steps(3)')
  })

  it('lets go of focus when a focused row leaves, so the other rows run again', async () => {
    render(
      <ToastLog
        items={[
          item('a'),
          item('b', 'error', { action: { label: 'open run', href: '/runs/7f3a21' } }),
        ]}
        onDone={() => {}}
      />,
    )
    const link = screen.getByRole('link', { name: 'open run' })
    act(() => link.focus())

    // Unmounting a focused element fires no focusout. Without the row giving
    // focus up, 'a' would stay held by a focus that no longer exists.
    escape(link)
    expect(link).not.toHaveFocus()
    await tick(LIFETIME.success)
    expect(exits()).toBe(2)
  })

  it('starts the next toast running after the panel empties under the pointer', async () => {
    const { rerender } = render(<ToastLog items={[item('a')]} onDone={() => {}} />)
    fireEvent.pointerEnter(screen.getByRole('region', { name: 'Messages' }))
    escape()
    await tick(EXIT)

    // Nothing fires pointerleave for a panel that collapses from under a
    // pointer that has not moved.
    rerender(<ToastLog items={[]} onDone={() => {}} />)
    rerender(<ToastLog items={[item('b')]} onDone={() => {}} />)
    await tick(LIFETIME.success)
    expect(exits()).toBe(2)
  })

  it('is a polite live region before any row arrives, and announces an error as an alert', () => {
    const { rerender } = render(<ToastLog items={[]} onDone={() => {}} />)
    const list = screen.getByRole('list')
    expect(list).toHaveAttribute('aria-live', 'polite')
    expect(screen.queryByRole('region')).not.toBeInTheDocument()

    rerender(<ToastLog items={[item('a', 'error')]} onDone={() => {}} />)
    expect(screen.getByRole('list')).toBe(list)
    expect(within(list).getByRole('alert')).toHaveTextContent('Error: body a')
  })

  it('keeps a plain click on the action in-app and a modified click on the anchor', () => {
    const onSelect = vi.fn()
    render(
      <ToastLog
        items={[
          item('a', 'error', { action: { label: 'open run', href: '#run-7f3a21', onSelect } }),
        ]}
        onDone={() => {}}
      />,
    )
    const link = screen.getByRole('link', { name: 'open run' })
    expect(link).toHaveAttribute('href', '#run-7f3a21')

    expect(fireEvent.click(link)).toBe(false)
    expect(onSelect).toHaveBeenCalledTimes(1)

    expect(fireEvent.click(link, { metaKey: true })).toBe(true)
    expect(onSelect).toHaveBeenCalledTimes(1)
  })
})
