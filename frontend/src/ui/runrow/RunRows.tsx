import { useEffect, useRef, useState } from 'react'
import type { MouseEvent, ReactNode } from 'react'
import { Scan } from '../scan/Scan'
import { Tooltip } from '../tooltip/Tooltip'
import './runrow.css'

// RunRows — the agent-run list, one row per run. NEEDS YOU and RUNNING both
// instantiate it: needing you and running are two states of one thing, not two
// kinds of thing. If a third surface needs rows, it mounts this — the Overview
// carried its own inline copy of this design for a while, and one tooltip fix
// had to be made twice before the page was repointed here.
//
// ONE prose string per row. The row leads with what is happening and everything
// else is demoted so it cannot be mistaken for a second sentence: the reference
// in small mono, the age closing the row in mono, the source as a glyph. Two
// sentence-shaped strings of equal rank on one line give the row two centres of
// gravity and open a ragged trench between them — that shape was tried and cut.
//
// When the prose is too long for the row it dissolves; the reference stays
// whole. The row's identity is the thing that must survive a narrow window.
//
// There is no title, deliberately. An agent run has no name of its own: what
// identifies it is the work, and the work is already the prose. Where a run
// hangs off a pull request or a ticket, the reference names it; where it does
// not, the activity carries it alone.
//
// Two axes, never conflated. `lifecycle` is where the run is; `asks` is whether
// it wants a person — a done run holding an open pull request still asks. A
// working run SCANS its activity (ui/scan): that motion is emission, an agent
// acting, and nothing else in the product may use it. A row that asks takes the
// warm tick whatever its lifecycle, and warm is only ever this.
//
// A QUEUED run is the one case where the activity cannot describe itself —
// "waiting for a slot" is the absence of work, and spending the row's only
// sentence on it means a queue of six rows all say the same thing. So the wait
// becomes a mark: an hourglass and the places ahead, and the prose names the
// work the run will do when it starts.
//
// Every row goes to its run, and a row does not ACT — no claim, no requeue, no
// snooze. The Board owns the verbs; a second place to act on a run is a second
// place for the two to disagree about its state. One target per row is also
// what makes a 35px row comfortable.

/** Where the work came from. Names the SOURCE, not the exact event: six event
 *  types do not want six glyphs, and the words on the row carry the specifics. */
export type RunSource = 'pull' | 'ticket' | 'manual' | 'alert'

/** Where the run IS. The design language's first axis. */
export type RunLifecycle = 'queued' | 'working' | 'done' | 'failed'

export type RunRowItem = {
  /** Stable key — the conversation id. */
  id: string
  source: RunSource
  lifecycle: RunLifecycle
  /**
   * The one prose string on the row, and the only one. While working it is the
   * agent's current action in present tense; when the row asks, it is the ask.
   * Never a title.
   */
  activity: string
  /** The source's own reference, written the source's way: `repo#772`, `SKY-412`. */
  ref?: string | null
  /** Elapsed, pre-formatted: `40s`, `11m`, `18h`. The row does no time math —
   *  a caller who wants the age to stay live passes a self-updating node
   *  (ui/shared/LiveText) rather than teaching the row about clocks. */
  age: ReactNode
  /**
   * The run wants a person. Independent of lifecycle — a done run holding an
   * open pull request still asks. Drives the warm tick, and warm is only ever
   * this.
   */
  asks?: boolean
  /**
   * Places ahead of it in the org's own queue. Set only on a queued run: it
   * draws an hourglass and a warm count after the age.
   */
  queue?: number | null
  /** The run view this row navigates to. A real href so the row opens in a new
   *  tab like any link; in-app navigation goes through onPick. */
  href?: string
  /** Opt one row out of navigation — for a run with nowhere to go, not for a
   *  viewer without permission. Every run is readable. */
  nav?: boolean
}

