# Table

Rows you can sort, select and act on. It knows about columns, selection, paging
and time; it knows nothing about repositories, channels or people. A people
table is this component with people columns.

## Growing the estate

`add` puts a verb on the title row. Given `options` or `fields` it opens a **draft page** rather than a dialog: a draft row in the table's own grid, plus the candidates it could become. While it is open the pager gains a `+` to the left of page 1 — the page you are on — and it is gone again when you leave, because a pager reports where you can go and there is nowhere to go until the row is open. Rows that exist and a row being written never share a page, and the draft page is the same height as the page it replaced, so nothing moves.

```tsx
import Table from './Table'
import { tableCells, ago } from './cells'

<Table
  columns={memberCols}
  rows={memberRows}
  add={{
    label: 'add teammate',
    placeholder: 'name or email',
    options: org,                                   // [{ id, name, note, disabled }]
    fields: [{ key: 'role', options: ['member', 'admin'] }],
    onAdd: (person, { role }) => assign(person, role),
  }}
/>
```

**Nothing is offered until three characters are typed**, and the field's own placeholder is what says so — the ask belongs where the typing happens, which also means it has to be short: it shares a name column with a role picker at any zoom. A directory dumped the moment the row opens invites picking a name off a list; the reader is meant to know who they are adding.

**The line under the draft row says how the page ends, always.** `pushing 'esc' or navigating to a different page will save changes` is true before anything is staged and after, so nobody has to stage something to find out what leaving does. Override it with `hint` only if a table's own exit is different.

**The columns do the explaining.** The first column takes the search field, a `fields` column becomes a picker at its own value, and every measurement column prints an em dash — nobody who has just been added has run anything, and an empty cell says that where a zero would lie.

**Nothing is applied until you leave the page.** A name leaves the candidates and lands in a strip along the bottom of the page as a pill carrying its rank; clicking the pill takes it back. Escape, the `+`, or any numbered page leaves, and that is what fires `onAdd` once per pill, in the order they were named. So the whole page is one reversible act: four names typed, four pills to check, one exit.

**Rows are for people the table has.** A staged person is not one of them yet, which is why they sit in the strip rather than in the grid — and why the search can stay put as you work: whoever you just took is gone from the candidates, so the same query keeps showing who else it matched. The strip is one line however many people it holds, scrolling sideways, so the page's height does not move.

**Opening the draft page clears the selection.** The rows a verb would have applied to are not on screen once the pager moves to `+`, so a selection bar still floating over the draft row is aimed at nothing.

**The draft row is a band, not a card.** No outline, no radius, no raised fill — a warm wash with a 2px marker in the gutter and a warm rule along its bottom, so the header divider above stays the only divider. One warm sweep crosses it as it opens: that flash is what tells the eye where the page changed, which is the job an outline would otherwise be doing permanently.

**Candidates are rows, not a menu.** The matches draw as full-width rows in the same grid, directly under the draft row — reading them is reading the row each one would become, and the highlighted one is what Enter takes. How many show is the table's own height minus the draft row, its hint and the staging strip, so a busy directory looks busy and a small table still fits. A candidate prints only what the table has columns for, plus the reason it cannot be taken (`already on this team`): a discipline or a home team shown beside the name would be a column the roster does not have. The only overlay in the whole flow is a `fields` picker's own menu.

## The undo model

The mutation applies to what you see immediately, and the request is held
client-side for ten seconds. Undo means the request is never sent — no
compensating write, no window where the server disagrees with the screen. The
alternative, sending now and asking the server to reverse it, needs every
mutation to be invertible server-side, which removals and role changes are not,
cleanly.

Two consequences worth keeping: `mutate` describes the row after the verb (or
returns `null` to drop it), and `onCommit` is the only place a request belongs.
Starting a second action commits the first rather than stacking snapshots.

