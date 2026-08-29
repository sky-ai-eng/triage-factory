# Converge

Many to few. One instrument for the question "what happened to everything that
arrived?": events fan in from the left, resolve into a small number of named
outcomes, and the counts are the data.

```tsx
import { Converge } from '../../ui/converge/Converge'

;<Converge
  kicker="SINCE MIDNIGHT"
  title="events triaged"
  titleNode={<FlapCount value={events} size={24} label={events + ' events triaged'} />}
  outcomes={[
    { name: 'merged', v: 6, tone: 'warm' },
    { name: 'running', v: 3, tone: 'cool' },
    { name: 'need you', v: 7, tone: 'ask' },
    { name: 'filtered by rules', v: 296, tone: 'quiet' },
  ]}
  strands={28}
  height={260}
  fill
  endpointBand={[0.28, 1]}
  replayOnClick
/>
```

## The honesty rules

- **Strands are structure, not data.** Twenty-eight filaments stand for 312
  events; only the counts on the right are read. Allocation is **square-root
  scaled** (see `convergeParts.ts`): this product's healthy day is 95%
  filtered, and proportional allocation hands that outcome twenty-six of
  twenty-eight strands with no picture left. The floor of two applies to every
  outcome, zero included — a zero outcome keeps strands flowing into its
  endpoint, and the offline fan (all counts zeroed) keeps its shape.
- **The tail is the point.** In a healthy deployment most filaments land on the
  quiet outcome; that ratio is the instrument.
- **One cool outcome at most.** Cool means live; two cool endpoints means the
  emission layer is being used as a palette.
- **The build plays on arrival and on click, never on data change.** A value
  that ticks moves in place. Replay is cancel+play on the live Animation
  objects — never a remount or a gate drop, because every element rests at
  opacity 0 and any ungated frame is a frame with no instrument in it.
- **The endpoints radiate in a scattered order** — they resolved
  independently; a tidy cascade would imply a sequence that did not happen.
- **Labels do not decode.** Only the optional headline does, which keeps decode
  rationed to one per view.
- Four to six outcomes. Beyond that the endpoints stack and the fan reads as a
  gradient rather than a mapping.

## `height` is a viewBox unit — and `fill` is the other regime

By default the SVG is `height: auto` with `preserveAspectRatio="none"`, so its
rendered height is _width × (height / 740)_: a caller wanting a fixed pixel
height must measure the width and solve for the viewBox — JS in the resize
path, and a height that lands a frame after the width settles (a rail
animating open makes the fan snap once the motion finishes).

`fill` moves the height to CSS by positioning the component **absolutely into
a `position: relative` container with a definite height**. Not `height: 100%`:
that cannot resolve through a mount wrapper with no definite height, falls
back to `auto`, and an SVG at auto height takes its **intrinsic ratio** — the
fan then scales as a rectangle, so a rail toggle moves both axes. Two fixes
ride with it: the endpoint labels take a fixed 12.5px (their container-query
size swung 13.0 → 10.5px on a rail toggle — a number is read, and a read thing
holds its size), and the headline gets 46% width instead of 34% (it wrapped to
two lines below ~700px, and a headline changing line count on a toggle reads
as the component resizing). Note the two `.conv svg` rules tie on specificity,
so the fill override is declared _after_ the base one.

## `endpointBand`

`[start, end]` as a fraction of the padded plot, defaulting to `[0, 1]` —
arithmetically identical to having no band. It goes through `yFor`, the single
function strands, bars and labels all read, so a narrowed band cannot slide
labels off their own endpoints. It **compresses rather than translating** —
the last lane already sits near the plot floor, so there is nowhere to shift
the whole set down to. Narrowing makes the fan sweep toward one side rather
than splay evenly: a different drawing, worth looking at before shipping.

## The headline

`title` decodes — characters churn and settle left to right, in mono because a
proportional face changes width every frame and the line shimmies. It paints
above the plot and needs three things to stay legible, all in the CSS and none
optional: `z-index` on `.convtitle` (the strands otherwise paint over the
words — the symptom looks like contrast, the cause is paint order), a halo of
three ground-color blurs on the title, and the kicker at `ink-3` (its problem
is the ink, not the field). Dark mode is the case to check: the ink ramp
inverts and the strand tones do not.

**`titleNode`** is for a figure that animates itself. The decode scrambles a
_string_, so a self-updating count cannot live in `title` — every increment
would re-garble the whole sentence, and an increment is not a reveal.
`titleNode` renders before the decoded words and is left alone; it should
carry the headline's accessible label (the decoded span is `aria-hidden`
scenery, and the chart's own `role="img"` names the data).

## Reduced motion

Two answers, both required. The CSS build elements rest at their _from_ states
and the motion blanket is `animation: none !important` — which beats any
duration override — so the media branch in `converge.css` writes the **end
states** directly (`.charting` still gates them, so the fan appears on arrival
without beats). The decode is rAF-driven and invisible to the blanket, so
`decodeInto` reads the preference itself and writes the final string at once.
