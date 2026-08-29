# CratePile

A standing count, drawn as a 2.5D pile.

```tsx
import { CratePile } from '../../ui/cratepile/CratePile'

<CratePile
  count={openPRs}
  caption="open pull requests"
  captionOne="open pull request"
  href={prsHref}
/>
```

## What it is for

A quantity that **accumulates** rather than resets: open pull requests, a
backlog, anything that was there yesterday and will be there tomorrow.

This matters because of where it usually sits. Put a standing figure among
event figures — spend today, events triaged since midnight — and it gets read
as an event count. "23 pull requests happened today" is wrong and quietly
alarming. The pile is what says *this one is a different kind of figure*, and
it says it without a word.

## The pile is texture with a floor, not a tally

Nobody is meant to count 23 crates. They are meant to see a pile bigger than
Friday's. That is the difference from one tile per item, which invites counting
and then breaks at thirty. It has a floor, and the floor is the point: growth
has a direction, so a bigger backlog is unmistakably *more stuff* rather than a
longer line.

## Projection

The Pie's, not a new one — tilt 0.44, about 26 degrees, top face lightest and
the two sides stepped down, so this and the spend ring look like the same hand
drew them. No fourth face and no back; at this angle they are never seen.

Faces are mixed toward **the ground**, never toward transparent. A crate has to
be opaque or the pile behind shows through and the whole stack reads as one
translucent smear. A hairline in the ground colour separates them, or two
adjacent top faces at the same shade merge into a plateau. Higher crates read
lighter, so the pile has a light source.

## Two things that were bugs

**Painter order is `(i + j + k)` ascending.** A cube with a lower sum can never
occlude one with a higher sum, so one sort covers every overlap with no depth
testing.

**Crates are keyed on the cell, not on array position.** This is load-bearing.
A crate added at the top of the pile has a *low* painter sum — (0,0,2) sums to
2 — so it sorts near the front of the array. Keyed by index, every crate after
it shifted one slot, React re-mounted them all, and the entrance re-ran from
opacity 0 behind its stagger delay. A count going **up** made an existing crate
vanish for a beat; going down was clean, because removal takes the highest sum,
which is already last.

**Empty is measured off the full footprint**, not off one crate. A pallet with
nothing on it is the same pallet, so zero renders at the scale every other
count does — a dashed 3×3 outline, which reads as *nothing waiting* rather than
as a graphic that failed to load.

## The figure is bounded at four glyphs

At 27px mono each glyph is about 16px and `FlapCount` puts 3px between them, so
four is 74px — five would not fit beside the pile at any sensible width.
`compactCount` is exact under a thousand (a backlog of 340 is a number someone
acts on), one decimal to ten thousand (`1.2k`), whole thousands past that. It
is passed to `FlapCount` as `format`, which keeps the beat on the number and
the glyphs on the abbreviation — so `1.2k` → `1.3k` flaps one glyph.

The width cap belongs to the **caption**, not the column: on the column it
bound the figure too, and a three-glyph count ran straight out of its own box
and over the pile.

## The pile saturates at 36

Height comes from the occupied cells while `width` stays fixed, so an uncapped
pile climbs forever — at twelve layers it draws about 181px tall in a 98px-wide
box. Four layers is the cap, for the texture-not-tally reason above: *a lot* is
the most a pile can honestly say, and past 36 the figure carries the number
alone, which it was already doing. A surface that genuinely needs to
distinguish 40 from 90 wants a bar, not a pile.

## The entrance

Crates fade in bottom-up, 18ms apart, on first build only. A crate arriving
later is one event, not a sequence: inheriting a 0.4s delay would make a single
increment look like a pause before it landed.

The figure is `FlapCount`, so an increment flaps only the digits that changed —
two counts on one page moving by two different mechanics would be the page
disagreeing with itself. The block sets `--flap-ink` to gray the figure at zero
or offline.

## Navigation

With an `href` or `onOpen` the whole block is a link. Without either it renders
as a plain span — never an anchor to `#`.

## Reduced motion

No branch needed: the entrance ends at the resting position, so the motion
blanket removing it leaves every crate where it belongs.

## Where it is used

The Overview's live band, leading the NEEDS YOU row with the open pull-request
count.
