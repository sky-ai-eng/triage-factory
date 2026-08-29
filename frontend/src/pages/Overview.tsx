import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Link, useNavigate } from 'react-router'
import { RunRows } from '../ui/runrow/RunRows'
import type { RunRowItem } from '../ui/runrow/RunRows'
import { Converge } from '../ui/converge/Converge'
import type { ConvergeOutcome } from '../ui/converge/Converge'
import { FlapCount } from '../ui/flapcount/FlapCount'
import { CratePile } from '../ui/cratepile/CratePile'
import { SpendRing } from '../ui/spendring/SpendRing'
import { needsRow, ringModels, runningRow, utcMidnightISO, windowSums } from './overview/data'
import {
  useActivityWindow,
  useClock,
  useConversationSets,
  useOpenPRCount,
  useOverviewTick,
  useSpendToday,
  useTasksIndex,
} from './overview/hooks'
import { useShellScope } from '../hooks/useShellScope'
import { useOrgHref } from '../hooks/useOrgHref'
import { useWsConnected } from '../hooks/useWebSocket'
import { ACTIVE_STATUSES, workStartedAt } from '../lib/conversationStatus'
import type { Conversation } from '../types'
import './overview/overview.css'

// Overview — the first row in WORK, and the page nobody lands on: you land on
// your last page, not here. So it is not a dashboard you monitor. It is the
// page you open deliberately, once, to find out what happened and what is
// waiting for you.
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
// never a usage pattern or a stored preference. And NOTHING TICKS except the
// masthead's clock: figures move when data moves, ages are stamped per
// refetch, and the sweep is emission, not a tick.
//
// The layout is the pinwheel: the convergence leads with the masthead in its
// top-right corner, then the two graphics sit in opposite corners of the two
// bands — the pile leads NEEDS YOU, the ring closes RUNNING. Each band is a
// wrapping flex row, so below roughly 800px of content width the graphic
// drops onto its own line with no breakpoint owning that; NEEDS YOU wraps in
// reverse so its rows land ABOVE the pile and stay the first thing read.

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
  const { clock, date } = useClock()
  // Midnight is stamped once per mount: the convergence's window opens where
  // the day did, and a page that recomputed it per render would tick.
  const midnight = useMemo(() => utcMidnightISO(), [])

  const { needs, running } = useConversationSets(teamId, tick)
  const tasks = useTasksIndex(teamId, tick)
  const sinceMidnight = useActivityWindow(teamId, midnight, tick)
  const usage = useSpendToday(teamId, tick)
  const openPRs = useOpenPRCount(teamId, tick)

  const daySums = sinceMidnight ? windowSums(sinceMidnight) : null

  // Offline is not a loading state: a readout goes inert rather than holding
  // the last number it happened to have — the same rule the rail applies to
  // itself. Unknown is not zero either, so a read that has not answered is a
  // dash too.
  const num = (v: number | null | undefined) => (offline || v == null ? '--' : String(v))

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

  // The board link under a list: what it excludes when it excludes something,
  // the way in when it does not. The quiet state drops it — "open the board"
  // under "Nothing needs you." would be an errand with no reason attached.
  const boardMore = (hidden: number) => (
    <Link to={orgHref('/board')}>
      {hidden > 0 ? `+${hidden} more on the board` : 'open the board'}
    </Link>
  )

  // The ring is the model-attributed spend; 'forbidden' is the grant that
  // disagrees, and then the ring goes entirely — the rows take the full
  // width, one fewer answer rather than a second layout.
  const spendGone = usage === 'forbidden'
  const spendModels = useMemo(
    () => (usage == null || usage === 'forbidden' ? [] : ringModels(usage.by_model ?? [])),
    [usage],
  )

  // Converge runs in fill mode — the container's flex height drives the
  // drawing, so width and height resolve in one layout pass and a rail toggle
  // reshapes the fan continuously. The measurement survives for one coarse
  // job: the endpoint band, so the masthead's roughly constant ~104px of
  // clearance stays a sensible fraction of a plot whose height varies. A band
  // that lands a frame late shifts a lane by a pixel and nobody sees it; a
  // HEIGHT that landed a frame late was a visible snap.
  const convBox = useRef<HTMLDivElement | null>(null)
  const [convPx, setConvPx] = useState(300)
  const measure = useCallback(() => {
    const box = convBox.current
    if (!box || !box.clientHeight) return
    setConvPx(box.clientHeight - 16)
  }, [])
  useEffect(() => {
    measure()
    let ro: ResizeObserver | null = null
    if (typeof ResizeObserver === 'function' && convBox.current) {
      ro = new ResizeObserver(measure)
      ro.observe(convBox.current)
    }
    return () => {
      ro?.disconnect()
    }
  }, [measure])
  // The band compresses the lanes rather than translating them (the last lane
  // already sits near the plot floor), and it floors at 0.14 — a window short
  // enough to need less clearance has no room for a large masthead anyway.
  const convBand = useMemo<[number, number]>(
    () => [Math.max(0.14, Math.min(0.42, 104 / (convPx || 300))), 1],
    [convPx],
  )

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
  const events = offline || !daySums ? null : daySums.events

  return (
    <div className="ov">
      <div className="ov-col">
        {/* The away line is gone — it stated a timestamp nothing else on the
            page was measured against. Its slot survives for the one thing
            that belongs at the top: the offline condition, stated once,
            because it qualifies every readout below it. */}
        {offline ? (
          <div className="ov-away">
            <span className="ov-away-tail">readouts are inert until the connection returns</span>
          </div>
        ) : null}

        <div className="ov-conv" ref={convBox}>
          <div className="ov-mast">
            <span className="ov-mast-name">Triage Factory</span>
            <span className="ov-mast-line">
              <span className="ov-mast-clock">{clock}</span>
              <span className="ov-mast-date">{date}</span>
            </span>
          </div>
          <Converge
            kicker="SINCE MIDNIGHT"
            title="events triaged"
            titleNode={
              <FlapCount
                value={events}
                size={24}
                label={(events == null ? 'no' : events) + ' events triaged'}
              />
            }
            outcomes={outcomes}
            strands={28}
            height={260}
            endpointBand={convBand}
            fill
            replayOnClick
          />
        </div>

        <div className="ov-needsband">
          <div className="ov-pile">
            <CratePile
              count={openPRs ?? 0}
              // Unknown is not zero: an unanswered backlog reads as the inert
              // pallet, never as an empty one.
              offline={offline || openPRs == null}
              caption="open pull requests"
              captionOne="open pull request"
              href={orgHref('/prs')}
              onOpen={() => navigate(orgHref('/prs'))}
            />
          </div>
          <div className="ov-rows">
            <RunRows
              label="NEEDS YOU"
              count={
                <span style={{ color: 'var(--color-warm)' }}>{num(quiet ? 0 : needsTotal)}</span>
              }
              rows={quiet ? [] : needsItems}
              onPick={onPick}
              more={quiet || needs == null ? null : boardMore(needsHidden)}
              // Nothing needing you is an ANSWER, stated in words — not a band
              // of figures, which the rest of the page already carries.
              empty={quiet ? 'Nothing needs you.' : ' '}
            />
          </div>
        </div>

        <div className="ov-liveband">
          <div className="ov-rows">
            <RunRows
              label="RUNNING"
              count={<span style={{ color: 'var(--color-cool)' }}>{num(running?.total)}</span>}
              rows={runningItems}
              onPick={onPick}
              empty="Nothing running right now."
              more={runningHidden > 0 ? boardMore(runningHidden) : null}
            />
          </div>
          {spendGone ? null : (
            <div className="ov-ringbox">
              <SpendRing
                models={spendModels}
                // Unanswered is a dash, not a zero-spend claim.
                offline={offline || usage == null}
                href={orgHref('/usage')}
                onOpen={() => navigate(orgHref('/usage'))}
              />
            </div>
          )}
        </div>
      </div>
    </div>
  )
}
