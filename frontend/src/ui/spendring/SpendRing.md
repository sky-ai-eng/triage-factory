# SpendRing

A spend total that decomposes in its own hole on hover. **No legend, ever.**

```tsx
import { SpendRing } from '../../ui/spendring/SpendRing'

;<SpendRing models={byModel.map((m) => ({ name: m.model, v: m.cost }))} href={usageHref} />
```

## The legend is the design decision

A permanent legend is a _promise of content_. On a zero-spend day it renders
blank rows and reads as broken rather than as quiet — which is exactly how this
component came to exist.

So the split moved into the ring's own hole. Hovering a band swaps the total
for that model's figure and names it. One readout, two things it can say, and
nothing reserved for a list that may have nothing in it. At zero there is
nothing to hover, so nothing is missing.

## Why a flat ring and not Pie

`Pie` is an extruded isometric slab with its own legend, and it wants a whole
column's width. This has to sit in a live band beside a row list.

## What moves

The hovered band **thickens**, 14 to 17. Colour already carries which model it
is, so weight is the only channel free. The others drop to 0.38 opacity, and
the hole switches to that model's figure with its name under it, one ink step
brighter — an answer to the pointer rather than the standing label it replaced.

## The build

Each band draws on, 0.62s, 0.13s apart. It is a **transition** on
`stroke-dasharray`, not a keyframe: the target length is the same value hover
re-renders, so a keyframe would restart on every pointer crossing. It needs
**two frames** to start — the first paint lands with the dasharray at zero so
the browser has a value to transition from.

Under reduced motion the blanket removes the transition, which leaves each band
at its true length — the end state, and the right answer with no branch needed.

## A name too long for the hole

Fixed upstream, not in the layout — `shortModelName` strips the vendor prefix
and the build date, so `claude-sonnet-4-20250514` renders `SONNET-4`. The two
layout answers that were built and cut (two-line wrap, crossing the ring) are
catalogued in `shortModelName.ts` and `spendring.css`. Anything still too long
ellipses at 58% of the ring — the chord that clears the band at the caption's
own height. Pass `shorten={(s) => s}` if the names are already display names.

The open caption sits at **ink-2**, not ink-4: the standing label is a label
and can be faint; a name is an _answer_ to the pointer and has to be read.

## A bigger number, and a smaller ring

**Both readouts scale from `size`**, on two ratios. The caption is already near
the floor of legible mono, so it floors at **7.5px** — 7.5 rather than 8
because at 8 the caption came out proportionally _larger_ on a small ring and
"SPENT TODAY" ellipsed at 104px. The standing label has to fit at every size.

**The figure drops precision, never characters.** Cents below $100, none above:
at $4.20 the cents are the number, at $1,249 they are noise and two characters
the hole does not have. A clipped money figure would be a lie. **One rule at
every size** — dropping cents on the small ring only would mean a ring reads
differently at 104 than at 168, and then they are not the same component. Past
about seven characters the ring needs to be bigger; that is a caller decision,
and `format` is the override.

The caption names the timeframe (`SPENT TODAY`) because nothing else does —
this component ships with no section header over it, deliberately. A ring with
a dollar figure in its hole does not need the word TODAY above it as well.

## Hover is an accelerator, not the only route

There is no hover on touch and none on a keyboard, so a split reachable only by
pointer is a split most people never see. The ring is a link to the usage page,
where the same breakdown is the whole subject. Without an `href` or `onOpen` it
renders as a plain block — never an anchor to `#`.

## Offline is not zero

`--`, not the last figure it happened to hold. The rail's rule, applied here.

## Where it is used

The Overview's live band, closing the RUNNING row. Admin-gated there (see the
usage node's grant): a member sees the same band with the ring absent and the
rows across the full width — one fewer answer, not a second layout.
