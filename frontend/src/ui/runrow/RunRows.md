# RunRows

The agent-run list. One row per run, in NEEDS YOU, in RUNNING, and anywhere
else a set of runs is shown.

```tsx
import { RunRows } from '../../ui/runrow/RunRows'

<RunRows
  label="NEEDS YOU"
  count={<span style={{ color: 'var(--color-warm)' }}>7</span>}
  rows={items}
  onPick={(r) => navigate(r.href!)}
  more={<Link to={boardHref}>+4 more on the board</Link>}
  empty="Nothing needs you."
/>
```

## One prose string per row

The row leads with what is happening. Everything else is demoted so it cannot
be mistaken for a second sentence: the reference trails the prose in small
mono, the age closes the row in mono, the source is a 14px glyph.

This is the whole design. Two sentence-shaped strings of equal rank on one
line — a subject and an activity — give a row two centers of gravity, and the
gutter between them goes ragged, because a short subject leaves a trench and a
long one leaves none. That shape was built and cut.

## There is no title

An agent run has no name of its own. What identifies it is the work, and the
work is already the prose: "Replaying 6 commits onto origin/main" tells you
what this run is better than any label would. We also do not have one to show —
runs carry an entity and an event type, not a title. Showing both the source's
subject line and the activity is showing the same thing at two resolutions, so
the subject lost.

If taskless runs become common enough that a list of them reads as anonymous,
that is the case for a generated summary on that one kind of run — not a title
primitive for everything.

## Two axes

Straight from the design language, and conflating them is what makes a list
lie.

- **`lifecycle`** — `queued` · `working` · `done` · `failed`. Where the run is.
- **`asks`** — whether it wants a person. Independent of lifecycle: a merged
  pull request whose release note is still unwritten is `done` and asking.

`asks` takes the warm tick, whatever the lifecycle, and warm is only ever
this. A `working` row **scans** its activity — `ui/scan`, which is
`readout/Emission` applied to type. That motion means emission: an agent is
acting, and nothing else in the product may use it. Do not collapse the axes
into one enum — the list's correctness depends on their independence.

## The icon names the source

`pull` · `ticket` · `manual` · `alert`. Not the event: a review request and a
merge conflict both draw a pull request, and the distinction lives in the words
on the right. Six event types do not want six glyphs. Failure is the exception
and takes its own mark, because a failure is a different kind of thing from a
request and should not have to be read to be seen. The paths come from the
shipped rail's glyph table, so a row draws from the same hand as the frame
around it.

## Every row goes to its run

`href` is a real href, so a row opens in a new tab like any link; a plain
primary click is intercepted for `onPick` (in-app navigation), while modified
clicks and non-primary buttons keep the anchor's own behavior. There is no
read-only mode — a run's view is always reachable, including for a run that can
no longer be resumed — so `nav: false` is for a row with nowhere to go, never
for a viewer without permission.

What a row does *not* do is act. No claim, no requeue, no snooze — the Board
owns the verbs, and a second place to act on a run is a second place for the
two to disagree about its state. One target per row is also what makes a 35px
row safe: the whole band is the hit area.

## The queue mark

A queued run takes `queue: n` — the places **ahead** of it (0 = next in line).
It draws an hourglass and a warm count after the age, freeing the prose to name
the work rather than saying "waiting for a slot" on every row of a queue.

The mark carries `ui/tooltip` with `focusable={false}`, because the row is
itself an `<a>` and a tab stop inside an anchor is invalid and redundant. The
words reach assistive technology through `role="img"` and an `aria-label` on
the mark — one string feeds both, so the visible and announced words cannot
drift. This replaced a native `title`, which waited about a second, could not
be styled, and competed with the browser's own link hint on a row that is a
link.

## Layout

The list is one grid; every row is a subgrid of it. That is what keeps the age
column and the prose start aligned down the list without a row knowing its own
width — per-row flexbox would let the right edge wander a few px per row.

The prose dissolves when it runs out of room; the reference stays whole. The
row's identity is what must survive a narrow window, so the sentence is the
sacrificial element. The fade rides on the prose's own trailing padding, which
is what stops it dimming a short sentence.

## Props of note

The **count arrives already colored**, because the accent belongs to the
section, not to the component: NEEDS YOU is warm, RUNNING is cool, and a third
section could carry a third tone without this file changing.

**`more` is a slot, and the caller passes its own anchor** (a router `Link`
with a real href). The design bundle's `onMore` callback became this instead:
a ui component takes data and callbacks, but a link that navigates wants a
real href for exactly the reasons the rows themselves have one. The slot is
aligned to the prose column so it reads as belonging to the list.

## Where it is used

The Overview's NEEDS YOU and RUNNING sections — both of them, through this
component. The Board has its own card, which shares the activity treatment but
not this layout.
