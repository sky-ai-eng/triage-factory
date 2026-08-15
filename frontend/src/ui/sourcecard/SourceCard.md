# SourceCard

The tile an event source takes in a settings grid. One card, three surfaces, and the surface is the whole message: raised and bordered for a source this team has configured, sunk into the ground for one the organization has not connected, an outline for one that does not exist yet.

```tsx
import SourceCard from './SourceCard'

<SourceCard
  name="GitHub"
  state="configured"
  scope="33 of 48 repositories"
  stats={[['events · 7d', 164], ['became tasks', 22, 'warm']]}
  onClick={goGithub}
/>

<SourceCard
  name="Slack"
  state="unavailable"
  scope="not connected"
  note="An org admin connects Slack once for the whole organization. Until then this team has no channels to watch."
  actionLabel="Ask an org admin"
  requestKey="sky-ai-eng:slack"
  onAction={requestSource}
/>

<SourceCard name="Schedule" state="soon" scope="coming soon" note="Runs the factory on a cadence you set, with no event to trigger it." />
```

## Rules

**Only a configured card is clickable.** There is nothing behind the other two, so neither takes a cursor or a hover. A card that lights up under the pointer and then does nothing is worse than a card that stays still.

**Numbers belong to configured cards only.** The inert states replace the stat block with the one sentence that explains them. A zero on an unconnected source reads as a measurement, when the truth is that nothing has been measured.

**`unavailable` is not `soon`.** They are different problems and they look different. Unavailable is a real integration behind an org-level decision, so it keeps a solid hairline, sinks into the ground, and carries a verb — something can be done about it. Soon is drawn as a dashed outline with no verb, because nothing can.

**A live source wears its own colors; an inert one gives them up.** Configured cards draw the real brand marks — that is what makes a grid of sources scannable. Both inert states desaturate the mark and step the name back to `--color-ink-2`: one asset, two readings, no badge needed. GitHub is the exception in either direction, drawing in primary ink rather than brand black, since black is the one brand color a dark ground destroys.

**The request is asked once, and the card says so in place.** Clicking the verb replaces it with a ticked `requested · waiting on an org admin` — no banner, no toast, nothing moves. It is recorded per user under `requestKey` and never offered again: a second request tells an admin nothing the first one didn't. Pass `requested` instead when the caller holds that record server-side.

## Keyboard and semantics

A configured card **navigates** — it opens that source's own page — so it is a
`role="link"` with `tabIndex={0}`, and **enter** activates it. Space
deliberately does not: space scrolls a page, and a link that swallowed it would
be the only one on the page that did. Its accessible name is `title` when given,
otherwise the source name; the mark is `aria-hidden` scenery.

The other two states take no role, no tab stop and no cursor. A card a reader
cannot use is not a control, and giving it a focus ring would promise otherwise.

`Ask an org admin` is a real `<button>`, so it is reachable and it stops the
click from reaching the card underneath.

## CSS

All three surfaces are `[data-state]` selectors in `source-card.css`; the
component branches on nothing but content. **Hover is a `:hover` rule, not React
state** — a grid of five cards used to hold five hovers, and a card whose warm
ring stayed lit after the pointer left was the recurring defect.

Requires `source-card.css`.

## Copy

`scope` is the status line under the name — a count when the source is live (`33 of 48 repositories`), a state when it is not (`not connected`, `coming soon`). Say what an org admin has to do in `note`, not what the reader cannot do: the reader is not the blocker.

## Related

Sits in the sources grid on team settings. The stat rows use the same dotted leader as `Readout`, and clicking a live card is what opens the per-source settings page.
