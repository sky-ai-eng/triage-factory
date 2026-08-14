# Acquire

A value arriving under attention.

Four beats, fixed: **frame** (reserved, empty), **flash** (filled once — the
space is claimed), **frame** (empty again, so the flash reads as a single
strike), **cross** (a wireframe X drawn corner to corner). Then the value lands.

## It is not a loading mask

The sequence runs for the same length of time whether the data is already in
hand or still in flight. Its job is to point at one figure, not to fill time.

Two consequences, both deliberate. A fast response does not cut the sequence
short — a figure appearing during the flash makes the whole gesture look like a
glitch. And a slow response does not stretch it: when the beats are done and the
answer is not there, the box hands off to a skeleton. That is a different
statement. The cross said *look here*; the skeleton only says *still working*.

**The skeleton is the shape of the answer, not the reserved box carried over.**
Money waits as `$--.--`, a count as `--`, a name as a short bar. A rectangle
that persists past the sequence reads as the sequence stalling, when what
actually happened is that the sequence finished and the call did not. Pass it as
`skeleton`, per field.

The pulse is the one repeating animation in this system, and it stops the moment
the value lands.

## When to use it

For a figure worth pointing at as it arrives: today's spend in the identity
panel, a total that just changed, a number the user came to this surface for.
One per surface, at most. Everything that arrives under a cross is being
called important, and a screen where six things are important has said nothing.

Not for lists, not for whole pages, and never for something that changes on its
own — a count that ticks by itself is an event, and events are marked once by
their own rule, not acquired.

A page's own loading state is a skeleton, not this: a skeleton describes the
shape of a whole screen, and every page owns its own. Acquire is one figure.

## Rules

The flash is ink, never warm. Warm is the measurement layer, and claiming space
is not a measurement.

The box is reserved space, not the value in hiding. An unresolved figure has no
width to promise, so give it a fixed one and let the resolved value size itself.

The cross is drawn, corner to corner, and it is the placeholder mark — the same
X a wireframe puts in a box whose contents are not yet known. It draws rather
than appearing because the box is being measured out.

## Why the phases are JS, not CSS

The first version of this lived in keyframes with delays, and failed twice: a
delayed animation with `both` fills *backwards* through its delay, so "wait,
then flash" meant "flash from the first frame"; and a stray brace elsewhere in
the stylesheet silently dropped the cross rule, which is invisible until
someone stares at a box that never marks. Timers and classes cannot fail either
way, and the phase is inspectable as `data-phase`.

## Props

`value` — the resolved value, or `null` while the call is out. Rendering is
props-only: `Acquire` never fetches, and the caller owns the request.
`width` / `height` — the reserved box, in px. Default 54 × 12.
`align` — `'right'` (default) or `'left'`.
`outline` — hairline box during the sequence, dropped on resolve. Default true.
`skeleton` — what waiting looks like: the shape of the answer for this field.
`phase` — force a phase. For documentation and tests only; never in a screen.

Re-running the sequence is a remount: change the element's `key`. The component
deliberately has no replay method, because a value that re-acquires itself on
change is an ambient loop wearing a costume.

## Changed from the design bundle

Two things, both mechanical:

- **Alignment is `data-align`, not a match on the inline style string.** The
  bundle found the left-aligned case with `[style*='flex-start']`, which stops
  working the moment a caller passes any other inline style. Same rendering,
  and it follows the house rule that state is a data attribute.
- **`aria-busy` while the sequence runs.** The one fact assistive tech can act
  on is whether the figure has resolved. Deliberately *not* a live region —
  this component is ruled out above for anything that changes on its own, so
  there is nothing here worth interrupting a reader to announce.

## Unreachable

`failed` is the third ending. The sequence runs as always, then the box shows the
shape of the answer struck out — and it does not move. Waiting breathes because
something is still happening; a failed read is inert because nothing is. That
difference is the only thing distinguishing "slow" from "gone", so never animate
the failed state and never pulse it.

## Reduced motion

The sequence is JS-timed — `setTimeout`, not `@keyframes` — so the blanket rule in
`tokens/motion.css` cannot reach it, and it ran in full under
`prefers-reduced-motion: reduce` until it read the preference itself.

Under the preference there are **no beats at all**: the component mounts straight
into its end state — the value if it is there, the answer-shaped skeleton if it is
not, the struck state if it cannot arrive. The end state is the information; the
four beats are the attention, and attention is the thing being asked for less of.
Nothing is shortened or sped up, because a sequence at double speed is still a
sequence.
