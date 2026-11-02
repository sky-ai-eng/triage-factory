// A Web Animations stand-in, imported only by tests. jsdom implements no WAAPI,
// and a component whose timer IS an animation's `finished` (the toast log's
// clock, the notification strip's arrival) needs one that runs on vitest's
// fake timers: it pauses, resumes with the time it had left, and rejects on
// cancel, which is all those components ask of a real one.

export class FakeAnimation {
  playState: AnimationPlayState = 'running'
  readonly finished: Promise<FakeAnimation>
  readonly keyframes: Keyframe[]
  readonly options: KeyframeAnimationOptions
  private resolve!: (a: FakeAnimation) => void
  private reject!: (e: unknown) => void
  private left: number
  private since = 0
  private timer: ReturnType<typeof setTimeout> | undefined

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

  // Cancelling a finished animation drops the values it was holding (a close's
  // `fill: forwards`) without rejecting anything: its promise has resolved.
  cancel() {
    if (this.playState === 'idle') return
    const pending = this.playState !== 'finished'
    clearTimeout(this.timer)
    this.playState = 'idle'
    if (pending) this.reject(new DOMException('cancelled', 'AbortError'))
  }
}

/**
 * Replace Element.prototype.animate with FakeAnimation, and return the list
 * every animation it creates is pushed to, so a test can ask what was
 * animated as well as when. Call it in beforeEach, under fake timers.
 */
export function installFakeAnimations(): FakeAnimation[] {
  const made: FakeAnimation[] = []
  Element.prototype.animate = function (keyframes, options) {
    const a = new FakeAnimation(keyframes as Keyframe[], options as KeyframeAnimationOptions)
    made.push(a)
    return a as unknown as Animation
  }
  return made
}
