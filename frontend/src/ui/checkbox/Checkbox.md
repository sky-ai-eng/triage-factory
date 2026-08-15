# Checkbox

The square a row is selected with. Unchecked is structure — a 1.2px hairline and
no fill. Checked is a value — a solid warm block carrying a light mark, the same
figure/ground flip a tracked cell uses in the repository field.

## Centring the mark

The mark is centred on its **ink**, not on its bounding box. A check is
asymmetric: the long arm is thin and sits high and right, the vertex is a dense
corner low and left. For the path this component draws, the stroke centroid
(each segment weighted by its length) lands 0.32 viewBox units BELOW the
square's centre while the bounding box lands 0.11 ABOVE it. Centring on either
one alone reads as off, which is the bug this component was extracted to fix —
the table used to place the mark with fixed pixel offsets that only held at one
size.

`TICK_DY` splits the difference. `checkboxCentering()` re-measures a rendered
box so a future edit to the path can be checked rather than eyeballed, and
`checkbox.card.html` draws a 118px box against a crosshair for the same reason.

## Use

```tsx
import Checkbox from './Checkbox'

<Checkbox checked={on} onChange={setOn} />
<Checkbox indeterminate size={13} />
<Checkbox checked disabled label="Owned by another team" />
```

Sizes are free — the radius and stroke scale with `size`. 13px is the table
row default; 11px is the smallest that still reads.

## Keyboard and semantics

The control is a `role="checkbox"` with `aria-checked` (`"mixed"` when
indeterminate), and it is **reachable**: `tabIndex={0}`, so the announcement and
the operation agree. An earlier version declared the role without either a tab
stop or a key handler, which announced a checkbox that could not be reached or
toggled — worse than not announcing it at all.

- **Space** toggles. `preventDefault()` on the press, or the page scrolls.
- **Enter** deliberately does nothing. A row checkbox sits inside a row that has
  its own Enter meaning; swallowing it would take the row's primary action away.
- **Disabled stays focusable** — `aria-disabled`, not `tabIndex={-1}`. A row
  checkbox is usually disabled because something else owns that row, and a
  keyboard reader should be able to find out which rows those are. Clicks and
  keys are both ignored.
- The focus ring is drawn on the **square**, not on the whole control. On a
  labelled checkbox the tab stop's box includes the text, and a ring around
  text that is not itself the control reads as a selection.
- Accessible name: the `label` when there is one, otherwise `title` via
  `aria-label`. The square itself is `aria-hidden` scenery.

## Notes

- Size is the only thing passed in as inline style, as `--check-size`,
  `--check-radius` and `--check-stroke` custom properties — it is a genuine
  per-instance runtime value. Everything else, **hover included**, is a selector
  in `checkbox.css`. The component holds no hover state, which is the point: a
  `:hover` rule cannot be left stuck on after the pointer leaves.
- The mark is painted in `--color-ground`, not white: it is a hole punched in
  the warm block, so it stays correct on either theme.
- Requires `checkbox.css`.
- `Table` and `StatusRules` render this when it is present. Both are mounted
  with it already loaded — the loader resolves the dependency, not the
  component; neither one fetches this file at runtime any more.
