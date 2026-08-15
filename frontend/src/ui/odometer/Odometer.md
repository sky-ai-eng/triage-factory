# Odometer · Ticker

Two readings of one mechanism — digits behind a window one digit high — kept in
one folder because a source page uses them together and they must agree about
what a figure looks like.

```tsx
import { Odometer, Ticker } from '../../ui/odometer/Odometer'

<Odometer value={33} label="33 tracked" at={0.2} width={62} />
<Odometer value={40} suffix="s" label="40 seconds" at={0.52} width={62} />
<Ticker value={tasks} />
<Ticker value={tasks} variant="row" />
```

## Odometer — a value arriving

Each digit is a 30-step ramp (0–9, three times) that runs past its window and
stops on the answer. The landing is on the **third** pass, so every digit has
spun past its own face twice before it settles: a `1` that travelled one step
reads as a correction, not as an arrival.

Digits start 0.06s apart, left to right, from the `at` the caller gives. A
column of figures on one page therefore has a single schedule — the page owns
the `at` values and the component owns the stagger inside one figure.

**It is not a loading mask.** Mount it when the answer is in hand. A ramp that
rolls to nothing and then jumps to the real number is worse than a dash that
becomes a number, which is what `value={null}` draws.

## Ticker — a value changing

`became tasks` is not allowed to change quietly. The figure travels one line
height inside a hollow warm frame that appears and fades — no filled strike, on
a 24px figure that reads as an alarm and this is an increment.

`variant="head"` rolls and flashes, on arrival and on every change after it.
`variant="row"` is the same figure in a column of figures: it does not travel,
because the band above already showed the movement, and it does not flash on
arrival — the first frame a reader sees is not news.

Re-running is a **remount**: the window and the frame are keyed on a beat count.
That is the same answer `Acquire` gives for replaying a sequence. (The design
bundle alternates between two identical keyframe names to the same end; a key is
the mechanism this codebase already has.)

The beat is derived from the prop **during render**, not in an effect. An effect
runs after paint, so the first frame would show the new figure and the roll would
then start from underneath it — the movement would read as a correction of
something already on screen.

## Neither of these is Acquire

`Acquire` points at one figure as it lands, and its own `.md` rules it out for
"something that changes on its own — a count that ticks by itself is an event,
and events are marked once by their own rule". These are the two cases it names
and declines: a column of figures resolving together, and a count that ticks.

## Accessibility

**Rolled figures are `aria-hidden` scenery with a labeled wrapper.** The ramp is
30 stacked digits; without `role="img"` and an `aria-label` on the wrapper, a
page's primary statistics read as `0123456789…` to a screen reader and to
anyone copying them. The suffix is inside the label (`"40 seconds"`), not read
as a separate character.

`value={null}` draws a plain em dash and claims no role: there is nothing to
announce about a measurement nobody has taken.

## Reduced motion

**Skip to the end state**, the first of the three answers in `CONVENTIONS.md`.

Both instruments are CSS-animated, so `tokens/motion.css` already blankets them
— and that blanket is exactly the problem. An animation with `both` that never
runs leaves the element at its **from** state: the ramp would sit on the digit
`0` and the ticker's strip on the value it used to hold. Both are wrong answers
rather than absent motion. So each reads the preference itself and writes the
end state directly, under `[data-still]`.

The frame is the exception and needs no branch: it starts at `opacity: 0` and
returns there, so a flash that never runs is simply a flash that did not happen.
