# RunRows

The agent-run list. One row per run, in NEEDS YOU, in RUNNING, and anywhere
else a set of runs is shown.

```tsx
import { RunRows } from '../../ui/runrow/RunRows'

<RunRows
  label="NEEDS YOU"
  count={<span style={{ color: 'var(--color-warm)' }}>7</span>}
  rows={items}
  lead="ref"
  onPick={(r) => navigate(r.href!)}
  more={<Link to={boardHref}>+4 more on the board</Link>}
  empty="Nothing needs you."
/>
```

## One prose string per row

The row leads with what is happening. Everything else is demoted so it cannot
be mistaken for a second sentence: the reference in small mono, the age closing
the row in mono, the source as a 14px glyph.

This is the whole design. Two sentence-shaped strings of equal rank on one
line — a subject and an activity — give a row two centers of gravity, and the
gutter between them goes ragged, because a short subject leaves a trench and a
long one leaves none. That shape was built and cut.

The age arrives pre-formatted; the row does no time math. A caller who wants
it live passes a self-updating node — `ui/shared/LiveText` with the caller's
own format — which keeps the once-a-second work confined to the age's text
node instead of teaching this component about clocks.

## Which identity leads

`lead` decides whether the row opens on the prose or on the reference. It is
set per list, never per row.

`lead="activity"` is the original: prose first, reference trailing it in small
mono. It reads best when the list is a feed scanned for *what is happening*.

`lead="ref"` gives the reference a column of its own ahead of the prose.
Because every row is a subgrid of one grid, that column is sized by the widest
reference in the whole list, so every sentence in the list starts on the same
line.

The reason it exists is motion. A working run's activity is the agent's current
action, and it is replaced as the agent moves between commands — "Reading the
repository" becomes "Replaying 6 commits onto origin/main" becomes "Running the
test suite". With the prose leading, each of those changes the width of the
element the reference is packed against, so the reference slides left and right
while the row sits still otherwise. Nothing is wrong on any single frame;
across frames the row appears to twitch, and a list of four working runs
twitches out of step with itself. Anchoring the reference removes the movement
entirely: the prose grows and shrinks to the right of a fixed column, into the
fade it was always going to end in.

The cost is real and is why this is a switch rather than a change. The prose no
longer holds the first position, and the reference — mono, ink-3, one rung
brighter than when it trails — sits where the eye lands first. A list read for
the work still wants `lead="activity"`.

`anchor={false}` keeps that order and drops the column. The reference goes back
into the row's own flex line, first instead of trailing, and each sentence
starts wherever that row's reference ended. The twitch stays fixed — the
reference leads, so nothing is pushing it — but the list loses its shared left
edge of prose, which is also the edge the section label, the note and the
"more" link align to.

The lead is ignored when no row in the list carries a reference: an empty
column is 11px of gap in front of every sentence and nothing else.

### Both columns are capped

The reference column is `fit-content(152px)`, stepping down to 116px under a
470px list. Without a cap the column goes to whichever row has the longest
reference, and one `platform-control-plane-migrations#1184` in a list of twelve
short refs charges the other eleven for it. The cap is stated on the cell as
well as the track, because `fit-content` floors at min-content and a reference
is one unbroken word — its min-content is its full width, which lifts the track
straight back past the cap.

Past the cap the reference loses its **head**, never its tail: the number is
what distinguishes one row from the next, so it is split off at the `#` and
pinned while the repo name ellipsizes into it — `platform-control-pl…#1184`. A
reference with no `#` has no dispensable half and truncates at its end like
ordinary text. Over the cap the whole reference is available on hover.

The prose has no upper bound. It had one — an 86ch measure — and that put the
fade in the middle of the row: on a 950px list the sentence dissolved at 600px
with 70px of empty ground between it and the time it was supposedly running out
of room for. A fade has to land on something, and the row's own time is the
only thing available, so the sentence runs to it. If a list is ever wide enough
for line length to be a real reading problem, the answer is a narrower list.

A reference that gave up width does not hand it to the prose; the two are
independent, which is the point of the anchor.

### A run with no entity keeps the slot

A hand-started run has no upstream entity, and in lead position the column is
still there — an empty cell reads as a rendering fault rather than as an
absence. So the absence is drawn: an em dash, the mark a table uses for a value
that does not exist, dimmer than a real reference so it cannot be mistaken for
one. It is `aria-hidden`; a missing reference is not worth announcing.

Not the word "manual". The glyph beside it already names the source, and this
column answers *which one*, not *what kind*. If runs ever carry a short
displayable id, that is a better fill than the dash and should replace it.

Only in lead position. Where the reference trails the prose there is no column
to hold, and a dash after the sentence would read as content.

### The tail belongs to its row

Every other column in a row is a track of the list's grid, shared so the starts
align. The tail is not: it shares the prose's cell and each row divides that
cell alone. As a track it was as wide as the widest tail in the list, so one
queued row's `20s · ⧖ 3` set the width and every plain row's sentence stopped
short of its own `12m`, dissolving against a gap that belonged to a different
row.

Nothing is lost by it. The queue mark follows the age, so the right-most
element is the mark on a queued row and the age on a plain one — there was
never a shared left edge to keep, and both are still flush to the gutter on the
right.

### Under 400px the column is dropped

Not narrowed. A 92px reference is a stub with an ellipsis in it, holding the
width the sentence needs to say anything at all. So the reference leaves the
line and hangs off the source glyph as a tooltip: the glyph is already the
row's source mark, which makes it the one place the reference can go without
spending any of the line. The hover exists only where the column is gone, so it
is never an echo of words already on the row, and the reference reaches
assistive technology through the glyph's own `aria-label` rather than a second
tab stop inside a link.

The measurement is on the list, not the window — the same list is a 430px
column on Overview and a full-width pane on a card — which is why it is a
`ResizeObserver` and not a container query: the reference moving onto the glyph
is a change of markup, not of style.

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

The list is one grid; every row is a subgrid of it. That is what keeps the tick,
glyph, reference, and prose start aligned down the list without a row knowing
its own width — per-row flexbox would let the starts wander a few px per row.
The tail is deliberately outside that: prose and tail share one cell and each
row divides it alone.

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
component, and both with `lead="ref"` from a single page-level setting: two
lists of the same primitive on one page reading in two different orders is the
reader doing the component's job twice. The Board has its own card, which
shares the activity treatment but not this layout.
