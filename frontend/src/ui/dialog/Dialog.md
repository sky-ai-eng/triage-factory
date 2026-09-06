# Dialog

A decision the product cannot make for you. Used for confirmations that have
consequences worth reading — never for "are you sure?".

## When to use it

Only when an action's effects are not obvious from where you clicked, or when it
is hard to undo. If the page already has a dirty bar with Revert, an ordinary
change does not need a dialog: the receipt is the safety net.

## Rules

**State consequences, not a question.** "Are you sure?" transfers the decision
without informing it. Write what will actually happen, one line each, in the
product's own terms. Read the code before writing them — a dialog that describes
behavior the backend does not have is worse than no dialog.

**Mark what is lost.** `tone: 'loss'` crosses the line and inks it; everything
else is dashed and quieter. The distinction is carried by the mark as well as the
color, so it survives grayscale.

**`kind: 'destructive'` is not a style choice.** It switches the head rule and
the confirm control to alarm, and alarm appears only when something is actually
lost. An ordinary confirmation uses warm.

**One decision per dialog.** No tabs, no forms, no second question. If the
dialog needs a text field, it is a settings surface wearing a dialog's clothes.

## A reading in a dialog

The slots above are prose-shaped, and some decisions need a **reading** first —
a token's figure band and its grid of address ranges, say — before the one verb
at the foot. `children` is that slot: free markup laid between `body` and
`note`, taking the same entrance stagger as the lines around it. Two props go
with it. `confirmHold` (milliseconds) makes the confirm control a `Hold`, so a
destructive verb reached from a reading takes the gesture every other
irreversible verb in this system takes rather than being the one place a click
suffices; it implies the destructive keyboard rules below whatever `kind` says.
`noConfirm` is a reading with nothing to decide — Cancel, labelled `Close`, is
the one control.

A reading is still one decision. A dialog that needs a text field, or a second
question, is still a settings surface wearing a dialog's clothes.

## Behavior

Escape cancels, Enter confirms, and the backdrop cancels. Focus lands on the
PANEL, never on the confirm control — an autofocused destructive button turns a
stray Enter or Space into a confirmation, and the ring on it reads as a decision
already half-made. Tab still reaches both controls. Escape is claimed in capture so a host that also handles it
(the shell does) cannot close something underneath instead.

The page stays visible behind a 62% wash and a 2px blur: the question is usually
about what you are looking at, so hiding it would be the wrong kind of modal.

## Elevation

It uses the float pair from `tokens/elevation.css` — `--edge-float` and
`--shadow-float` at radius 10px. In-flow content in this system is borderless;
only things that genuinely float get a frame, and a dialog is the clearest case.

## Build

It builds, in Acquire's vocabulary and at a dialog's pace — 1.05s end to end:

1. **Frame.** The panel settles in 0.14s. Empty.
2. **Flash.** The frame is claimed. Hard on, hard off — an eased flash reads as a
   glow, and a glow is ambience.
3. **Wireframe.** A cross corner to corner: the mark a wireframe puts in a box
   whose contents are not there yet.
3b. **Measure.** A caliper draws across the empty frame in the same window as the
   cross, so it costs no time: the frame is sized before anything is put in it.
   Revealed by a clip, never a scale — a scaled tick is a distorted tick.
4. **Empty.** The cross clears and the frame is briefly nothing but a frame. This
   beat is load-bearing: without it the lines arrive under the cross and the whole
   thing reads as one dissolve instead of four statements.
5. **Resolve.** The head rule draws, then the lines arrive in reading order on a
   0.05s stagger. Every line is on that one step — head, body, each consequence,
   the note — so it reads as a single pass down the panel. The list must not have
   its own separate cadence: two rhythms make the note either overtake the
   consequences it qualifies or arrive long after them.

`build="none"` skips the BEATS, not the motion: no flash and no measure, but the
panel still settles and the lines still resolve in reading order, at half the pace
— half the stagger, half the fade — because someone who has read it once is
reading it faster the second time. Contents that arrive fully formed read as a screenshot rather than a
surface. Use it for confirmations someone meets constantly, and
for anything on a hot path — a build is a second of not reading, and the whole
point of the dialog is the reading. The panel still settles rather than appearing
instantly, because a surface that arrives with no movement at all reads as a
glitch.

