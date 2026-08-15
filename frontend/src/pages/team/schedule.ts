// The schedule a source page's left column builds on.
//
// Its own module rather than a constant beside the components, for the same
// reason `cells.tsx` sits beside `Table.tsx`: a file that exports both a
// component and a plain value cannot be fast-refreshed.
//
// Row `i`'s figure starts at 0.20 + 0.08i and its label lands at 0.70 + 0.08i,
// which is the prototype's own arithmetic. The consequence is the point: the
// last figure is still rolling as the first label arrives, so five rows read as
// one movement resolving rather than five things happening.

/** When row `i`'s figure begins rolling, in seconds. */
export const figureAt = (i: number) => 0.2 + i * 0.08

/** When row `i`'s label fades in, in seconds. */
export const labelAt = (i: number) => 0.7 + i * 0.08
