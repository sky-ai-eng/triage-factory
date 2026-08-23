# StatusRules

The Jira board: every status the connection reports lives either in one of four
columns or in the tray under the board. You drag a status between columns, or to
the tray to unmap it.

```tsx
import StatusRules from './StatusRules'

;<StatusRules
  value={rules}
  onChange={saveRules}
  statuses={connection.statuses}
  showProjects={false}
  note="drag a status between columns, or to the tray to unmap it"
/>
```

A status is an item — `{ id, label }` — and the id is the identity everywhere:
membership, the drag payload, and ★ all key on it, because two statuses can
share a label (Jira permits it across issue types) and a label can be renamed
upstream without the status changing. The label is only ever paint.

Supplying `onChange` makes the board **controlled**: `value` is the board,
every gesture — a landing, a ★ move, a suggested mapping — reports the next
whole map, and nothing is stored inside. Without `onChange` the board poses its
own state seeded from `value`, which is the demo mode `/dev/ui` mounts.

Requires `status-rules.css` and `Checkbox` (a normal import — it no longer
fetches and evals the file at runtime).

## What the shape is saying

**Four columns, because TF needs four answers.** Ready is where work is picked
up, in progress and in review are where a delegated agent parks a ticket, done is
where it lands. `ready` has no ★ — TF reads work out of it and never writes it —
and the board enforces it: landing in READY never mints a primary, and READY
chips are not buttons, because a chip with no write target to set has no action
to offer. Every other column needs one canonical status to write back, which is
what ★ marks; the board mints one on a first landing and hands it to the next
member when its chip leaves, so a non-empty write-target column always carries
one.

**There is no hidden pool and no + menu.** A status is in a column or in the
tray, so adding and removing are one verb in two directions. The tray takes the
height the columns are not using, up to three rows, then scrolls — a connection
with forty statuses does not push the board off the page.

**Unmapped is a real state, not an error.** A status nobody mapped simply sits in
the tray. The one genuine failure — no projects watched — says so in alarm text
under the picker, because a board with nothing reaching it is silently inert.

## interactive

`interactive={false}` is for a viewer who may see the mapping but not change it —
a team member on the Jira page. Chips lose `draggable`, columns stop accepting
drops, **and the grab cursor goes with them**: a cursor that says "pick me up" over
a chip that cannot be picked up is worse than a disabled control, because it
promises and then fails.

**Nothing is greyed out.** The mapping answers "where does our work come from",
which a member legitimately needs. Dimming it would hide information to signal a
permission.

## Keyboard

The board is a drag surface, and drag is pointer-only. It does not need a
keyboard equivalent invented for it, because **it already has one**: click a tray
chip to stage it, then click a column to land it. That path existed for anyone who
would rather not drag; it simply was not reachable. Now it is.

- **A tray chip** is `role="button"`, `tabIndex={0}`, `aria-pressed` for staged.
  Enter or space stages it and un-stages it.
- **A column becomes a tab stop only while something is staged** — that is the
  only moment it is a control. It takes `role="button"` and an
  `aria-label` of "Put QA in IN REVIEW", so the announcement names both halves of
  the move. Enter or space lands the chip.
- **A column chip** is `role="button"`, `tabIndex={0}`. Enter or space makes it
  the write target, which is what clicking it does. It stops propagation, so the
  press does not also fire the column underneath. READY chips are the
  exception — no write target to set means no action to offer, so they carry no
  role and no tab stop, and a click falls through to the column so a staged
  chip still lands.
- **Escape cancels a staged chip**, which the board already did.
- Space always calls `preventDefault()` — the board scrolls, and a staging
  gesture that also scrolls the page away from the board is no gesture at all.
- **Under `interactive={false}` all of this is absent**, not disabled: no role, no
  tab stop, no cursor. A board a viewer may only read has nothing to reach.

Reordering _within_ a column has no keyboard path because it has no meaning —
members are a set, and ★ is set by pressing the chip.

One gap is real and known: **a chip that has landed cannot be moved to a
different column or back to the tray from the keyboard** — the click path runs
tray → column only, and Enter on a placed chip is taken by ★. Remapping a
landed status is drag-only for now.

## Dragging

Dragging uses the HTML5 drag API with the browser's own drag image suppressed
and a hand-built clone instead: the native ghost paints the chip inside an opaque
white rectangle, which reads as a mistake over a warm ground.

Three details, each of them a bug that was fixed once:

- **Every drop ends the drag, including a drop into dead space.** A chip that
  changed columns has been unmounted and remounted by the time its `dragend`
  would fire, and a removed node never gets one. So the sink swallows dead-space
  drops and clears the drag itself, rather than waiting for an event that is not
  coming.
- **The clone is removed on the same frame as the state change**, or the dropped
  chip appears twice — once where it landed and once, frozen, where the mouse
  let go.
- **The column under the cursor takes a faint warm wash**, per column, not per
  chip. The drop target is a column; highlighting the chip you are hovering says
  the wrong thing about where it will go.

## CSS

Every value is a selector in `status-rules.css`, keyed off data attributes the
component sets from content: `[data-over]` for the column under the pointer,
`[data-staged]`, `[data-canonical]` for ★, `[data-more]` for a column that
scrolls, `[data-clipped]` for a name too long for its column, `[data-empty]` and
`[data-board-empty]` for the two ends of the board/tray height trade.

Two things stay inline, because both are measured at runtime: each column's and
chip's build delay (`--at`) and the peek label's fixed position.

The drag ghost is now a class too. It used to carry literal colors "because it
hangs off `<body>`, outside the scope that defines the tokens" — but a class
resolves against `:root`, which is exactly the scope it should read, so the clone
picks up the reader's own lighting. It keeps the chip's own classes and adds
`.sr-ghost`; the state attributes are stripped off the clone so `.sr-ghost` is not
outranked by a `[data-canonical]` fill.

The build keyframes moved here from `table.css`, where they had been filed as
"shared keyframes for the extracted instruments" — which meant a page mounting
the status board had to load the table's stylesheet to make it animate.

## Build

Columns land left to right, one after another, then the chips fill in. The build
runs on arrival and on an explicit replay, never on a data change — a poll that
redraws the board every fifteen seconds must move chips in place.
