# SelectionBar

What a multi-select table offers once rows are selected: a count, a choice, a
destructive verb, and a way out. It floats over the table rather than living in
the chrome.

## Why it floats

Permanent chrome costs layout whether or not anything is selected, and it puts
the verbs across the page from the checkboxes that produced them. A bar that
appears on selection costs nothing at rest, arrives near where you were
clicking, and never competes with the pager for the table's footer.

## The treatment

**The bar is the control.** No verb gets its own button inside the bar — the bar
itself is Hold's surface, and the measurement crosses all of it.

**One gesture, parameterized.** Every verb runs through that hold; `holdMs` is
the pressure. A removal is 900ms and says so in its label — "Hold to remove",
not "Remove", because a control that ignores a click reads as broken. An
ordinary verb is 0: the rings arrive at once, settle, and it fires on press.
Zero is not the absence of the gesture but its shortest form, so the
acknowledgement is the same shape whatever the verb costs, and raising a verb's
price later is a number rather than a rewrite. The rings take the tone of the
verb being pressed, so an ordinary one does not borrow alarm's weight.

**The picker draws itself.** A choice is a stem growing out of a terminus X,
ticks extending, labels arriving along them, separated by a soft-edged blur
field. No fill, no rectangle, no shadow — a dropdown panel is the one shape this
system does not have. The noun stays visible while it is open: a glyph can
retire without consequence, a word leaves a hole where the thing you clicked
was.

**Reversible edits apply, then offer undo.** Picking a value lands it
immediately and the bar becomes a countdown you can cancel. The ring drains
continuously rather than stepping once a second — a jump on a fourteen-pixel
dial reads as a stutter, and the window is a duration, not a count. A host that
owns the snapshot (Table) passes `undo` and drives it; there is one undo
treatment, not one per table. A pending state that
has not applied yet makes the table lie for ten seconds, and confirming a
reversible edit is what trains people to dismiss the dialogs that matter — which
is what makes the hold on the destructive verb work.

**A blocked option is listed, not hidden.** It states why it cannot be chosen,
carries no hover, and its tick is fainter. Hiding it leaves the reader
wondering whether the product forgot.

**Nothing in it is selectable text.** A drag that starts on a label would
otherwise highlight the word instead of pressing it, which is fatal for a
press-and-hold.

## Two measured values

Both come from the DOM rather than constants, and both are load-bearing:

- **The spine** runs from the terminus X's centre to the last tick, so both ends
  are joins and the option count is free. Only its LENGTH is measured. Its x is
  structural: the spine and the option list are children of the terminus and
  centre through the same `left: 50%` plus half-width translate its cross bars
  use, because a measured `left` cannot agree with that rule to the sub-pixel
  and a crisp 1px line lands visibly off centre when it disagrees. For the same
  reason the arrival animation belongs to an inner wrapper around the cross
  bars, not to the terminus itself — a transform on that box would scale the
  spine and the whole option list with it. Length measurements are divided by
  the live canvas scale — `getBoundingClientRect` reports screen pixels while a style
  length is CSS pixels, so a zoomed canvas otherwise draws the spine at the
  wrong height. The gauge is taken off the bar's *width*: a dozen-pixel gauge
  rounds into whole pixels of error once it multiplies an eighty-pixel spine.
- **The blur field** is a union of one blob per row, so it follows the ragged
  text edge instead of drawing a rectangle by another name.

The spine's growth is stepped in a frame loop rather than declared. Both
declarative routes — a CSS keyframe and a WAAPI animation — strand at their
from-state on a remounted node, reporting `running` with a start time that never
resolves, which leaves the spine invisible on every open after the first.

## Usage

```tsx
import SelectionBar from './SelectionBar'

<SelectionBar
  count={2}
  picker={{
    label: 'Role',
    value: role,
    options: [
      { id: 'admin', name: 'Team admin', note: 'can add, remove and configure' },
      { id: 'member', name: 'Member', note: 'can see everything, change nothing' },
      { id: 'org', name: 'Organization admin', note: 'set in Organization', blocked: true },
    ],
    onPick: (o) => setRole(o.id),
    onRevert: () => setRole(previous),
  }}
  actions={[
    { label: 'Watch', onSelect: watch },
    { label: 'Stop watching', tone: 'alarm', holdMs: 900, onSelect: unwatch },
  ]}
  onDismiss={clearSelection}
/>
```

Requires `selection-bar.css`, plus `hold.css` and `countdown`'s stylesheet-free
dial. `Hold` and `Countdown` are ordinary imports — the bar used to fetch and
`eval` both at runtime so a caller did not have to load them, and the "without
Hold the verbs fall back to clicks" path existed only because that fetch could
fail. Both are gone. Mount it inside a `position: relative` container — `.selbar-mount` is
provided for the usual case of pinning it to the bottom of a table.

## Do not

- Give the destructive verb its own button. The bar is already the affordance;
  a button inside it is one affordance too many.
- Fire a verb with no acknowledgement at all. `holdMs: 0` still flashes; a verb
  that applies with nothing on screen reads as a mis-click either way.
- Put more than six options in the picker. Past that it needs a filter, and a
  filter row belongs to the same structure — see the reference picker — not to a
  scroll container, whose bounded height is the rectangle this avoids.
- Confirm a reversible edit with a dialog. Apply it and offer undo.
- Use it for single-row actions. Selecting a checkbox to reach "change one
  person's role" is a detour; that wants a per-row menu.

## The undo dial

The countdown in the undo bar is `Countdown` — three crates draining, loaded by the bar itself the same way it loads `Hold`. The bar keeps the clock: the window ends by committing, so the dial and the commit come off one timer. Pass `undo={{ msg, frac, onUndo }}` and the caller's clock drives it instead, which is what the table does.

## Naming, and who owns it

The bar mounts `Hold` in **surface mode**, where the wrapper claims no
accessible name and each trigger carries its own — so the bar is responsible for
naming its verbs, and it does: every verb span is a `role="button"` tab stop
labelled with the verb plus " — press and hold to confirm" whenever its
`holdMs` is above zero, and with just the verb when it is not.

This matters because the alternative was silent and wrong. Hold's own `label`
prop defaults to "Hold to save", and while the wrapper still carried an
`aria-label`, a screen-reader user pressing and holding the control that REMOVES
a teammate was told they were saving. See `Hold.md`.

Two more controls that were clickable spans and are now real buttons: **Undo**,
which was the only control offered during the undo window and could not be
reached by keyboard at all, and the **picker**, which is now a
`role="button"` with `aria-haspopup="listbox"` and `aria-expanded`, its options a
`listbox` of `option`s with `aria-selected`. Escape already closed it.

## Reduced motion

The picker's spine grows out of the terminus in a hand-stepped frame loop — the
comment above `grow` explains why it is not a CSS keyframe or a WAAPI animation —
so the blanket rule in `tokens/motion.css` cannot reach it. Under
`prefers-reduced-motion: reduce` the spine is **already at full height**: the
structure is the information and the growth is the flourish.

The undo dial is `Countdown`, which coarsens itself; the rings are `Hold`, which
needs no branch. Both are documented in their own files.
