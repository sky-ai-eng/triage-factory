import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Link, useNavigate } from 'react-router'
import RunRows from './overview/RunRows'
import type { RunRowItem } from './overview/RunRows'
import Converge from './overview/Converge'
import type { ConvergeOutcome } from './overview/Converge'
import {
  donutSegments,
  money,
  needsRow,
  runningRow,
  seenLead,
  awayTail,
  utcMidnightISO,
  windowSums,
} from './overview/data'
import {
  useActivityWindow,
  useConversationSets,
  useOverviewSeen,
  useOverviewTick,
  useSpendToday,
  useTasksIndex,
} from './overview/hooks'
import { useShellScope } from '../hooks/useShellScope'
import { useOrgHref } from '../hooks/useOrgHref'
import { useWsConnected } from '../hooks/useWebSocket'
import { useModelCatalog } from '../hooks/useModelCatalog'
import { ACTIVE_STATUSES, workStartedAt } from '../lib/conversationStatus'
import type { Conversation } from '../types'
import './overview/overview.css'

// Overview — the first row in WORK, and the page nobody lands on: you land on
// your last page, not here. So it is not a dashboard you monitor. It is the
// page you open deliberately, once, to find out what happened while you were
// away and what is waiting for you.
//
// SCOPE is whatever the org · team mark says — the page has no scope control
// of its own, and changing team in the rail reframes every figure. That is
// what lets it be one page for every grant: a member and an org admin looking
// at the same team see the same numbers, because the numbers are the team's.
//
// READ-ONLY: every row links out to its run, and nothing here acts. The Board
// owns the verbs; a second place to act on a run is a second place for the two
// to disagree about its state. One target per row is also what makes a 35px
// row comfortable.
//
// THE SHAPE IS STATIC, deliberately: a fixed set of sections with
// deterministic conditional logic only — quiet, offline, nothing running —
// never a usage pattern or a stored preference. And NOTHING TICKS: the rail's
// counts are the frame's only moving numbers, ages are stamped per refetch,
// and the shimmer is emission, not a tick.

/** How many rows each section shows before deferring to the Board. */
const NEEDS_SHOWN = 3
const RUNNING_SHOWN = 6

