import type { ReactNode } from 'react'

// The vendor marks, as data rather than as part of the control. Their own
// module because a module exporting both a component and a lookup table is what
// react-refresh objects to — and these are shared: the source pages draw the
// same mark in their own headers.

// SourceCard — the tile an event source takes in a settings grid.
//
// One idea: THE CARD'S SURFACE SAYS WHAT YOU CAN DO WITH IT. A source this team
// has configured is a raised, bordered card carrying numbers and it takes a
// click. A source the organization has not connected is the same card sunk into
// the ground with its numbers replaced by the one sentence that would change
// that — it is real, it is just not yours to turn on. A source that does not
// exist yet is drawn as an outline only: nothing to sink, nothing to click.

// Brand marks live here so a caller passes a key rather than pasting paths.
//
// A live source wears its own colors — that is what makes a grid of sources
// scannable. The one exception is GitHub, whose mark is brand black: it draws in
// currentColor so source-card.css can put it in primary ink, which is the only
// honest way to keep a black logo on a ground that can go dark. (It used to
// carry fill="var(--color-ink-1)" — var() does not resolve in an SVG
// presentation attribute, so the mark fell back to default black and stayed
// black on the dark ground.) Sources that are not wired up desaturate instead.
// Linear and Schedule also take currentColor: neither is a connection yet, so
// neither has a color to wear.
export const ICONS: Record<string, ReactNode> = {
  github: (
    <svg width="24" height="24" viewBox="0 0 24 24" fill="currentColor">
      <path d="M12 .5C5.73.5.5 5.73.5 12c0 5.02 3.29 9.28 7.86 10.74.58.11.79-.25.79-.55v-2.1c-3.2.7-3.88-1.54-3.88-1.54-.53-1.34-1.29-1.7-1.29-1.7-1.05-.72.08-.7.08-.7 1.16.08 1.77 1.19 1.77 1.19 1.03 1.77 2.7 1.26 3.36.96.1-.75.4-1.26.73-1.55-2.55-.29-5.23-1.28-5.23-5.7 0-1.26.45-2.29 1.19-3.1-.12-.29-.52-1.46.11-3.05 0 0 .96-.31 3.15 1.18a10.9 10.9 0 0 1 5.74 0c2.19-1.49 3.15-1.18 3.15-1.18.63 1.59.23 2.76.11 3.05.74.81 1.19 1.84 1.19 3.1 0 4.43-2.69 5.4-5.25 5.69.41.36.78 1.06.78 2.14v3.17c0 .3.21.67.8.55C20.71 21.28 24 17.02 24 12 24 5.73 18.77.5 12 .5z" />
    </svg>
  ),
  jira: (
    <svg width="24" height="24" viewBox="0 0 24 24" fill="none">
      <path
        d="M11.53 1.5 3.1 9.93a.66.66 0 0 0 0 .94l3.9 3.9 4.53-4.53 4.53 4.53 3.9-3.9a.66.66 0 0 0 0-.94z"
        fill="#2684ff"
      />
      <path
        d="M11.53 22.5 19.96 14.07a.66.66 0 0 0 0-.94l-3.9-3.9-4.53 4.53L7 9.23l-3.9 3.9a.66.66 0 0 0 0 .94z"
        fill="#2684ff"
        opacity=".45"
      />
    </svg>
  ),
  slack: (
    <svg width="24" height="24" viewBox="0 0 24 24" fill="none">
      <rect x="8" y="1.5" width="5" height="9.5" rx="2.5" fill="#36c5f0" />
      <rect x="13.5" y="8" width="9" height="5" rx="2.5" fill="#2eb67d" />
      <rect x="11" y="13" width="5" height="9.5" rx="2.5" fill="#ecb22e" />
      <rect x="1.5" y="11" width="9" height="5" rx="2.5" fill="#e01e5a" />
    </svg>
  ),
  linear: (
    <svg
      width="24"
      height="24"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.5"
      strokeLinejoin="round"
    >
      <path d="M3.6 14.2 9.8 20.4" />
      <path d="M3.2 9.9 14.1 20.8" />
      <path d="M4.6 6.2 17.8 19.4" />
      <path d="M7.7 3.7 20.3 16.3" />
    </svg>
  ),
  schedule: (
    <svg
      width="24"
      height="24"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.5"
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      <circle cx="12" cy="12" r="8.2" />
      <path d="M12 7.2V12l3.2 2.1" />
    </svg>
  ),
}
