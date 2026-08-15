# SourceChart

A fortnight of events, with the portion that became tasks stacked inside it.

```tsx
import SourceChart from '../../ui/sourcechart/SourceChart'

<SourceChart
  title="WHAT THE REPOSITORIES SENT"
  keys={['EVENTS', 'TASKS']}
  units={['events', 'tasks']}
  series={byDay}
/>
```

## The gap is the subject

Two series, and the reading is the distance between them: events nothing was
done about. Everything else follows from that.

**One scale, not two.** Tasks are drawn against the **events** maximum. Giving
each series its own peak would put a quiet week's tasks level with a busy week's
events, and the chart would say the opposite of what is true.

**Both figures come from one row.** The hover names the day and both numbers
together, so they must arrive together — a caller joining two series will
eventually draw a day where tasks exceed events.

## The build

One gesture, in two beats: the baseline draws left to right, then the area and
**both** lines are revealed under a **single** mask wipe. Two wipes would read as
two charts.

The wipe is a masked gradient driven by a registered `--scfw` percentage.
Registration is load-bearing: an unregistered custom property is not
interpolable, so the wipe would snap from nothing to everything on the first
frame.

## No series

`series={null}` is a real state, not an error, and it is what a page shows while
the aggregation behind the chart does not exist. The block keeps its height and
draws its **baseline**, so it reads as a chart waiting for its answer rather than
as a hole in the page — and the legend keys are dropped, because a key naming
lines that are not drawn is a legend for an empty box.

`series={[]}` is not that state and must not be used for it. A flat line along
the baseline is a claim about a fortnight; `null` is the absence of one.

## Geometry

A 320 × 96 viewBox that stretches to whatever column it is given
(`preserveAspectRatio="none"`), baseline at 84, tallest point at 8. Strokes take
`vector-effect: non-scaling-stroke` so the lines keep their weight at any width;
without it a wide column draws hairlines and a narrow one draws cables.

## Accessibility

The drawing is `aria-hidden`. It carries no reading a screen reader could use —
the figures beside it are the reading, and they are announced there. The hover
readout is ordinary text and is announced as it appears.

## Reduced motion

**Skip to the end state.** Both build animations fill `both` from a zeroed
start, and the blanket rule in `tokens/motion.css` removes the animation without
removing the from-state: the baseline would sit at `scaleX(0)` and the mask at
`0%`, which hides the entire chart. So the component reads the preference itself
and states both end states under `[data-still]`.
