# FlapCount

A running total changing. One window per digit, and only the digits whose face
actually turns get a roll and a frame.

```tsx
import { FlapCount } from '../../ui/flapcount/FlapCount'

<FlapCount value={events} size={24} label={events + ' events triaged'} />
<FlapCount value={backlog} size={27} format={compactCount} label={backlog + ' open pull requests'} />
```

## Why this exists alongside Ticker

`ui/odometer` already has two instruments for figures in motion, and this is
not a third opinion about how a number should move — it borrows all of their
mechanics. What it changes is scope.

`Ticker` frames the whole figure and rolls all of it. That is right when the
figure **is** the news: one arrival, one frame, read once.

A running total is not that. `312 → 335` moved two digits; the leading `3` did
not. Framing all three claims more change than happened, and on a figure that
updates every few seconds it reads as the number being **replaced** rather than
incremented. A departure board with two flaps turning is unmistakably a board
where two things changed.

So: `Ticker` for an arrival, `FlapCount` for an increment. Neither is a
replacement for the other, and a page should not use both on the same figure.

## What moves

Per digit, comparing the incoming value to the outgoing one:

- **Changed** — a window one digit high, a strip carrying the old face then the
  new one, rolling `0.62s cubic-bezier(.22, 1, .36, 1)`, and a hollow warm
  frame that appears and fades.
- **Unchanged** — static text. No window, no frame, nothing.

Digits start **0.06s apart, left to right** — the house stagger.

**The flaps roll with the direction of travel** — up on an increment, down on a
decrement. A flap that always turns one way says "changed" and nothing else;
rolling with the sign lets a backlog shrinking look like a backlog shrinking
without reading a digit. The whole row turns together, because a board does not
flap half its flaps one way. The strip's child order swaps with the direction so
both end on the incoming glyph.

The frame is hollow and warm, never a filled strike: at this size a solid block
reads as an alarm and this is an increment. It leaves on its own, because an
outline that stayed would become part of the number.

## When the column count changes

`99 → 103` shifts every position, so **every digit flaps**. There is no honest
way to call one unmoved when the column it sits in has changed meaning. The new
leading column rolls up from **blank**, not from a zero that was never on the
board.

## Rules taken from Odometer.md

Not reinvented — two instruments that disagree about what a changing figure
looks like is worse than either one being wrong.

- The beat is derived from the prop **during render**, not in an effect. An
  effect runs after paint, so the first frame would show the new digit and the
  roll would start from underneath it — a correction, not a change.
- Re-running is a **remount**, keyed on a beat count.
- Rolled digits are `aria-hidden` scenery inside a labeled wrapper.
- `value={null}` draws an em dash and claims no role. An offline readout uses
  this rather than the last number it happened to hold.
- Reduced motion writes the end state directly under `[data-still]` — the
  motion blanket alone would leave a `both`-filled strip on the value it used
  to hold, a wrong answer rather than absent motion. Each roll direction rests
  somewhere different, so each writes its own. The frame needs no branch: it
  starts and ends at opacity 0.

## Nothing rolls on first paint

The first render has no previous value, so no digit is "changed" and the figure
is plain text. The first frame a reader sees is not news.

## `format` separates the beat from the glyphs

The beat tracks the number, so a caller can abbreviate — 1249 to `1.2k` — and
still get a correct diff, because the comparison deciding which flaps turn runs
on the strings a reader actually sees. `CratePile` uses it to bound its figure
at four glyphs.

## `--flap-ink`

The figure's ink is overridable by the caller's own custom property — a zero
backlog or an offline readout is not the same claim as a live one, and the
caller should not have to out-specify a rule on the element itself.

## Where it is used

The Overview, on both figures that change while someone is watching: the
`SINCE MIDNIGHT` headline (passed to `Converge` as `titleNode` — the decode
scrambles a _string_, so a self-animating figure cannot live in `title`), and
the standing pull-request backlog inside `CratePile`. Two counts moving by two
different mechanics would be the page disagreeing with itself.