export type RunRowsProps = {
  rows?: RunRowItem[]
  onPick?: ((row: RunRowItem) => void) | null
  /** Shown in place of the list when there are no rows. */
  empty?: ReactNode
  /** Section label, e.g. NEEDS YOU. Omit to render the list bare. */
  label?: ReactNode
  /** Rendered after the label, ALREADY COLORED — the accent belongs to the
   *  section, not to this component: NEEDS YOU is warm, RUNNING is cool, and a
   *  third section could carry a third tone without this file changing. */
  count?: ReactNode
  /**
   * A link under the list for the rows it does not show — "+4 more on the
   * board". The caller passes its own anchor (a router Link with a real href);
   * this component only owns the slot, aligned to the prose column so it reads
   * as belonging to the list rather than to the section.
   */
  more?: ReactNode
  /** Mono line under the list — what the list excludes, say. */
  note?: ReactNode
  /**
   * Which of the row's two identities leads.
   *
   * `activity` puts the prose first and trails the reference behind it. Reads
   * best when the rows are a feed you scan top to bottom for WHAT is
   * happening.
   *
   * `ref` gives the reference its own column ahead of the prose. The prose
   * then grows and shrinks to the RIGHT of a fixed anchor, so a working row
   * whose current action changes every few seconds no longer drags its own
   * identity back and forth across the row. Costs the prose the leading
   * position and one column of width; ignored when no row in the list carries
   * a reference.
   */
  lead?: 'activity' | 'ref'
  /**
   * Only read when `lead` is `ref`. `true` puts the reference in its own grid
   * column, so every sentence in the list starts on the same line. `false`
   * leaves it packed against the prose in the row's own flex line: the order
   * changes, the column does not, so each row starts its sentence wherever its
   * own reference happens to end and the list's left edge of prose goes
   * ragged. The reference itself no longer moves either way — it is first now.
   */
  anchor?: boolean
}

// Lucide hourglass on the rail's 16 grid, for a queued run's wait.
const HOURGLASS =
  'M3.33 14.67h9.33M3.33 1.33h9.33' +
  'M11.33 14.67v-2.78a1.33 1.33 0 00-.39-.94L8 8l-2.94 2.94a1.33 1.33 0 00-.39.94v2.78' +
  'M4.67 1.33v2.78a1.33 1.33 0 00.39.94L8 8l2.94-2.94a1.33 1.33 0 00.39-.94V1.33'

// Glyph paths lifted from the shipped rail (ui/shell/glyphs.tsx), so a row
// draws from the same hand as the frame around it. An icon names the SOURCE —
// pull request, tracker item, hand-started run, failure — not the exact event.
const GLYPH: Record<RunSource, string> = {
  pull:
    'M6 4a2 2 0 11-4 0 2 2 0 014 0M14 12a2 2 0 11-4 0 2 2 0 014 0' +
    'M8.7 4h2A1.3 1.3 0 0112 5.3V10M4 6v8',
  ticket: 'M3 4h10M3 8h10M3 12h6',
  // Lucide message-square: a hand-started run is a conversation.
  manual:
    'M14 10a1.33 1.33 0 01-1.33 1.33H4.67L2 14V3.33a1.33 1.33 0 011.33-1.33h9.33A1.33 1.33 0 0114 3.33z',
  alert: 'M8 5.4v3.2M8 11.1h.01M8 2.2l6 10.4H2z',
}

/** The row's tone: asks takes warm whatever the lifecycle; failure takes its
 *  own mark because it should not have to be read to be seen. */
function tone(row: RunRowItem): 'warm' | 'alarm' | 'cool' | 'quiet' {
  if (row.asks) return 'warm'
  if (row.lifecycle === 'failed') return 'alarm'
  if (row.lifecycle === 'working' || row.lifecycle === 'queued') return 'cool'
  return 'quiet'
}

// Split a reference so the identifying half survives truncation. Everything
// from the last `#` is the tail and is pinned; the rest ellipsizes into it. A
// reference with no `#` has no dispensable half, so it stays one piece.
function splitRef(ref: string): [string, string] {
  const i = ref.lastIndexOf('#')
  return i > 0 ? [ref.slice(0, i), ref.slice(i)] : [ref, '']
}

// Mono characters that fit the leading reference's column at 11px — 22 for
// the full 152px cap, 17 for the 116px it steps down to under a 470px list.
// Kept in step with the caps in runrow.css by hand: they decide only whether
// a tooltip is offered, so being a character or two out costs a tooltip on a
// reference that just fits, not a wrong layout. The pair has to follow the
// list's width the same way the CSS container query does, or an 18-character
// reference clipped by the narrower column would offer no hover to recover
// its text.
const REF_CAP = 22
const REF_CAP_STEPPED = 17

