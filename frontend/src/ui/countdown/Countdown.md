# Countdown

The shape a timed window takes in this system: three stacked crates emptying top down, each draining left to right. Use it wherever something has been done and there is a bounded moment to take it back, or wherever a wait has a known end.

```jsx
// Self-driven — the component owns the clock.
<Countdown seconds={10} onDone={commit} />

// Controlled — the caller owns the clock (the usual case in a table, where
// the same timer fires the commit).
<Countdown frac={left / total} />
```

## Rules

**It drains, it does not fill.** The window is time being spent. A filling meter says "something is being built"; this says "something is running out".

**It does not tick.** The value moves continuously. A per-second jump reads as a stutter at 14px, and the window is a duration rather than a count of seconds. Never pair it with a number of seconds counting down — the number would be the thing read, and the dial would be decoration.

**The footprint never changes.** Empty crates stay drawn as outlines at 26% of the tone, so the mark holds its size at zero and nothing shifts around it.

**Three crates at small sizes.** `steps` above three only pays for itself above roughly 24px; at 14px a fourth crate is a smear.

## Where the clock lives

Give it `frac` when the caller already owns the timer — a table holding an undo snapshot must, since the same timer fires the commit and the dial would otherwise disagree with the data. Give it `seconds` when the countdown is the only consequence, and it will call `onDone` once at zero. `startKey` restarts a self-driven window without a remount.

## Tone

`warm` by default. `alarm` for a window on a destructive change, which pairs it with Hold's alarm tone — the pattern is hold to destroy, then a red window to take it back. `ink` for a neutral wait with no verb attached.

## Related

`Hold` measures a gesture before the fact; `Countdown` measures the window after it. A given action gets one or the other, never both: an action worth holding for is one you meant, and an action offering a window is one that applied immediately.

## Reduced motion

The drain is `requestAnimationFrame`-driven, so the blanket rule in
`tokens/motion.css` cannot reach it.

Here the motion is not decoration — it **is** the remaining time — so under
`prefers-reduced-motion: reduce` it is not removed, it is COARSENED: the dial
steps once per second instead of once per frame. The value still tells the truth
and the window still closes at the same moment; nothing moves smoothly. Removing
the drain entirely would leave a timed window with no way to read how much of it
is left, which is information loss dressed as an accommodation.

The comment about a per-second jump reading as a stutter still holds — a stutter
is the correct trade here, and it is the only place in the system that takes it.
