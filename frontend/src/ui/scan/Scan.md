# Scan

Emission applied to type: a crest travelling across a line of text, meaning an
agent is working.

```tsx
import { Scan } from '../../ui/scan/Scan'

<Scan className="rr-act" active={run.working}>{run.activity}</Scan>
```

## It is Emission in a different medium

`readout/Emission` makes the same statement with a dot, a bar or a spine in
cool. This is that statement made by the words themselves, for a row with no
space for a mark where the sentence is already the thing being reported.

It inherits Emission's doctrine, including the constraint: **cool is only ever
linear or point, never an area.** A crest crossing a line of text is linear — a
moving band, not a filled region — which is what makes this allowed to exist.

**One meaning: an agent is emitting.** Nothing else in the product may use it,
and it is not a loading state. A skeleton says "no answer yet"; this says "the
answer is being written".

## One rule, both grounds

**The crest is always the lighter value, and the base is whatever reads at
rest.** A light passing over text, either way. What changes per ground is only
where the base can sit — the measured luminance table and the two failed
alternatives (a hue crest, a downward-widened ramp) are in `scan.css`.

Dark works with the base mid-ramp (`ink-3` → `ink-1`, a 3.1× step the right
way). On light the same step runs the wrong way and nearly vanishes, so the
base becomes `ink-1` and the crest `ink-4` mixed 45% toward the ground — the
resting state is the most readable it can be instead of the least. A lighter
crest does move toward the ground, but only under the travelling band while
every other glyph stays fully dark: it reads as a glint, not as text dropping
out.

## Theme scoping is load-bearing

Per-ground values are custom properties on `:root` / `:root.dark`, **scoped
exactly as tightly as the tokens they read**. The palette is root-scoped, so a
bare `.dark` selector here would match a nested themed div and swap the scan's
pair while `--color-ink-1` still resolved to the light palette — half-theming
the component against an unthemed palette.

## `cool` costs contrast

The same rule with hue in the crest, mixed toward the ground on light so it is
a pale glint rather than another dark ink. A hue step is not a luminance step,
so it is the choice for a surface that wants the accent — not the default.

## Timing

**Rate and frequency are separate**, and that is the whole timing design. The
crest crosses in 2.2s; the rest between crossings is a hold at the head of the
keyframe — 46% of a 4.8s cycle is the crossing, the first 54% is rest. A pulse
every 4.8s reads as a heartbeat; one every 2.2s reads as a loading bar.

`background-repeat: no-repeat` is load-bearing: the default tiles the gradient,
so a second crest arrives mid-pass and the wrap reads as a skip.

## Reduced motion relights the text

The glyphs are painted by clipping a gradient to them, so the text's own colour
is `transparent`. The motion blanket removing the animation alone would leave an
**invisible line** — which is exactly the bug this once shipped with. The media
branch in `scan.css` restores a colour with the stillness; any future branch
that stops the sweep has to do the same.

## The selector is `[data-tone]`, not the class alone

Scan merges a caller's className, and a sibling class setting `color` at equal
specificity but later in source order silently hid the whole effect — the
animation ran, nothing moved. The attribute selector out-specifies the class it
is designed to sit beside.

## Where it is used

`ui/runrow`, on a `working` run's activity line — the only row state where an
agent is mid-sentence.