// `value`, not `ref` — React claims that name on any element.
function Ref({ value, lead, cap = REF_CAP }: { value: string; lead: boolean; cap?: number }) {
  if (!lead) return <span className="rr-ref">{value}</span>
  // A hand-started run has no upstream entity, and in lead position the column
  // is still there — an empty cell reads as a rendering fault rather than as
  // an absence. So the absence is drawn: an em dash, the same mark a table
  // uses for a value that does not exist, dimmer than a real reference so it
  // cannot be mistaken for one. Not the word "manual": the glyph beside it
  // already says that, and this column answers WHICH one, not WHAT KIND.
  if (!value)
    return (
      <span className="rr-ref rr-ref-none" aria-hidden="true">
        —
      </span>
    )
  const [head, tail] = splitRef(value)
  const mark = (
    <span className="rr-ref">
      <span className="rr-ref-head">{head}</span>
      {tail ? <span className="rr-ref-tail">{tail}</span> : null}
    </span>
  )
  // Only where the reference can actually be clipped. Not focusable: the row
  // is the anchor, and the words are already in the accessible name of the
  // link — the tooltip restores only what the clipping hid.
  return value.length > cap ? (
    <Tooltip content={value} focusable={false}>
      {mark}
    </Tooltip>
  ) : (
    mark
  )
}

// Below NARROW the reference column is dropped and the reference moves onto
// the source glyph as a tooltip; between it and STEP the column holds at the
// tighter 116px cap (the CSS container query's threshold, measured on the
// same element). On the LIST, not the window: the same list is a 430px column
// on Overview and a full-width pane on a card.
const NARROW = 400
const STEP = 470

