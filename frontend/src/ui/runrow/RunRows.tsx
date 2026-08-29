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
// trails the prose in small mono, the age closes the row in mono, the source is
// a glyph. Two sentence-shaped strings of equal rank on one line give the row
// two centres of gravity and open a ragged trench between them — that shape was
// tried and cut.
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
  /** Elapsed, pre-formatted: `40s`, `11m`, `18h`. The row does no time math. */
  age: string
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

export function RunRows({
  rows = [],
  onPick = null,
  empty = null,
  label = null,
  count = null,
  more = null,
  note = null,
}: RunRowsProps) {
  const list = (
    <div className="rr">
      {rows.map((r) => {
        const live = r.lifecycle === 'working'
        const clickable = r.nav !== false && !!r.href
        // The hint is one string for both the tooltip and the mark's
        // aria-label — computing it twice is how the visible words and the
        // announced words drift apart.
        const hint =
          r.queue == null
            ? null
            : r.queue === 0
              ? 'Next in the queue'
              : r.queue + ' runs ahead of this one'
        const body = (
          <>
            <span className="rr-tick" />
            <svg
              className="rr-ico"
              viewBox="0 0 16 16"
              fill="none"
              strokeWidth="1.4"
              strokeLinecap="round"
              strokeLinejoin="round"
              aria-hidden="true"
            >
              <path d={GLYPH[r.source]} />
            </svg>
            <span className="rr-line">
              <Scan className="rr-act" active={live}>
                {r.activity}
              </Scan>
              {r.ref ? <span className="rr-ref">{r.ref}</span> : null}
            </span>
            <span className="rr-tail">
              <span className="rr-age">{r.age}</span>
              {hint != null ? (
                /* A real tooltip rather than the native `title` this used to
                   carry: `title` waits about a second, cannot be styled, and
                   on a row that is itself an anchor it competes with the
                   browser's own link hint.

                   focusable={false} because the row IS the <a>. A tab stop in
                   here would be invalid — an anchor may not contain
                   interactive content — and pointless, since the row already
                   takes focus. So the tooltip is scenery, and the words reach
                   assistive technology through the mark's own aria-label.

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
              if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
              e.preventDefault()
              onPick?.(r)
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
