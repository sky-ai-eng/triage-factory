# NotifyStrip

The strip in Shell's `banner` slot, at the top of the page column above the
header. It shows urgent notifications until they resolve, and announces a high
one for 8s when it arrives.

```tsx
<Shell
  banner={<NotifyStrip urgent={urgent} arrival={arrival} onArrivalDone={next} onOpen={openItem} />}
  …
>
```

## Which priority goes where

| priority     | announcement                                  | lasts                        |
| ------------ | --------------------------------------------- | ---------------------------- |
| urgent       | the urgent line: 40px while unread, 26px read | until the condition resolves |
| high         | the arrival line, 26px                        | 8s, then only the rail count |
| low, notices | none                                          | the page and the count only  |

Toasts never carry notifications (`ToastLog.md`).

## Where it sits

Above the page header, inside the page column. The rail stays full height. The
slot pushes the header and the page down by the strip's height, and the shell
publishes that height as `--sh-banner-h` so the toast log clears it.

Two other placements were tried and rejected: across the whole window, which
moved the rail too, and under the header, which cost the header its place as
the page's top edge.

## The urgent line

- **Ground:** the strip is `--color-warm-1` with a `--color-warm-3` bottom
  edge. Urgent is warm, not `--color-alarm`: alarm sits 9° of hue from warm and
  reads as a mismatch beside the rail's warm marks, and alarm belongs on a
  container's frame, never a badge.
- **Unread: 40px**, gap 14.
  - Label: `URGENT · ORG` in 10px mono, 0.12em, warm.
  - Title: 600 12.5 ink-1, a button that calls `onOpen`.
  - Since: 10.5 mono ink-4.
  - Action: the item's first permitted action, as a solid warm button
    (`--color-warm` / `--color-warm-ink`).
  - With several urgent items open, a `‹ 1 of 2 ›` stepper.
- **Read: 26px**, gap 12. The same facts, smaller: a 9px label, a 500 12px
  title, the since, `· +N urgent` when there are more, and the action as a mono
  link. It keeps the warm ground until the condition resolves.
- **Which item:** the line starts on the first unread item, so an unread
  condition is never hidden behind a read one's `+N urgent`.
- **Read** means this viewer opened the item, took its action, or rendered it on
  the page (`mark-read`). Seeing the strip doesn't count.
- **No dismiss.** An urgent condition that can be dismissed is dismissed from
  the page.
- The height change between 40 and 26 is a CSS transition (160ms,
  `--ease-default`).

## The arrival line

- **Layout:** 26px, with the label `HIGH · TEAM INFRA`, the title as a button,
  and `open` as a mono link.
- **Clock:** a 1.5px `--color-warm-4` line at the foot runs out over
  `ARRIVAL_MS` (8s). Its WAAPI `finished` is the timer: it starts the close,
  and `onArrivalDone` fires once the line has closed.
- **Pausing:** it pauses while the pointer is over the strip or focus is inside
  it, and while the tab is hidden or the window blurred. Focus holds it for the
  same reason the pointer does: a button should not leave from under the
  keyboard that reached it.
- **Opening it** calls `onOpen`, then closes the line.
- **One at a time.** The caller queues the rest, and the next one opens as the
  last one closes.
- With an urgent line also showing, the arrival sits below it behind a
  `--color-warm-2` hairline.

## Motion

Each line opens and closes like a toast row:

- **Open:** the height goes 0→auto over 120ms (`--ease-default`); the content
  holds at opacity 0 for 40% of 300ms, then fades in.
- **Close:** the content fades over 120ms, then the height closes over 160ms
  (`--ease-fold`).

A closing line keeps its content on screen, so a resolved urgent item leaves
showing what it was rather than going blank. A line that opens again before it
has finished closing cancels the close. A line that closes holding focus blurs
it first, since a focused element that unmounts fires no focusout.

### Reduced motion

The open, the close and the clock are JS-timed (WAAPI), so the component reads
`useReducedMotion`.

- **Open and close:** the height changes are skipped (answer 1, skip to the end
  state), and lines appear and leave with a 120ms opacity change. The page
  still moves, in one step instead of a slide.
- **The clock:** steps once a second (answer 2, coarsen).
- **The 40↔26 change:** a CSS transition, so the motion blanket already removes
  it.

## Keyboard and assistive tech

- **Live region:** `aria-live="polite"` with `aria-relevant="additions"`, so a
  new line is announced once and a ticking `since` is not. It stays mounted at
  no height when empty, because a live region inserted together with its
  content is often not announced. It is a region named "Notifications" only
  while it shows something.
- **Tab stops:** the title, the action and the stepper's arrows are buttons. The
  strip adds no others.
- **Escape:** none. The strip has no transient layer to close.
- **`interactive={false}`:** titles and actions render as text, the stepper
  keeps its count without arrows, and only an inactive tab holds the clock.

## Don't

- **Don't use alarm for the ground or the label.** See above.
- **Don't let it overlay the page.** The slot takes height, so nothing it covers
  is lost.
- **Don't announce low items here, or put a notification in the toast log.** One
  place per priority.