**Everything the table can apply, `onCommit` must be able to send.** It receives
the picked option alongside the action id, because a caller that can apply a
role change has to be able to send one — without it the screen shows a change
the server never hears about, and a refresh silently reverts it. That is the
failure this model invites if the handler is not exhaustive, and it is silent.

**A window can end three ways, and all three owe the request.** `reason` says
which:

| | |
| --- | --- |
| `window` | it ran out, or a second action cut it short |
| `navigation` | the table unmounted with a window still open |
| `unload` | the page is going away |

The last two used to drop the request — the reader watched the row go and
nothing was sent. They flush now. On `unload` the caller must set
`keepalive: true` on the request, or the browser kills it with the document;
`sendBeacon` is not a substitute, since it is POST-only and cannot express a
PATCH or a DELETE. An undone window sends nothing, however it ends, and several
endings racing send exactly once.

## The treatment

**A page is a fixed frame.** Paging, not a scroller: the row under the cursor
never moves, and the count below the table is always true. A short last page
holds its height open with a spacer, so the pager does not walk up the screen on
page three. That is also why rows are an explicit 35px rather than padding plus
whatever the line box comes to.

**The pager is a range and numbered pages**, right-aligned: `19–20 of 20`, then
`‹ 1 2 3 ›` as 22px chips with the current page filled. Past seven pages it
windows to five around the current one — forty chips is not navigation.

**Verbs float.** With `bar` set, selecting rows summons a SelectionBar over the
table instead of lighting up a toolbar across the page from the checkboxes that
produced it. A verb that cannot apply to the whole selection is dropped from the
bar (`enabledFor`), never shown disabled — a disabled entry in a floating bar
reads as a bug, and the reason is rarely legible from the row.

**Every verb takes the same gesture, at its own price.** The bar is a hold
surface; `holdMs` is what the verb costs. Destructive verbs default to 900ms and
name the gesture in their label; ordinary ones are 0 and fire on press, with the
rings arriving at once. Making the cheap case a shorter hold rather than a
different control means raising a verb's price later is a number, not a redesign.

**The undo lives in the bar.** The table owns the snapshot and the ten-second
window, and hands the bar `{msg, frac, onUndo}` to draw — the selection bar and
the undo bar are the same object in two states, in the same place, rather than
two treatments that happen to appear at the same corner. The ring drains
continuously; a once-a-second step reads as a stutter at that size.

**Columns size themselves; do not pass widths.** One rule, applied by the
component: a left-justified column runs right until it meets the next
left-justified column or the table edge. An end- or center-justified column
takes the width of its own content — the wider of its header label and its
widest cell, measured across every row rather than the visible page, so paging
never shifts the grid — capped at 240px. What is left goes to the left-justified
columns. An explicit `width` still wins, for a bar or glyph column that must not
move, but a hand-tuned pixel value on a text column is a bug waiting to be
re-tuned at the next zoom level.

**Cell types are a registry, not a switch.** A column names one with `type`;
the registry says how it draws, how wide it wants to be and what it sorts by,
and the column's own keys override any of that. Seven ship with it — `text`, `ago`,
`identity` (avatar + name), `spark` (a trail of bars), `toggle` (the cell IS
the switch, applied on click, `locked` for a row that cannot be switched off),
`mark` (a single-select star that moves but never clears) and `bar` (hatched
track, filled to the value, number at the right) — and a caller adds its own
with `tableCells.badge = {…}` rather than growing the component.
Anything a type cannot express is still a plain `render` returning a node: a
string or number gets the text treatment, a node is left alone — no ellipsis,
no hover-to-read, no type face imposed. Such a column gives `measure` a proxy
string if it wants to be sized, and `sortable: false` if its header should be
inert, which a sparkline's is.

**Not every table's row control is a selection.** The models table selects
nothing: its checkbox is the model's enabled state and its star is the default,
both applied on click. That is `selectable={false}` plus a `toggle` and a
`mark` column — the same table, without the bulk-verb machinery it has no use
for. `rowBg` tints the row that is the one.

