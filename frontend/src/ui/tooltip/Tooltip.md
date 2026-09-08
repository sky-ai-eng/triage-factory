# Tooltip

The project's one tooltip: the definition of a label, never the value of a
datum.

```tsx
import { Tooltip } from '../../ui/tooltip/Tooltip'

<Tooltip content="A review was requested from you on this pull request.">
  <span className="badge">Review requested</span>
</Tooltip>

<Tooltip content="3 runs ahead of this one" focusable={false}>
  <span role="img" aria-label="3 runs ahead of this one">…</span>
</Tooltip>
```

## One treatment, one delay

This is the shipped rail's surface — radius 4, 11px sans, `ink-1`, the float
edge and shadow, **no arrow**. An arrow is a pointer to the thing the tooltip is
already touching. The project had three tooltips and no decision between them
(the rail's, Radix content styled per page, and the proposal layer's tokened
Radix look); the rail's is the least chrome that still reads as a surface, so it
won and the others were removed.

`TOOLTIP_DELAY` (200ms) is exported and is the delay everywhere. Per-page delays
ranging 150–650ms are three different products to anyone who opens more than one
page. Focus opens with **no** delay: a delay guards against a pointer crossing
the screen, and a keyboard never crosses anything. Keyboard focus only, though —
a pointer's focus is a side effect of its press, and the pointer already has its
own routes in (hover, and the tap). Opening on it too made the press a flash:
focus opened the hint and the click half of the same press toggled it shut.

## Definitions, not data

A value the reader needs belongs on the page, or annotated with a leader line.
The test is what is missing: a number absent from the page wants a leader line;
a number **on** the page whose unit is missing — a warm `3` beside an hourglass —
is a label wanting its definition, and that is this instrument.

`wrap` is off by default. A hint that needs a paragraph is documentation, and
one that wraps unpredictably inside a dense row changes the row's height on
hover. It caps at 220px; pass a number for the rare hint that is a small panel
rather than a phrase.

## Two modes, and the wrong one is an accessibility bug

**`focusable` (default `true`)** — the trigger is the interactive thing. It
takes a tab stop and `aria-describedby`, and the tooltip is a real
`role="tooltip"`.

**`focusable={false}`** — the trigger sits _against_ something already
interactive: a mark inside a row that is itself an `<a>`, or a host wrapped
around one. A tab stop there is invalid or redundant, so the host takes
neither and the tooltip becomes `aria-hidden` scenery. The hint still follows
the interactive thing's own focus — focus bubbles, so a host wrapping an
anchor opens when the anchor is tabbed to, and the hint is never mouse-only.
**The caller still owes the same words to assistive technology by another
route** — `role="img"` plus an `aria-label` on the mark, or `aria-describedby`
on the anchor — because a visually-opening scenery hint is silent. A visual
echo of text that is present anyway is not announced twice — the same answer
`Table.md` gives for its clipped-cell tooltip.

## A tap toggles it

There is no hover on touch, so a tap is the only route to the hint. The host
stops its own click — load-bearing inside a link, where the tap would otherwise
navigate and leave touch with no route to the hint at all.

## Positioning

Not portaled: the bubble stays a child of its host in the DOM, which keeps the
component free of app machinery, keeps `aria-describedby` pointing at a node
under the trigger, and keeps focus bubbling through the host in scenery mode.
It escapes clipping anyway: the bubble is a `popover="manual"` shown from a
layout effect on open, which puts it in the browser's **top layer** — a paint
order, not a DOM move — where no ancestor's `overflow` can cut it off. Manual
rather than `auto`, because `auto` light-dismisses and joins the popover
stack, so it would take an Escape meant for a dialog underneath. The position
is the trigger's rect on the page, measured by whichever event opened the
hint, plus a transform that centers the bubble without knowing its size.

Two alternatives were rejected and should not be substituted. A **portal**
relocates the node and complicates both of the wiring points above. Bare
**`position: fixed`** stops escaping the clip under any ancestor with
`transform`, `filter`, `backdrop-filter`, `contain` or `will-change` — callers
have these, so it would pass review and break in place.

Where the popover API is missing the bubble is an ordinary absolute child of
its host, clipped as before; the floor does not move.

**The hint closes on a scroll that moves the trigger, and on resize** — a
scroll inside any scroller the trigger sits in, not just the window's. Its
position and edge correction are measured once on open, so once the trigger
moves they are stale, and a hint is only open under a live pointer or a focused
trigger, so a scroll that moves it is the reader leaving it. Re-measuring per
frame would cost a rect per frame to keep a hint nobody is looking at. Each
scroll event compares one rect against the one taken at open rather than
closing blind, because focusing an off-screen trigger scrolls it into view and
that scroll's event fires a frame after the focus that opened the hint —
closing on it leaves a keyboard user tabbing down a list with no hint at all.

## It stays on the page

The bubble is centered on its trigger, and a trigger near either edge of the
page puts half of it outside the window, where it cannot be read. So the
position is measured once on open and corrected, in the order of how much the
correction changes the tooltip:

- a **shift** along the page for a `top`/`bottom` bubble — it stays where it
  was pointing and only slides clear of the edge;
- a **flip** to the opposite side for `left`/`right`, which cannot slide
  sideways without covering the thing it names;
- a **wrap** if the content is wider than the page can hold at any position,
  since then there is nowhere to slide it to and the line has to break.

Nothing to pass; a tooltip that already fits is untouched and animates exactly
as before. The shift is applied as `--tip-dx` as well as an inline transform,
because the entrance keyframes restate the resting transform and outrank an
inline style for the whole run — written inline alone, a corrected bubble
animates in centered and jumps sideways at the end. The keyframes read the
variable, defaulting to `0px`, and `.tip` carries `max-width: calc(100vw -
16px)` as a floor for any tooltip mounted without the measurement.

## Reduced motion

Nothing to write back: the entrance has no fill mode, so the motion blanket
removing the animation leaves the tooltip at its resting position.