export default function Overview() {
  const { teamId } = useShellScope()
  const orgHref = useOrgHref()
  const navigate = useNavigate()
  const connected = useWsConnected()
  const offline = !connected

  const tick = useOverviewTick()
  const anchor = useOverviewSeen()
  // Midnight is stamped once per mount: the convergence's window opens where
  // the day did, and a page that recomputed it per render would tick.
  const midnight = useMemo(() => utcMidnightISO(), [])

  const { needs, running } = useConversationSets(teamId, tick)
  const tasks = useTasksIndex(teamId, tick)
  const sinceMidnight = useActivityWindow(teamId, midnight, tick)
  // The away tail's window opens at the viewer's marker; with no marker the
  // page anchors to midnight and reuses that read rather than asking twice.
  const sinceSeen = useActivityWindow(teamId, anchor ?? null, tick)
  const usage = useSpendToday(teamId, tick)
  const catalog = useModelCatalog()

  const daySums = sinceMidnight ? windowSums(sinceMidnight) : null
  const seenSums = anchor ? (sinceSeen ? windowSums(sinceSeen) : null) : daySums

  // Offline is not a loading state: a readout goes inert rather than holding
  // the last number it happened to have — the same rule the rail applies to
  // itself. Unknown is not zero either, so a read that has not answered is a
  // dash too.
  const num = (v: number | null | undefined) => (offline || v == null ? '--' : String(v))
  const cash = (v: number | null | undefined) => (offline || v == null ? '--' : money(v))

  const runHref = useCallback((id: string) => orgHref(`/runs/${id}`), [orgHref])
  const onPick = useCallback(
    (row: RunRowItem) => {
      if (row.href) navigate(row.href)
    },
    [navigate],
  )

  const now = Date.now()

  const needsTotal = needs?.total ?? null
  const quiet = !offline && needsTotal === 0
  const needsItems = useMemo<RunRowItem[]>(() => {
    const rows = [...(needs?.rows ?? [])]
    rows.sort(
      (a, b) => Date.parse(b.CompletedAt ?? b.StartedAt) - Date.parse(a.CompletedAt ?? a.StartedAt),
    )
    return rows
      .slice(0, NEEDS_SHOWN)
      .map((c) => needsRow(c, tasks?.get(c.TaskID), runHref(c.ID), now))
    // eslint-disable-next-line react-hooks/exhaustive-deps -- `now` is stamped per data change, not a tick
  }, [needs, tasks, runHref])

  const runningItems = useMemo<RunRowItem[]>(() => {
    const rows = running?.rows ?? []
    const live = rows
      .filter((c) => (ACTIVE_STATUSES as readonly string[]).includes(c.Status))
      .sort((a, b) => Date.parse(workStartedAt(b)) - Date.parse(workStartedAt(a)))
    const queued = rows
      .filter((c: Conversation) => c.Status === 'queued')
      .sort((a, b) => (a.queue_position ?? 1e9) - (b.queue_position ?? 1e9))
    return [...live, ...queued]
      .slice(0, RUNNING_SHOWN)
      .map((c) => runningRow(c, tasks?.get(c.TaskID), runHref(c.ID), now))
    // eslint-disable-next-line react-hooks/exhaustive-deps -- `now` is stamped per data change, not a tick
  }, [running, tasks, runHref])

  const needsHidden = needsTotal != null ? Math.max(0, needsTotal - needsItems.length) : 0
  const runningHidden = running != null ? Math.max(0, running.total - runningItems.length) : 0

  // The day's spend ring: segments are shares of the model-attributed spend,
  // while the hole shows the day's whole figure — system overhead has no
  // model and belongs to the total alone.
  const segments = offline || !usage ? [] : donutSegments(usage.by_model ?? [])
  const legend = useMemo(() => {
    if (offline || !usage) return []
    const sorted = [...(usage.by_model ?? [])].sort((a, b) => b.cost - a.cost)
    return sorted.map((m) => ({
      key: m.model,
      name: catalog.models.find((c) => c.key === m.model)?.display_name ?? m.model,
      value: money(m.cost),
    }))
  }, [offline, usage, catalog.models])

  // Converge's `height` prop is a viewBox unit, not pixels: what renders is
  // width × (height / 740). So measure the pixel room, clamp THAT, and convert
  // back — passing the room straight in is how the footer lands below the
  // fold. Re-measured on resize, plus once after layout settles (the first
  // pass can run before the flex row has its final width).
  const convBox = useRef<HTMLDivElement | null>(null)
  const [convHeight, setConvHeight] = useState(190)
  const measure = useCallback(() => {
    const box = convBox.current
    if (!box || !box.clientWidth) return
    const room = box.clientHeight - 16
    const px = Math.max(140, Math.min(300, room))
    setConvHeight(Math.round((px * 740) / box.clientWidth))
  }, [])
  useEffect(() => {
    measure()
    const raf = requestAnimationFrame(() => requestAnimationFrame(measure))
    let ro: ResizeObserver | null = null
    if (typeof ResizeObserver === 'function' && convBox.current) {
      ro = new ResizeObserver(measure)
      ro.observe(convBox.current)
    }
    return () => {
      cancelAnimationFrame(raf)
      ro?.disconnect()
    }
  }, [measure])

  // Offline keeps the outcome names and zeroes their counts, so the fan stays
  // a shape with no claim in it rather than vanishing from the page.
  const outcomes = useMemo<ConvergeOutcome[]>(
    () => [
      { name: 'merged', v: offline ? 0 : (daySums?.merged ?? 0), tone: 'warm' },
      { name: 'running', v: offline ? 0 : (running?.total ?? 0), tone: 'cool' },
      { name: 'need you', v: offline || quiet ? 0 : (needsTotal ?? 0), tone: 'ask' },
      { name: 'filtered by rules', v: offline ? 0 : (daySums?.filtered ?? 0), tone: 'quiet' },
    ],
    [offline, quiet, daySums, running, needsTotal],
  )

  const boardNote = (hidden: number) => (
    <Link to={orgHref('/board')}>
      {hidden > 0 ? `+${hidden} more on the board` : 'open the board'}
    </Link>
  )

  return (
    <div className="ov">
      <div className="ov-away">
        <span className="ov-away-lead">{anchor === undefined ? ' ' : seenLead(anchor)}</span>
        <span className="ov-away-tail">
          {offline
            ? 'readouts are inert until the connection returns'
            : seenSums
              ? awayTail(seenSums)
              : ' '}
        </span>
      </div>

      <div className="ov-conv" ref={convBox}>
        <Converge
          kicker="SINCE MIDNIGHT"
          title={`${offline || !daySums ? '--' : daySums.events} events triaged`}
          outcomes={outcomes}
          strands={28}
          height={convHeight}
          replayOnClick
        />
      </div>

      <div className="ov-needs">
        <RunRows
          label="NEEDS YOU"
          count={num(quiet ? 0 : needsTotal)}
          countTone="warm"
          rows={quiet ? [] : needsItems}
          onPick={onPick}
          note={quiet || needs == null ? null : boardNote(needsHidden)}
          // Nothing needing you is an ANSWER, stated in words — not a band of
          // figures, which the rest of the page already carries.
          empty={quiet ? 'Nothing needs you.' : ' '}
        />
      </div>

      <div className="ov-live">
        <div className="ov-runcol">
          <RunRows
            label="RUNNING"
            count={num(running?.total)}
            countTone="cool"
            rows={runningItems}
            onPick={onPick}
            empty="Nothing running right now."
            note={runningHidden > 0 ? boardNote(runningHidden) : null}
          />
        </div>

        <div className="ov-donut">
          <div className="ov-donut-head">
            <span className="ov-donut-label">TODAY</span>
            <span className="ov-donut-sub">BY MODEL</span>
          </div>
          <div className="ov-donut-row">
            <div className="ov-ring">
              <svg viewBox="0 0 104 104" aria-hidden="true">
                <circle
                  cx="52"
                  cy="52"
                  r="40"
                  fill="none"
                  stroke="var(--color-tint-3)"
                  strokeWidth="13"
                />
                {segments.map((s, i) => (
                  <circle
                    key={i}
                    className="ov-ring-seg"
                    cx="52"
                    cy="52"
                    r="40"
                    fill="none"
                    stroke={s.color}
                    strokeWidth="13"
                    strokeDasharray={s.dash}
                    strokeDashoffset={s.offset}
                    style={{ animationDelay: s.delay }}
                  />
                ))}
              </svg>
              <div className="ov-hole">
                <span className="ov-hole-total">{cash(usage?.total_cost_usd)}</span>
                <span className="ov-hole-word">SPENT</span>
              </div>
            </div>
            <div className="ov-legend">
              {/* Index-aligned with the segments: both are the by_model cut
                  sorted largest first, so slice i's swatch is segment i's shade. */}
              {legend.map((m, i) => (
                <div key={m.key} className="ov-legend-row">
                  <span className="ov-legend-swatch" style={{ background: segments[i]?.color }} />
                  <span className="ov-legend-name">{m.name}</span>
                  <span className="ov-legend-value">{m.value}</span>
                </div>
              ))}
            </div>
          </div>
        </div>
      </div>
    </div>
  )
}