**A picker option is an action.** Give `bar.picker` or `bar.danger` an
`action` and the pick runs through the same snapshot, undo window and commit as
a labelled verb; `mutate` receives the picked option as its third argument. A
role change and a removal should not have two different undo stories.

**"+ add" sits on the title row, off by default.** `add={{label:'add teammate'}}`
puts it immediately left of the filter, where the reader already is. Most
estates are not grown from the table that lists them, so it is opt-in.

**A clipped name says itself.** A cell too narrow for its value ends in an
ellipsis and names itself in full on hover. Headers clip the same way. With the
sizing rule above, a value is clipped only when the table is genuinely out of
room, never while empty space sits to its right.

**Blanks sort last in both directions.** No primary team is the absence of a
value, not a value below 'a', and it should not fill the top of the column you
just asked to rank. Strings compare with `localeCompare`, so mixed case ranks
the way a reader expects.

**The chevron follows the reading.** Highest at the top points down.

**Sorting is a column click**, with the direction marked on the active column
only. The header cell is the target, not its glyphs.

**Elapsed time is a measurement.** `tableAgo` prints `40m`, `6h`, `12d`, `3w`.
"Yesterday" is both vague and three times the width of the column.

**A dash means not applicable; a zero means zero.** Columns that can be either
must use `color` to keep the two apart, because a reading the surface argues
from cannot be printed in the ink reserved for "no data".

## Usage

```tsx
import Table from './Table'
import { tableCells, ago } from './cells'

<Table
  label="CHANNELS"
  columns={[
    { key: 'name', label: 'CHANNEL', width: 'minmax(0,2fr)' },
    { key: 'owner', label: 'PRIMARY TEAM', align: 'end', render: (r) => r.owner || '—' },
    { key: 'mentions', label: 'MENTIONS · 7D', align: 'end', width: '120px' },
    { key: 'last', label: 'LAST EVENT', align: 'end', width: '96px', render: (r) => tableAgo(r.lastMin) },
  ]}
  rows={channels}
  pageSize={8}
  sortKey="mentions"
  sortDir={-1}
  headerRight={<Filter />}
  bar={{
    actions: [
      { id: 'watch', label: 'Watch', message: (n) => n + ' channels watched' },
      {
        id: 'primary',
        label: 'Claim as primary',
        enabledFor: (rows) => rows.every((r) => !r.owner),
        message: (n) => 'this team is primary in ' + n + ' channels',
      },
      { id: 'drop', label: 'Stop watching', tone: 'bad', message: (n) => n + ' channels unwatched' },
    ],
  }}
  barPosition="absolute"
  mutate={(row, id) => (id === 'drop' ? { ...row, watched: false } : { ...row, watched: true })}
  onCommit={(id, ids) => api.channels(id, ids)}
/>
```

A people table is the same component with people columns:

```tsx
import Table from './Table'
import { tableCells, ago } from './cells'

<Table
  label="MEMBERS · 14"
  columns={[
    { key: 'name', label: 'NAME', type: 'identity' },
    { key: 'email', label: 'EMAIL', color: () => 'var(--color-ink-3)' },
    { key: 'tasks', label: 'TASKS · 14D', align: 'end', sortValue: (r) => -r.tasks },
    { key: 'spark', label: 'ACTIVITY', type: 'spark', width: '118px' },
  ]}
  rows={members}
  add={{ label: 'add teammate', onSelect: invite }}
  headerRight={<Filter />}
  bar={{
    picker: { label: 'Role', options: roles, action: { id: 'role', message: (n, o) => n + ' members are now ' + o.name } },
    danger: { label: 'Hold to remove', action: { id: 'remove', message: (n) => n + ' members removed' } },
  }}
  mutate={(row, id, pick) => (id === 'remove' ? null : { ...row, role: pick.id })}
/>
```

