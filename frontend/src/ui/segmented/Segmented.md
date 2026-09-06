# Segmented

One choice from two to five short options, all of them visible, taking effect
at once — appearance, an expiry preset, a lens, a scope. The sibling of a radio
group for the case where the options are words the system reports back rather
than a form's answers.

## The mark moves, the words do not

Whichever variant, the thing that says "this one" is a **single element** that
travels from the old choice to the new on the content curve
(`cubic-bezier(0.22, 1, 0.36, 1)`, 0.34s), so the eye reads a change of state
and not a redraw. Its place is measured — a label's width is the font's
business — and handed to the stylesheet as `--seg-x` / `--seg-w`; the
stylesheet owns everything else. The first placement is a jump: a control that
arrives with its mark sliding in from zero reads as a change nobody made.

## Variants

The four ways this system already marks a selection, so a page can pick the one
its neighbours use. Decided in the design canvas: **spine is the default.**

- **spine** — a 2px marker to the left of each word, lit on the chosen one. The
  scope switcher's spine, laid on its side: honest about there being N
  positions, at the cost of N marks. The user settings page's Appearance
  control.
- **tick** — a warm hairline under the word. Bare, no housing; what the Usage
  lens filter already draws, kept so it can migrate without changing shape.
- **housed** — an inset-hairline box, the chosen segment a tint fill. Fill
  before hue, so no warm at all.
- **plate** — a sunk track with a raised plate under the chosen word. The
  Toggle's knob, for N positions.

## A struck option stays in place

`disabled` on an option strikes it in ink-4 and takes its click away, with the
reason on `note` (a native `title`, so it is on hover). It is **not** removed:
absent is for verbs a viewer may never use, and a preset the org's policy rules
out is information — "never" struck beside "90 days" says what the cap is.
`disabled` on the whole control dims it to 40% and takes every click.

## Type

Mono by default, `.04em` tracking, because the options are values the system
reports back. `mono={false}` for words a person wrote, in sans half a step
larger. Three sizes: 9.5 / 10.5 / 11.5px mono.

## Keyboard and semantics

`role="radiogroup"` with the caller's `label`; each option `role="radio"` with
`aria-checked`. **Roving tabIndex**: the chosen option is the one tab stop.
Arrow keys (either axis) move the choice and the focus together, wrapping;
Home and End go to the ends; a struck option is skipped. The focus ring draws
on the group, keyboard only — `:focus-visible` on the option excludes a mouse
press, so a click never wears the ring a keyboard user needs.

## Do not

- Use it for six or more options. That is a `Select`.
- Use it for options that are not mutually exclusive. Those are chips or
  checkboxes.
- Put a verb in it. It is a state, and picking one is the whole gesture.

## Reduced motion

The slide is a CSS transition, so the blanket rule in `tokens/motion.css`
covers it: the mark jumps to the new choice. Nothing is lost — the choice is
the information, the travel was the attention.
