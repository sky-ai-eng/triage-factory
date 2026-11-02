# ToastLog

One panel of short replies to what the viewer just did, newest on top.

```tsx
import { ToastLog } from '../../ui/toastlog/ToastLog'

const items = useToast()
<ToastLog items={items} onDone={toastStore.dismiss} />
```

## What goes in it

A toast is a reply: an error from a failed request, a confirmation, an info
note about a side effect, or a warning. A condition that stays true until
someone acts on it, or a failure nobody was watching for, is not a reply, and
a toast is the wrong place for it.

## One panel, newest on top

Rows share one raised surface instead of stacking cards, so a burst reads as
one block and a single toast is about 38px tall. The surface is the float
treatment: `--color-raised`, `--edge-float`, `--shadow-float`, radius 8.

It sits 12px below the 55px shell header at `right: 16px`, and is 360px wide.

**At most five rows.** Past five, the oldest leaves early through the normal
exit. A row already dismissed with Escape does not count toward the five.

The panel stays mounted while it is empty and draws nothing. Its list is the
live region, and a live region inserted together with its first content is
often not announced at all.

## A row

A 14px glyph column, then the text:

- **Glyph:** 12px at stroke 1.7, centered on the body's first line (18px
  tall).
  - success: `--color-ink-1` check
  - info: `--color-ink-3` circled i
  - warning: `--color-warm` triangle
  - error: `--color-alarm` ×
- **Title (optional):** 600 12.5/1.45, ink-1.
- **Body:** 400 12.5/1.45, ink-1, `text-wrap: pretty`. A 160-character run
  failure wraps across the full measure.
- **Time:** follows the body's last word in 10px mono ink-4, `HH:MM`. It wraps
  with the text, so it never takes a column.
- **Action (optional):** under the body in 11px mono, underlined. **Navigation
  only.** It always has an `href`, so it is a tab stop and a modified click
  opens a new tab; `onSelect` takes a plain primary click in-app. A toast never
  retries: the store holds no request, and recovery belongs to the surface that
  failed.

No level word is drawn, because the glyph carries the level. Screen readers
hear one through a visually hidden prefix ("Error:", "Warning:", "Done:",
"Note:") read before the title.

An error differs from the other rows in exactly two ways: the red ×, and a
clock line drawn in `--color-alarm-2` instead of `--color-line-2`.

## The clock

The 1px line at the row's foot, inset 13/12, runs down the row's lifetime:

| level         | lifetime |
| ------------- | -------- |
| success, info | 3s       |
| warning       | 6s       |
| error         | 10s      |

The line is a Web Animations API `scaleX` animation, and **its `finished` is
the timer**. Pausing it pauses the lifetime, with no second `setTimeout` to
keep in step. It pauses:

- while the tab is hidden or the window is blurred, so an error that lands
  while you are elsewhere waits for you;
- while the pointer is over the panel or focus is inside it (interactive
  only).

A row that leaves while holding focus blurs it first. A focused element that
unmounts fires no focusout, so the panel would otherwise go on holding every
other row's clock.

## Motion

- **Enter:**
  - The row's height opens from 0 over 120ms (`--ease-default`), pushing older
    rows down.
  - The content stays at opacity 0 for the first 40% of 300ms, then fades in,
    so the space arrives before the words.
- **Exit:**
  - The content fades out over 120ms.
  - Then the height closes over 160ms (`--ease-fold`) and `onDone(id)` fires.
  - The parent removes the item there, not earlier, or the row unmounts
    mid-fade.

The easings are read off the token layer when each animation starts, since
WAAPI takes a string, not a `var()`.

A 6px drop and an unroll from the top-right corner were both tried for the
entrance and rejected; the fade is the one that does not move the words.

### Reduced motion

The motion is JS-timed (WAAPI), so the blanket in `tokens/motion.css` cannot
reach it. The component reads `useReducedMotion` itself. Its answers:

- **Enter and exit:** height changes are skipped (answer 1, skip to the end
  state). Rows appear and leave in place with a 120ms opacity change only.
- **The clock:** it steps once a second instead of running continuously
  (answer 2, coarsen). Time left is information.

## Interaction

- **Clicking a row does nothing.** People select and copy toast text,
  especially errors, and a click that dismissed would take the text away
  mid-selection. Hovering pauses the clock while they do it.
- **Escape dismisses the top row**: the newest one that is not already leaving.
  It is listened for on the window, so rows without a link (no tab stop) can
  still be dismissed.
  - It defers to anything nearer: if a dialog, a menu or the shell palette
    already handled Escape and marked it (`defaultPrevented`), the log ignores
    it.
  - It ignores Escape typed in a text field, and marks the event when it does
    claim it, so one press never does two things.
  - Each press takes one row, so repeated presses walk down the list.
- **Keyboard:** the action link is the only tab stop.
- **Screen readers:** the list is `aria-live="polite"` with
  `aria-relevant="additions"`. An error row's content is `role="alert"`, set
  inside the `<li>` so the item stays a list item.
- **`interactive={false}`:** no Escape and no hover or focus pause. The clocks
  still run and still wait for an inactive tab. Use it for demos and read-only
  mounts.

## Don't

- **Don't give it a close button, or make a click dismiss.** Errors time out,
  and Escape covers the impatient case. A click has to stay free for selecting
  text.
- **Don't reorder by severity.** Newest on top is the only order that keeps
  rows from jumping while they are read.
- **Don't add a level-colored edge or box.** The glyph and the clock carry the
  level; a frame per level turns a burst back into a stack of cards.
