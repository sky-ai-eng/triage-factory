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
the screen, and a keyboard never crosses anything.

## Definitions, not data

A value the reader needs belongs on the page, or annotated with a leader line.
The test is what is missing: a number absent from the page wants a leader line;
a number **on** the page whose unit is missing — a warm `3` beside an hourglass —
is a label wanting its definition, and that is this instrument.

`wrap` is off by default. A hint that needs a paragraph is documentation, and
one that wraps unpredictably inside a dense row changes the row's height on
hover.

## Two modes, and the wrong one is an accessibility bug

**`focusable` (default `true`)** — the trigger is the interactive thing. It
takes a tab stop and `aria-describedby`, and the tooltip is a real
`role="tooltip"`.

**`focusable={false}`** — the trigger sits *inside* something already
interactive (a mark on a row that is itself an `<a>`). A tab stop there is
invalid — an anchor may not contain interactive content — and redundant, so the
host takes neither and the tooltip becomes `aria-hidden` scenery. **The caller
then owes the same words to assistive technology by another route**, usually
`role="img"` plus an `aria-label` on the mark. A visual echo of text that is
present anyway is not announced twice — the same answer `Table.md` gives for its
clipped-cell tooltip.

## A tap toggles it

There is no hover on touch, so a tap is the only route to the hint. The host
stops its own click — load-bearing inside a link, where the tap would otherwise
navigate and leave touch with no route to the hint at all.

## Positioning

Absolute against the trigger, not portaled. That keeps the component free of
app machinery, and it means a trigger inside an `overflow: hidden` ancestor can
clip its hint — if that happens, the fix is the trigger's container, not a
portal here.

## Reduced motion

Nothing to write back: the entrance has no fill mode, so the motion blanket
removing the animation leaves the tooltip at its resting position.
