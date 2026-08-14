# src/ui — the design system

The rules a component in here follows. Each one exists because it was got wrong
first; the reasoning is kept so the next component does not have to rediscover
it.

Most of this arrived with the design bundle that `Shell` and the team settings
surface were ported from. Where this file and that one differ, this one is
current — it is the version that has met the build.

---

## What belongs here

**The test is one question: does this file know that Triage Factory exists?**

If it does — if it fetches, or names a task, a run, a team, an org — it is a
feature component and belongs in `src/components/`. If it does not, it belongs
here.

The operational form of the same test: **if you cannot mount it on `/dev/ui`
with hand-written props and no network, it does not go in `src/ui/`.**

This is enforced, not merely encouraged. `eslint-rules/ui-no-app-imports.js`
fails the build if anything under `src/ui/` imports from outside it. An
unenforced boundary erodes in about six weeks; this one cannot.

Consequences worth stating plainly:

- A `ui` component never imports `lib/api`, `hooks/*`, `contexts/*` or
  `types.ts`. It takes data as props and reports events as callbacks.
- `Shell` is **not** a `ui` component. There is exactly one of it, and it knows
  routes, grants and deployment mode. It lives at `src/Shell.tsx`.
- A component that needs org state does not get an exception. It gets a prop.

## Every component folder

```
src/ui/<name>/
  <Name>.tsx        plain ESM, className + data attributes
  <name>.css        all structure, color and state
  <Name>.md         why it is shaped this way and what not to change
  <Name>.test.tsx   behaviour, not appearance
```

The `.md` is the part that cannot be reconstructed from the code, and it is the
first thing to read before changing a component. The `/dev/ui` entry is what
makes the behaviour reviewable without reading any.

---

## Tokens

**Every value resolves through the token layer.** `src/tokens/*.css`. No literal
hexes except vendor marks — Jira blue, the Slack hues — and GitHub's brand
black, which draws in `--color-ink-1` so it survives either ground.

**Every token sits in a Tailwind v4 namespace**, so one declaration is
consumable both ways: `className="bg-ground"` from a page, `var(--color-ground)`
from a component stylesheet. There is no alias layer, because an alias layer is
the translation step the namespaces exist to remove.

**Ramps are ordinal, never alpha-encoded.** `--color-ink-1..4`,
`--color-tint-1..3`, `--color-line-1..2`, `--color-warm-1..4`. What you reason
about is rank on a ramp — "one step stronger" — which holds in both lighting
conditions. Alpha does not: `--color-warm-1` is 8% in light and 10% in dark, so
a name carrying the number would already be lying.

**One theme mechanism.** Light is the default; dark is `.dark` on `<html>`,
applied before paint by the inline script in `index.html` and owned by
`lib/theme.ts` after that. There is deliberately no `prefers-color-scheme` arm
in the token layer — our convention is `.dark`-or-nothing, so a media query
would hand the dark palette to a user who explicitly chose light. OS-preference
resolution is the `auto` setting, and it belongs to `lib/theme.ts`, the only
layer that knows whether a preference was stored.

**Two Tailwind traps, both silent, both already paid for:**

1. **A composite token cannot carry a light/dark difference.** Tailwind expands
   `--shadow-*` at build time, so a `.shadow-float` generated from a literal
   rgba keeps that literal forever and a `:root.dark` override never reaches
   it. Put the varying part in a **color** token and mark the composite
   `@theme inline`. See `--color-cast` in `tokens/elevation.css`.
2. **Some names are already taken.** `max-w-prose` is a Tailwind built-in
   pinned at 65ch; a `--container-prose` token is shadowed by it rather than
   overriding it, with no warning. Ours is `--container-measure`.

## Authoring

**One style: `className` + a component stylesheet.** Not consistency for its own
sake. Inline styles cannot express `:hover`, `:focus-visible`,
`::before`/`::after`, `@media` or `[data-state]`, so a component that uses them
ends up tracking hover in React state — a state machine that has to stay correct
across mouseleave, scroll, disabled rows, re-render, unmount, and the pointer
leaving the window. A `:hover` rule cannot get stuck. "Hover state belongs to
the pointer, not to the last event" was a written contract precisely because the
structure did not make it automatic. Now it does.

**State is a data attribute; variants are selectors.** `[data-on]`,
`[data-tone='alarm']`, `[data-phase='flash']`, `[data-clipped]`. Branching in a
style object is the thing being replaced.

**Inline style is for measured values only** — a grid from a sizing pass, a
per-row animation delay, a position from `getBoundingClientRect`, a per-instance
size. Hand them to CSS as custom properties (`--tb-grid`, `--check-size`) so the
rule stays in the stylesheet and only the number is inline.

**The props type is the contract.** No hand-written `.d.ts`. They drift, and in
the source bundle they provably had: `Shell.d.ts` declared props the component
did not read and missed four it did.

## Accessibility

**A declared role is a promise.** A `role="checkbox"` with no tab stop and no
key handler announces a control that can never be operated — worse than saying
nothing. `aria-modal="true"` while Tab walks out into the page behind is the
same defect.

**Half a promise is worse than none.** `role="grid"` commits to a
two-dimensional focus model — arrows cell to cell, home/end, page up/down. Claim
`aria-sort` and row semantics without claiming cell navigation, rather than
claiming `grid` and implementing a third of it.

**State the keyboard contract in the `.md`.** Does space toggle? Is the board
reorderable by keyboard? What is the tab order through a row? It is a design
decision, not an attribute, and if it is not written down it gets guessed.

**Absent, not disabled.** A verb a viewer may never use is removed, along with
every affordance that belonged to it — the grab cursor, the tab stop, the
checkbox whose only purpose was that verb. A control that 403s is not
information.

**Naming belongs to whoever labels the control.** A wrapper that claims an
accessible name over children carrying their own produces a mismatch between the
announced name and the visible one — WCAG 2.5.3, and it lands hardest on
irreversible actions.

## Motion

**CSS animation is covered; JS timing is not.** `tokens/motion.css` blankets
`animation` and `transition` under `prefers-reduced-motion: reduce`, and a media
query cannot reach `setTimeout` or `requestAnimationFrame`. Any component whose
motion is JS-timed reads the preference itself via
`src/ui/shared/useReducedMotion.ts` **and states its answer in its `.md`.**

The three answers, in order of preference:

1. **Skip to the end state.** The end state is the information; the beats are
   the attention. Removing the motion should remove the explanation, never the
   fact.
2. **Coarsen it.** Where the motion *is* the information — a countdown's
   remaining time — step it once a second instead of once a frame. Removing it
   entirely would be information loss dressed as an accommodation.
3. **Nothing**, where the motion is already discrete or already driven by a
   class the blanket rule covers. State it anyway, so the next reader does not
   have to work it out.

**Never shorten a safety gesture.** A press-and-hold duration is a pressure, not
an animation. A destructive commit that completes faster because someone asked
for less motion is a defect.
