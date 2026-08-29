# Shell

The frame every page sits in. It owns the rail, the scope mark, the two live
counts, drill-in, the command palette, and the 55px page header. It owns nothing
else — not the page's content, not its background, and not its loading state.

## One route table

The rail is generated, never authored. `routes()` returns sections filtered by
deployment mode and by grant, and the palette searches the same array. This is
the whole reason the palette exists in this file rather than in its own: two
copies of a permission rule eventually disagree, and the failure mode is a
palette that offers a page the rail is hiding.

Availability is a function of **mode** and **grant**, never of role name. Local
mode has no teams, no organisation, no marketplace, so those rows do not exist —
they are absent rather than disabled, because a greyed row is an advertisement
for a feature the reader cannot buy.

## Collapsed by default

52px closed, 208px open, `⌘\` to pin, and the preference persists. Closed, a
section heading becomes a hairline rule — and gives up its height, or the closed
rail ends up taller than the open one. Four short runs of glyphs read cold in a
way one column of twelve does not.

The maximal rail is eleven routes in four groups, which needs about 700px of
window: roughly 520px of rows and rules, plus 185px of mark, counts and foot.
Below that the column scrolls, and because its scrollbar is suppressed a fade at
the foot is what says so. Do not add an overflow menu — a route that is one
scroll away is still in the rail; a route behind a "more" button is not. Every glyph carries its label as a tooltip,
because an icon-only rail without labels is a memory test.

The rail is a strip of **destinations**, not of contexts. That distinction is
carried by the scope mark above the rule — it is the only thing that switches
which world you are in, which leaves the icons free to mean pages. Remove it and
the rail reads as a workspace switcher.

## Drill-in, not expansion

A parent with children slides its column aside rather than pushing rows down. The
rail's length is a fact the reader learns once; a rail that changes height when
you click it has to be relearned every time. `Esc` comes back one level.

## The counts are the only moving thing

`needs` and `running` sit above the rule. When `needs` changes it is marked
once — a single 0.5s drop — and then it stops. No pulse, no ambient loop. If a
count could tick every few seconds, the mark would become wallpaper and the
reader would stop seeing it.

## What it does not own

Page content and the loading state that precedes it. A skeleton describes the
shape of one page's own instruments, so **each page defines its own**. The shell
draws immediately and never guesses at what is about to fill it.

## Do not

- Add a second navigation. A rail plus a floating menu plus a launcher is three
  navigations for a dozen routes.
- Put page actions in the rail. The header's right side is where a page's own
  verbs live.
- Animate the rail on route change. Navigation is not an event that needs
  marking; it is a thing the reader just did.

## Readouts on rail rows

A row may carry a tail: a count, a figure, or an alert. The rule is that a tail
states a **fact about the destination**, never a notification about the app —
`23` open pull requests, `$41` spent today, `6` machines. They are quiet, mono,
and the same weight as the row's label.

Governance is the exception, and deliberately so. Its tail is a warm alert glyph
and a count, because an org credential changing is not a tally that drifts, it is
a thing that happened and needs somebody. Collapsed, the count disappears but a
warm dot stays on the glyph: the detail can be hidden by width, the fact cannot.

Only two things in the whole rail are warm — the queue and a governance alert.
If a third appears, one of them is wrong.

## When the connection is gone

`offline` is not a loading state. Every readout in the rail goes inert — `--`,
not the last number it happened to hold — because a stale count in a triage rail
is worse than no count: 7 could equally be 0 or 40, and only the rail knows which
one it is claiming. Chevrons stay, because structure is not data.

The condition is stated once, in words, at the foot: **Connection lost ·
retrying**. Six readouts each announcing their own failure is six copies of one
fact. The glyph breathes, which is the one repeating animation in the rail and
the only one that is earned — a retry loop genuinely is still running.

## Keyboard

`⌘K` opens the palette, `⌘\` pins the rail, `Esc` closes whatever is open,
innermost first. Every row takes focus and announces itself: a parent that opens
a column is a `button`, a row that goes somewhere is a `link`, and the active
row carries `aria-current`. Focusing a glyph in the collapsed rail shows its
label — without that a keyboard user tabs through eleven unnamed marks.

## Escape belongs to the topmost layer

The shell handles Escape for its own transient layers — the palette, the scope
switcher, the identity panel, an open drill-in column — and calls
`preventDefault()` when it consumes the press. A page mounted in the shell that
gives Escape its own meaning must bail on `e.defaultPrevented`, or closing a menu
will also perform the page's action. Usage does this: Escape drills up one scope,
but not while a shell overlay is open.

## Reduced motion

**Deliberately no branch**, and worth stating because this component does run
timers. Every mark it makes — the tick on a count that just changed, the flash on
a name field that has hit its cap — is a CLASS or a data attribute that
`shell.css` animates; the timers only take the class back off again. So the
blanket rule in `tokens/motion.css` removes the motion and leaves the new value
sitting there, which is the right outcome: the count is the information, the tick
is the attention.

The rail's own width transition is CSS, covered by the same rule.

## A note on the props contract

This file's props are now the contract — the type is on the component. The
hand-written `Shell.d.ts` it replaces had already drifted twice: the `mode`
doc-comment had come loose and was sitting above `offline`, and `onTitleSave`,
`queued`, `counts` and `user` were props the component read and the declaration
never mentioned. That is the failure mode a separate declaration file has, and it
is why there is no longer one.

## Changed in the port

The design bundle's version is the reference; these are the deliberate
differences, and each one is a fix rather than an accommodation.

**The `alert` glyph now exists.** The governance row asked for one and the glyph
table never defined it, so the lookup returned `undefined` and the fallback fed
the literal string `"alert"` to a path's `d`. An invalid path renders as
nothing, silently — invisible until someone wonders why the rail's one warm mark
has no icon beside its count. `GlyphName` is now a type derived from the table,
so a row cannot name a glyph that was never drawn.

**`open` is derived, not synchronised.** The bundle held `open`, a `temp` flag,
and an effect putting them back, to express "a panel widens the rail, and
closing it gives the rail back". That is one fact derived from two others, so it
is now `pinned || scope || me`. The transient expansion is not merely careful
not to write the pin — there is no longer anything it could write.

**The count marks are a remount, not a timer.** `sh-tick` is a one-shot CSS
animation, so holding React state and a 520ms timer to add the class and take it
off again did by hand what remounting the node does for free. The count element
is keyed by the value it displays: the number changing and the mark playing are
now the same event, and there is nothing left switched on if a timer is
cancelled mid-flight. One consequence, accepted: a count that is already known
at first paint marks itself once on arrival. It is still a value arriving, and
it is still marked once.

**Counts accept `null`.** Not known yet reads as `--`, exactly like offline.
`0` is a claim — "nothing needs you" — and a rail has no business making it
before the count has loaded. The same reasoning as the offline rule, one step
earlier.

**Governance is behind a flag.** It has a rail row and a warm alert tail in the
design and no page in this app, so `flags.governance` is off until one exists.
Absent, not disabled — the rule the rail applies to features, applied to us.

**The scope switcher and the appearance control do something.** Both were inert
in the bundle. Teams come in as `teams` and report through `onTeamChange` **by
id** — a display name is the row's face, not its identity, and nothing makes
names unique;
appearance is `theme` + `onThemeChange`, owned by `lib/theme.ts` in the adapter,
so the rail's control and the Settings page cannot disagree.

**Presentational, with the router in an adapter.** This component takes route
ids and hands them back through `onRoute`; `src/Shell.tsx` maps them to paths
and supplies the data. That split is what lets `/dev/ui` mount all four grant
permutations at once, which is the only way the rail's behaviour across grants
gets reviewed — and that behaviour is the design.