export function RunRows({
  rows = [],
  onPick = null,
  empty = null,
  label = null,
  count = null,
  more = null,
  note = null,
  lead = 'activity',
  anchor = true,
}: RunRowsProps) {
  const listRef = useRef<HTMLDivElement | null>(null)
  // Three bands, one observer: state moves only on a band crossing, so a
  // pixel-by-pixel resize re-renders nothing.
  const [band, setBand] = useState<'wide' | 'stepped' | 'narrow'>('wide')
  useEffect(() => {
    const el = listRef.current
    if (!el || typeof ResizeObserver === 'undefined') return
    const ro = new ResizeObserver(([e]) => {
      const w = e.contentRect.width
      setBand(w < NARROW ? 'narrow' : w < STEP ? 'stepped' : 'wide')
    })
    ro.observe(el)
    return () => ro.disconnect()
  }, [])
  const narrow = band === 'narrow'
  const refCap = band === 'stepped' ? REF_CAP_STEPPED : REF_CAP

  // A ref column with nothing in it is 11px of gap in front of every sentence,
  // so the lead only takes effect if there is something to lead with.
  const refLead = lead === 'ref' && rows.some((r) => r.ref)
  // Narrow: the reference stops being a column and becomes the glyph's
  // tooltip. A 92px column is not an anchor, it is a stub with an ellipsis in
  // it, holding the width the sentence needs to say anything at all. The glyph
  // is already the row's source mark, so the source's own reference is the one
  // thing that can hang off it without spending any of the line.
  const refFirst = refLead && anchor !== false && !narrow
  const refOnGlyph = refLead && anchor !== false && narrow
  const list = (
    <div ref={listRef} className={'rr' + (refFirst ? ' rr-lead-ref' : '')}>
      {rows.map((r) => {
        const live = r.lifecycle === 'working'
        const clickable = r.nav !== false && !!r.href
        const named = refOnGlyph && !!r.ref
        // The hint is one string for both the tooltip and the mark's
        // aria-label — computing it twice is how the visible words and the
        // announced words drift apart.
        const hint =
          r.queue == null
            ? null
            : r.queue === 0
              ? 'Next in the queue'
              : r.queue + ' runs ahead of this one'
        const glyph = (
          <svg
            className="rr-ico"
            viewBox="0 0 16 16"
            fill="none"
            strokeWidth="1.4"
            strokeLinecap="round"
            strokeLinejoin="round"
            aria-hidden={named ? undefined : true}
            role={named ? 'img' : undefined}
            aria-label={named ? (r.ref ?? undefined) : undefined}
          >
            <path d={GLYPH[r.source]} />
          </svg>
        )
        const body = (
          <>
            <span className="rr-tick" />
            {/* The glyph carries the reference only where the column is gone,
                so the hover is never an echo of words already on the row. Not
                focusable — the row is the anchor — and the words reach
                assistive technology through the glyph's own aria-label. */}
            {named ? (
              <Tooltip content={r.ref} focusable={false}>
                {glyph}
              </Tooltip>
            ) : (
              glyph
            )}
            {/* Rendered even when empty: the rows are subgrids of one grid and
                cells place in source order, so a row that skipped its
                reference would slide its prose into the reference's column. */}
            {refFirst ? <Ref value={r.ref || ''} lead={true} cap={refCap} /> : null}
            {/* Prose and tail in ONE cell, divided per row — see runrow.css. A
                tail track shared across the list makes every plain row's
                sentence stop short of a queued row's mark. */}
            <span className="rr-body">
              <span className="rr-line">
                {/* Unanchored lead: the reference rides in the row's own flex
                    line ahead of the prose. flex:none keeps it whole, so it is
                    the prose that dissolves — same rule as when it trails. */}
                {refLead && !refFirst && !refOnGlyph && r.ref ? (
                  <Ref value={r.ref} lead={false} />
                ) : null}
                <Scan className="rr-act" active={live}>
                  {r.activity}
                </Scan>
                {!refLead && r.ref ? <Ref value={r.ref} lead={false} /> : null}
              </span>
              <span className="rr-tail">
                <span className="rr-age">{r.age}</span>
                {hint != null ? (
                  /* A real tooltip rather than the native `title` this used to
                     carry: `title` waits about a second, cannot be styled, and
                     on a row that is itself an anchor it competes with the
                     browser's own link hint.

                     focusable={false} because the row IS the <a>. A tab stop
                     in here would be invalid — an anchor may not contain
                     interactive content — and pointless, since the row already
                     takes focus. So the tooltip is scenery, and the words
                     reach assistive technology through the mark's own
                     aria-label.

                     The mark is the whole trigger, dot and glyph included, so
                     the pointer never has to find a 15px pill exactly. */
                  <Tooltip content={hint} focusable={false}>
                    <span className="rr-q" role="img" aria-label={hint}>
                      <span className="rr-q-dot" aria-hidden="true">
                        ·
                      </span>
                      <svg
                        className="rr-q-ico"
                        viewBox="0 0 16 16"
                        fill="none"
                        strokeWidth="1.4"
                        strokeLinecap="round"
                        strokeLinejoin="round"
                        aria-hidden="true"
                      >
                        <path d={HOURGLASS} />
                      </svg>
                      <span className="rr-q-n" aria-hidden="true">
                        {r.queue}
                      </span>
                    </span>
                  </Tooltip>
                ) : null}
              </span>
            </span>
          </>
        )
        const cls = 'rr-row rr-' + tone(r)
        return clickable ? (
          <a
            key={r.id}
            className={cls}
            href={r.href}
            onClick={(e: MouseEvent) => {
              // Plain primary click stays in the app; modified clicks and
              // non-primary buttons keep the anchor's own behavior (new tab).
              // Keyboard activation reports button 0 and stays in-app too.
              // With no onPick there is no app route to stay in, so the
              // anchor navigates normally instead of swallowing the click.
              if (!onPick) return
              if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
              e.preventDefault()
              onPick(r)
            }}
          >
            {body}
          </a>
        ) : (
          <div key={r.id} className={cls + ' rr-inert'}>
            {body}
          </div>
        )
      })}
    </div>
  )

  return (
    <div className="rr-sec">
      {label ? (
        <div className="rr-head">
          <span className="rr-label">{label}</span>
          {count != null ? <span className="rr-count">{count}</span> : null}
        </div>
      ) : null}
      {rows.length ? list : <div className="rr-empty">{empty}</div>}
      {more != null ? <div className="rr-more">{more}</div> : null}
      {note ? <div className="rr-note">{note}</div> : null}
    </div>
  )
}

export default RunRows