Requires `table.css`, and imports `Checkbox` and `SelectionBar` normally. It
used to fetch both over HTTP and `eval` the response so that a caller could
mount the table without knowing they were separate files; the loader resolves
that now, and the fallback square the fetch needed is gone with it. `barPosition="absolute"` needs a positioned
ancestor.

## Pages that fit the frame

`pageSize="auto"` shows as many rows as the table's slot has room for. The
frame it measures against is the TIGHTEST clipping or scrolling ancestor, not
the nearest: a box can clip and still grow with its content
(`overflow:hidden` with an auto height), and measuring one feeds the table's own
height back in — it takes a row too many, the box grows, and the page overflows.
The scroller further up does not move, so the minimum is the honest floor. The
document is a last resort rather than a peer, since an unstretched `body`
reports the viewport height while the page inside it is taller.

From that floor it subtracts everything still owed room below the table: each
ancestor's bottom padding and border, and any sibling stacked underneath (a
danger card, a footnote). Siblings *beside* the table are skipped — a column
takes no height from its neighbor.

## Narrow tables


Columns are measured in px before the grid is built — an explicit `width`, or
the wider of the header label and the widest cell across every row. A flexible
column costs its `floor` (default 150px), the width below which its content
stops being readable. If the total exceeds the room the table actually has, it
drops columns one at a time, least important first, re-measuring after each.

`drop: 1` goes first, `drop: 2` next, and a column with no `drop` never goes —
so a table that must keep every column behaves as it always did and clips.

```jsx
{ key: 'name',   label: 'PROJECT', floor: 120 }
{ key: 'issues', label: 'ISSUES',  align: 'end', drop: 1 }
```

Width is read from the table element, not the window: the same table is wide in
a page and narrow in a column, and only the element knows which it is. A
`ResizeObserver` means it re-fits on zoom, on the rail opening, on a panel
resizing — no breakpoints to declare and none to keep in sync.

Formatting *inside* a cell is the cell's own business, not a table parameter.
Every cell is a query container, so a renderer that wants to say the same thing
more briefly when it is narrow writes `@container (max-width: 90px)` in its own
CSS. The table stays out of it.

## Do not

- Scroll a long estate inside the table. Page it; a bounded scroll container
  hides the count and moves the row you were pointing at.
- Show a verb the selection cannot use. Drop it.
- Ask for confirmation on a reversible bulk edit. Apply it and offer undo;
  reserve the hold gesture for the one verb that removes something.
- Reach for the table when the answer is a single number or a ranked shortlist.
  Both read better as a stat stack or a list.
- Print a formatted value the sort cannot read. Give the column `sortValue`.
- Reimplement an avatar, a bar or a badge cell inside a page. Register the cell
  type once; the next table gets it for free and they cannot drift apart.

## Build

`build` runs the scan entrance once on mount: a beam down the rows, each row resolving out of hatch in its wake. Use it where a table arrives as an event — a panel opening, a page routed to — not on every render of a table that was always there.

```tsx
import Table from './Table'
import { tableCells, ago } from './cells'

<Table build columns={cols} rows={rows} />
```

## CSS

Every value lives in `table.css`, keyed off data attributes: `[data-on]` for a
selected row, `[data-align]`, `[data-sans]`, `[data-live]` / `[data-draft]` /
`[data-on]` on a pager button, `[data-lead]` and `[data-disabled]` on a candidate,
`[data-locked]` on a toggle that cannot be switched off.

