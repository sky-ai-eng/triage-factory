import type { CSSProperties } from 'react'
import { Link } from 'react-router'
import { Glyph } from './Glyph'
import type { ArtifactTotals, PendingTotals } from './cardModel'

// ArtifactStrip — what the run has WRITTEN, on every card that has a run, and
// on a queued card carrying what a requeue handed back with it.
//
// Six kinds exist (branch, pull_request, review, issue, comment, message) and
// four of them are terminal the moment they are written. Only two shapes can
// ever await a human: a DRAFT pull request, and a PENDING review with the
// ready sentinel set. That is a domain fact, not a policy — see
// `HasUnresolvedArtifacts`. A kind carrying one of them draws warm.
//
// The strip is a record, so it opens the run page rather than answering
// anything in place. It sits at the card's right edge because it is a target
// and wants an edge to sit against; elapsed holds the left.
//
// It is not an attention signal and must not become one — the card's frame
// carries that, and the two read the same `pending` set so they can never
// disagree.

// Creation order in a run, so the strip reads as a sequence rather than a set.
const ORDER: Array<keyof ArtifactTotals> = ['branch', 'pulls', 'review', 'comment']

export function ArtifactStrip({
  artifacts,
  pending,
  href,
  style,
}: {
  artifacts?: ArtifactTotals
  pending?: PendingTotals
  /** The run page. Renders a link; without it the strip is a plain readout. */
  href?: string
  style?: CSSProperties
}) {
  const has = artifacts ?? {}
  const open: Record<string, number | undefined> = pending ?? {}
  const kinds = ORDER.filter((k) => (has[k] ?? 0) > 0)
  if (kinds.length === 0) return null

  const body = kinds.map((k) => (
    <span
      key={k}
      style={{
        display: 'inline-flex',
        alignItems: 'center',
        gap: '3.5px',
        color: (open[k] ?? 0) > 0 ? 'var(--color-warm)' : 'inherit',
      }}
    >
      <Glyph kind={k} />
      {has[k]}
    </span>
  ))
  const base: CSSProperties = {
    display: 'flex',
    alignItems: 'center',
    gap: '9px',
    fontFamily: 'var(--font-mono)',
    fontSize: 'var(--text-readout)',
    fontVariantNumeric: 'tabular-nums',
    letterSpacing: 'var(--tracking-readout)',
    color: 'color-mix(in srgb, var(--color-ink-3) 80%, transparent)',
    textDecoration: 'none',
    ...style,
  }
  // A link when it has somewhere to go, because it always DOES go somewhere:
  // this is the run page, never an in-place disclosure. The cursor says so
  // unconditionally there — a pointer that appears only once a handler happens
  // to be wired makes the affordance a property of the integration rather than
  // of the control.
  return href ? (
    <Link to={href} title="Open the run's artifacts" style={{ ...base, cursor: 'pointer' }}>
      {body}
    </Link>
  ) : (
    <span title="What the run wrote" style={base}>
      {body}
    </span>
  )
}

export default ArtifactStrip
