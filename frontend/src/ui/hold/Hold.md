# Hold

A commit you have to mean. Press and hold; the control takes a measurement and
fires when it completes. It replaces a confirmation dialog for actions whose
consequences are short enough to state in a tooltip.

## Why a hold instead of a dialog

A dialog asks you to read a sentence and click Yes, and the second time you meet
it you click Yes without reading. A hold cannot be dismissed by reflex: the cost
is 850ms of deliberate pressure, paid at the control, with no surface to appear
and disappear. Use a dialog when the consequences genuinely need prose; use a
hold when they need a moment's thought.

## The treatment

**Hover empties the button, then blinks a cross.** At rest it is filled, like any
other commit control. Point at it and the fill drains immediately — it stops
reading as pressable — then a tenth of a second later both diagonals appear at
once, hold for 0.09s, and go. Acquire's wireframe beat, and blinking rather than drawing
for a reason: a drawn line reads as construction, a permanent one reads as state,
and a blink reads as a reading being taken — the frame was checked and it is empty.
The gap is load-bearing; arriving with the drain would make it one flourish
instead of two statements.

**Pressing expands from your finger.** Progress is not a bar sweeping in from an
edge you did not choose; it grows outward from the exact point you pressed, in
discrete rings, with one bright ring at the frontier. The commit is measured from
you, so the geometry starts at you. The radius is computed to the farthest corner
from that point, so a hold started at an edge still finishes everywhere at once.

**The rings are stepped, not smooth.** A smooth fill reads as **waiting** — what a
download does to you. Discrete stops read as **counting** — what an instrument
does for you. The count (`4/9`) is real, in mono, because the machine is speaking.

**Landing flashes the frame twice**, hard on and off: the same statement Acquire
makes when a value arrives.

## Implementation notes

**The steps come from an interval in state, not from CSS delays.** A keyframe
track can be created but not started, or inherit a part-elapsed clock on a
re-press — both produce a gesture that is quicker the second time. State also
makes releasing instant and the count honest.

**Pointer capture is mandatory.** The label changes width mid-hold, so the button
resizes under the cursor; without capture a press near the edge lands outside the
element and the hold dies. `pointercancel` handles real interruptions;
`pointerleave` must NOT cancel, or the gesture works or fails depending on where
you pressed.

**Both inputs hold.** Space and Enter run the same engine as the pointer — Space
because it is the button convention, Enter because people try it — guarded against
auto-repeat, with Space's default prevented so the page cannot scroll under the
gesture. A keyboard hold starts from the centre, since there is no cursor to
measure from. Releasing the key or losing focus aborts it. A hold that is
pointer-only is a commit a keyboard user can stage and never make, which is worse
than a plain button; the `aria-label` also names the gesture, because "Hold to
save" only describes it visually.

**onConfirm fires after the landing flash**, not with it, so the surface does not
get pulled away mid-statement.

## Surface mode

`variant="surface"` turns Hold from a button into a measurement drawn across
something that already exists — a selection bar, a row, a card. It contributes no
chrome: the wrapper keeps its background, radius and shadow, and hands Hold its
rhythm through `--hld-pad`, `--hld-gap` and `--hld-radius`.

```html
<div style="border-radius:8px;background:…;--hld-pad:8px 14px;--hld-gap:16px;--hld-radius:8px">
  <Hold variant="surface" tone="alarm" trigger="[data-remove]">
    <span>2 selected</span>
    <span>Change role</span>
    <span data-remove>Remove from team</span>
  </Hold>
</div>
```

Use it when the container is already the affordance and a button inside it would
be one affordance too many. `trigger` keeps the gesture to matching verbs, so a
bar can carry several actions — but the rings still cross the whole surface,
because the surface is what the commit is about.

**A trigger can set its own price.** `data-hold-ms` on the matched element
overrides `ms` for that press, so one surface can carry a removal at 900ms and
ordinary verbs at 0 without a second Hold. `onConfirm` receives the element that
started the gesture, which is how the host tells which verb fired.

**Zero is the gesture's shortest form, not its absence.** At `ms: 0` the rings
arrive at once, settle for the same 220ms, and then the verb runs. An action
that applies with nothing on screen reads as a mis-click, and a product where
cheap verbs look nothing like expensive ones teaches the rings to mean
"something is wrong" rather than "something is committing".

Two differences from button mode. There is no cross at all: the wireframe beat
says "this region is reserved and empty", which is true of a button about to be
filled and false of a bar already full of labels — only the rings run. And the
landing flash leaves the text alone, since the surface holds several labels and
only one of them is the commit.

Children need `position:relative` to sit above the rings.

## Reduced motion

**Deliberately no branch.** The rings are already driven from an interval in
state, one discrete step at a time, and the only smooth part is the CSS
transition between steps — which the blanket rule in `tokens/motion.css` removes,
leaving rings that jump from stop to stop. That is the correct reduced-motion
rendering of this control, and it arrives for free.

What must **not** happen is the gesture getting shorter. `ms` is a safety
pressure, not an animation duration: a destructive commit that completes faster
because someone asked for less motion is a defect, not an accommodation.

## Naming, in each mode

**Button mode** — the control is the button, and its accessible name is
`label` + " — press and hold to confirm", so the announcement carries the
gesture as well as the verb.

**Surface mode with a `trigger`** — the wrapper is deliberately **not** a
labelled control. Each trigger is the control: it carries its own name, its own
tab stop, and its own `data-hold-ms`. The wrapper renders as a plain `div` with
no name and no role.

That is a fix, not a detail. It used to render as a `tabIndex={-1}` `<button>`
carrying `aria-label` from the `label` prop — which callers in surface mode have
no reason to pass, since they label their triggers. On the selection bar that
meant an unreachable control announcing **"Hold to save"** sitting over a bar
whose visible verb read **"Hold to remove"**: WCAG 2.5.3 (Label in Name), and on
an irreversible verb a plain lie about what the gesture does. It also nested
focusable `role="button"` spans inside a `<button>`.

A caller in surface mode therefore owns the naming of its triggers. `SelectionBar`
does this already — each verb span gets
`aria-label` of the verb plus " — press and hold to confirm" whenever its
`holdMs` is above zero, and just the verb when it is not.