Four things are handed to CSS as custom properties, because all four are
measured: `--tb-grid` (the column tracks, out of the sizing pass), `--tb-d` (each
row's build delay), `--tb-run` (how far the beam travels) and `--tb-row-bg` (the
caller's tint for the one row that is the one).

**The hover fix.** Rows used to set `e.currentTarget.style.background` on
mouseenter and put it back on mouseleave — which is why a row could keep its wash
after the pointer had moved on, three separate times. It is
`.tb-row[data-selectable]:hover:not([data-on])` now, and that bug cannot be
written: selection outranks hover in the cascade, and the caller's own row tint
sits under both. The same applies to the header verbs, which used to swap their
own `style.color` on enter and leave.

## Keyboard and semantics

The table is a grid of divs, so the roles are explicit: `role="row"` on the
header and on every row, `role="columnheader"` with `aria-sort` on each heading,
`role="cell"` on each cell, `aria-selected` on a selectable row.

- **Sorting is reachable.** A sortable heading is a tab stop; enter or space
  sorts. A column that declines to be sorted takes no tab stop at all, which is
  the same rule its cursor already followed.
- **Selection is the checkbox's job.** Every row's checkbox is a real, reachable
  `Checkbox` (see its own `.md`), and clicking the row is the convenience on top.
  The header checkbox goes to `aria-checked="mixed"` on a partial selection.
  Shift-click still extends from the anchor, from the row or the box.
- **A row that opens.** A table whose rows lead somewhere — a token's sheet —
  passes `onRowOpen(row)`, and the row click is then the open rather than the
  convenience toggle; the checkbox alone selects, so the bulk verbs keep their
  selection without two gestures fighting over one click. Such rows take the
  pointer cursor and the hover tint whether or not they are selectable. Absent,
  nothing changes.
- **The pager is buttons.** `aria-current="page"` on the page you are on, real
  `disabled` on prev/next at the ends, and names on the glyphs — "Previous page",
  not "‹".
- **The draft row is a combobox.** The input carries `role="combobox"`,
  `aria-expanded`, `aria-controls` and `aria-activedescendant`; the candidate list
  is a `listbox` of `option`s with `aria-selected` following whichever of the
  pointer and the arrow keys owns the mark. Arrow keys, enter and escape already
  drove it — they now say so.
- **Staged pills are buttons** named "Remove <name>", not clickable spans.
- **The clipped-cell tooltip is `aria-hidden`.** It is a visual echo of text that
  is already in the cell; announcing it twice is worse than not announcing it.

**Deliberately not implemented: arrow-key cell navigation.** A real
`role="grid"` promises a two-dimensional focus model — arrow keys moving a single
tab stop cell to cell, home/end, page up/down — and half of that is worse than
none, because a screen reader will have announced the promise. Every verb the
table offers is reachable through the tab order as it stands. If cell navigation
is wanted, it is a deliberate piece of work, not an attribute.

## Reduced motion

The build is CSS — `tb-fill`, `tb-hatch`, `tb-beam` — so the blanket rule in
`tokens/motion.css` covers it: under the preference the rows are simply there.
The undo window's dial is `Countdown`, which handles the preference itself.


## Changed in the port

**The cell registry, `ago` and `tablePages` moved to `cells.tsx`.** They were
exported from `Table.tsx` alongside the component, which meant one module
exported both a component and a mutable registry — the thing
`react-refresh/only-export-components` objects to, and it is right to: a caller
registers a cell type once and every table gets it, which has nothing to do with
one component's render. `columnDef` stayed with the component, since resolving a
column against the registry is the table's job rather than the registry's.

```tsx
import Table from './Table'
import { tableCells, ago } from './cells'
```

**Shapes that were `any` now have names.** `TablePending` (the held mutation),
`TableCandidate` (someone the draft page could add), `PagerButton`, `SparkBar`,
and the picked option, which is `SelectionBar`'s own `PickerOption` rather than a
second definition of it. Two `any`s survive on purpose and say so where they are:
a row's cell values, because `unknown` would push a cast into every renderer a
caller writes, and the column's index signature, which is the registry's escape
hatch.

**Two ref writes and two `setState`s moved out of render and effects.** The
working rows and the build flag were synchronised in effects, which paints the
previous value once before correcting it — on a table that just received new
data, a visible flash of the old estate. Both are render-time adjustments now.
`stagedRef` and `addRef` were written during render, which React forbids because
a thrown-away render does not undo them.