`build="quiet"` drops beat 2 and keeps the rest, finishing in 0.96s. Use it where
the dialog is routine — a confirmation someone meets several times a day — and
keep the flash for the ones that take something away. The choice is about how
often, not how serious: a flash you see forty times is wallpaper.

The reason it earns a build at all: a dialog is a region of the screen that did
not exist a moment ago, holding facts you have to read before acting. The
sequence says *this is new, and here is what is in it* — the same claim Acquire
makes about a value. It runs once per open, never on close, and never repeats.

## Keyboard and focus

The full contract, because `aria-modal="true"` is a promise about all of it:

- **Escape cancels**, in the CAPTURE phase, so a host that also claims Escape —
  the shell does — cannot close something else while this is the topmost layer.
- **Enter confirms.**
- **Focus lands on the confirm control**, not on the panel. Landing on the panel
  means the first Tab is spent getting to a verb.
- **Tab is contained.** Tab and Shift-Tab cycle within the panel. Until this
  existed, `aria-modal="true"` was a lie: Tab walked straight out into the page
  behind, where every control was still reachable and none of them was visible.
- **Focus returns** to whatever opened the dialog when it closes — but only if
  that element is still on the page, since a dialog that removed the row its
  trigger lived on has nothing to go back to.
- **The ring waits for the keyboard.** The trap's initial focus is script focus,
  which Chromium paints as `:focus-visible`, so a reading opened by a click
  arrived wearing a ring on Close. The panel carries `data-kb` from the first
  Tab or arrow press, and the ring shows only under it; a mouse user never sees
  one they did not ask for.
- **The backdrop cancels.** A dialog you cannot dismiss by looking away is a
  trap, and every confirmation here is reversible by not confirming.
- The panel is labelled by its own head (`aria-labelledby`) and described by its
  body (`aria-describedby`).

**One open question for the port.** Focus lands on confirm and Enter confirms,
which on a `kind="destructive"` dialog puts an irreversible verb under a stray
Enter. Everywhere else in this system a destructive verb is a press-and-hold
(`Hold`) precisely so that cannot happen. Two defensible answers — focus Cancel
when `kind="destructive"`, or drop the Enter shortcut there — and it is a product
call rather than a component one, so it is written down here instead of chosen
quietly.

**In the port, build this on Radix's Dialog.** The focus containment, the return,
`aria-modal` and the escape semantics all come free and correct, and this file
becomes the styling and the build sequence. The hand-rolled version here exists
because the prototypes take no dependencies.

## Reduced motion

The build is a state machine on `setTimeout`, not a keyframe track (see the
comment above `phase` for why), so the blanket rule in `tokens/motion.css`
cannot reach it. Under `prefers-reduced-motion: reduce` the dialog is simply
**built**: a question you have been interrupted to answer should be readable the
instant it appears, and the beats are the interruption.

## Resolved in the port

**The open question is answered: on a destructive dialog, focus lands on Cancel
and Enter does not confirm.** The contract above left this deliberately open, so
here is the reasoning rather than a silent choice.

Everywhere else in this system an irreversible verb is a press-and-hold, and
`Hold.md` is explicit that the point is a commit you cannot fire by reflex. A
focused, Enter-bound destructive button is the exact hazard that gesture exists
to remove — it makes the one action that cannot be taken back the single easiest
thing on the surface to trigger. So both halves go: the tab stop starts on
Cancel, and Enter is unbound. Reaching Confirm takes a deliberate Tab and a
deliberate press.

Ordinary confirmations are unchanged: focus lands on Confirm and Enter fires it,
because there the cost of a mistake is one undo and the cost of hunting for the
verb is paid on every open.

**Focus containment comes from `src/ui/shared/useFocusTrap`, not Radix.** The
contract recommends Radix's Dialog, but `@radix-ui/react-dialog` is not a
dependency and the project's other modals already share this primitive — its own
docstring says so. Tab containment, initial focus and focus return all come from
it; this file keeps the Escape and Enter semantics and the build sequence.

## One contradiction in the contract above

The **Behavior** section says focus lands on the panel, "never on the confirm
control". The **Keyboard and focus** section says it lands on the confirm
control, "not on the panel". These cannot both hold. The port follows the second
for ordinary dialogs — landing on the panel spends the first Tab getting to a
verb, which the second section argues and the first does not — and neither for
destructive ones, per the decision above. Worth settling upstream.
