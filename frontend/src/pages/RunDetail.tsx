import { useCallback, useEffect, useMemo, useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import type { Conversation } from '../types'
import { setPresenceView } from '../hooks/useWebSocket'
import { useRunDetail } from '../hooks/useRunDetail'
import { useOrgHref } from '../hooks/useOrgHref'
import { isActiveRun } from '../lib/runStatus'
import { approvalCounts, hasUnresolvedArtifacts, unresolvedArtifacts } from '../lib/approval'
import RunStation, { type StationActions } from '../components/runstation/RunStation'
import ReviewOverlay from '../components/ReviewOverlay'
import PendingPROverlay from '../components/PendingPROverlay'
import ApprovalList from '../components/ApprovalList'
import ResolveAllConfirm from '../components/ResolveAllConfirm'
import { GlassBackdrop } from './setup/glass'
import { toast } from '../components/Toast/toastStore'
import { readError } from '../lib/api'

// RunDetail — the data shell for the full-screen run station. It loads the run +
// task + messages (live over websocket via useRunDetail), wires the real
// actions (message/interrupt steering, cancel, requeue, review/PR approval),
// and hands it all to <RunStation>, which owns every pixel.
export default function RunDetail() {
  const { runID } = useParams<{ runID: string }>()
  const navigate = useNavigate()
  const orgHref = useOrgHref()
  const {
    run,
    task,
    messages,
    artifacts,
    loading,
    notFound,
    error,
    pendingPermissions,
    resolvePermission,
    softRefresh,
  } = useRunDetail(runID)

  const [chainSteps, setChainSteps] = useState<Conversation[] | null>(null)
  const [now, setNow] = useState(() => Date.now())
  const [interruptPending, setInterruptPending] = useState(false)
  // Approval is derived now, not a stored run status: the run's unresolved
  // artifact set (draft PRs + ready reviews) drives a *list* of approvable cards.
  // `activeArtifact` is the one per-item editor currently open (you edit one at a
  // time — ReviewOverlay / PendingPROverlay key off a single artifact id);
  // `listOpen` is the roster panel; `confirmRequeueOpen` gates the destructive
  // resolve-all on Return-to-queue.
  const [activeArtifact, setActiveArtifact] = useState<{
    kind: 'review' | 'pr'
    id: string
  } | null>(null)
  const [listOpen, setListOpen] = useState(false)
  const [confirmRequeueOpen, setConfirmRequeueOpen] = useState(false)
  const [requeueBusy, setRequeueBusy] = useState(false)

  // The unresolved approvable subset — every draft PR + ready review on this run,
  // joined from the live artifact set and the run projection's authoritative id
  // list (the ready-review predicate lives server-side, so artifact.state alone
  // can't reconstruct it).
  const unresolved = useMemo(
    () => unresolvedArtifacts(artifacts, run?.pending_artifact_ids ?? []),
    [artifacts, run?.pending_artifact_ids],
  )

  // When the last item resolves (here or in another tab — the hook re-pulls the
  // set on softRefresh / artifact_updated), close the roster: there's nothing
  // left to approve, and the card re-derives to in-progress (live) / done
  // (terminal) on its own. No "mark done vs keep open" prompt.
  //
  // Gate on the run's authoritative derived predicate, NOT unresolved.length:
  // the intersection can be transiently empty (the roster opened before
  // /artifacts loaded, or a fetch race) while approvals genuinely remain —
  // closing on that would yank the UI out from under the user.
  useEffect(() => {
    if (listOpen && !hasUnresolvedArtifacts(run)) setListOpen(false)
  }, [listOpen, run])

  // Presence (TFAC-392): this run's detail page is an answer-capable surface for
  // ITS run's permission prompts. Report run:<id> while mounted (re-firing if the
  // runID changes) and fall back to 'other' on unmount.
  useEffect(() => {
    if (!runID) return
    setPresenceView(`run:${runID}`)
    return () => setPresenceView('other')
  }, [runID])

  // Tick while live so elapsed + the vent-heat flare stay current.
  useEffect(() => {
    if (!run || !isActiveRun(run)) return
    const id = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(id)
  }, [run])

  // Load blueprint chain steps, padding not-yet-spawned steps with synthetic
  // placeholders so the chain track renders its full length.
  useEffect(() => {
    if (!run?.blueprint_run_id) {
      setChainSteps(null)
      return
    }
    let cancelled = false
    fetch(`/api/blueprint-runs/${run.blueprint_run_id}`)
      .then((r) => (r.ok ? r.json() : null))
      .then(
        (
          data: {
            steps?: Array<{ step: { step_index: number }; run?: Conversation | null }>
          } | null,
        ) => {
          if (cancelled || !data?.steps) return
          const padded: Conversation[] = data.steps.map((s, i) => {
            if (s.run) return s.run
            return {
              ID: `__pending-${run.blueprint_run_id}-${i}`,
              TaskID: run.TaskID,
              Status: 'pending',
              Model: '',
              StartedAt: '',
              ResultSummary: '',
              blueprint_run_id: run.blueprint_run_id,
              blueprint_step_index: i,
            } as unknown as Conversation
          })
          setChainSteps(padded)
        },
      )
      .catch((err) => console.warn('Failed to load blueprint chain steps:', err))
    return () => {
      cancelled = true
    }
  }, [run?.blueprint_run_id, run?.TaskID])

  const handleCancel = useCallback(async () => {
    if (!run) return
    try {
      const res = await fetch(`/api/agent/conversations/${run.ID}/cancel`, { method: 'POST' })
      if (!res.ok) toast.error(await readError(res, 'Failed to cancel run'))
    } catch (err) {
      toast.error(`Failed to cancel run: ${(err as Error).message}`)
    }
  }, [run])

  // Steer a run: a free-form message lands on the live process (or wakes an
  // `open` run via resume). The backend records + broadcasts it as an
  // `message` event, so useRunDetail's append renders it — no optimistic insert.
  const handleMessage = useCallback(
    async (text: string) => {
      if (!run) return
      try {
        const res = await fetch(`/api/agent/conversations/${run.ID}/message`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ text }),
        })
        if (!res.ok) toast.error(await readError(res, 'Failed to send message'))
      } catch (err) {
        toast.error(`Failed to send message: ${(err as Error).message}`)
      }
    },
    [run],
  )

  // Interrupt pauses the current turn (run → open), leaving the process warm —
  // distinct from Cancel, which abandons the run. The composer stays open.
  // interruptPending disables the Pause button while the POST is in flight so
  // rapid clicks can't stack concurrent interrupts ahead of the WS status flip.
  const handleInterrupt = useCallback(async () => {
    if (!run || interruptPending) return
    setInterruptPending(true)
    try {
      const res = await fetch(`/api/agent/conversations/${run.ID}/interrupt`, { method: 'POST' })
      if (!res.ok) toast.error(await readError(res, 'Failed to interrupt run'))
    } catch (err) {
      toast.error(`Failed to interrupt run: ${(err as Error).message}`)
    } finally {
      setInterruptPending(false)
    }
  }, [run, interruptPending])

  // doRequeue fires the actual requeue. Return-to-queue is a task-level
  // force-resolve-all: the backend tears down every unresolved artifact (closes
  // draft PRs, discards pending reviews — branches kept) and cancels a live run.
  const doRequeue = useCallback(async () => {
    if (!run?.TaskID) return
    setRequeueBusy(true)
    try {
      const res = await fetch(`/api/tasks/${run.TaskID}/requeue`, { method: 'POST' })
      if (!res.ok) {
        toast.error(await readError(res, 'Failed to return to queue'))
        return
      }
      navigate(orgHref('/board'))
    } catch (err) {
      toast.error(`Failed to return to queue: ${(err as Error).message}`)
    } finally {
      setRequeueBusy(false)
    }
  }, [navigate, orgHref, run?.TaskID])

  // handleRequeue gates the destructive teardown behind the confirmation modal
  // whenever the run still has unresolved artifacts; otherwise it requeues
  // straight away (nothing to resolve).
  const handleRequeue = useCallback(() => {
    if (!run) return
    if (hasUnresolvedArtifacts(run)) {
      setConfirmRequeueOpen(true)
      return
    }
    void doRequeue()
  }, [run, doRequeue])

  // The dock's "Review N items →" affordance opens the approval roster (the list
  // of every unresolved draft PR / ready review), not a single overlay.
  const handleReview = useCallback(() => setListOpen(true), [])

  // Open one artifact's editor overlay by id — from a roster card, or from the
  // rail's Artifacts list. The per-item editors (ReviewOverlay / PendingPROverlay)
  // each key off a single artifact id, so they're addressed one at a time.
  const handleOpenArtifact = useCallback((kind: 'review' | 'pr', artifactId: string) => {
    setActiveArtifact({ kind, id: artifactId })
  }, [])

  // Keyboard: Esc → back.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.target instanceof HTMLElement) {
        const tag = e.target.tagName
        if (tag === 'INPUT' || tag === 'TEXTAREA' || e.target.isContentEditable) return
      }
      if (e.key === 'Escape') {
        e.preventDefault()
        navigate(orgHref('/board'))
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [navigate, orgHref])

  if (loading)
    return <FloorMessage tone="text-text-tertiary">Spinning up the station…</FloorMessage>

  // Order matters: a 5xx leaves `run` null AND sets `error`; checking notFound
  // first would mask the real failure behind a misleading "not found".
  if (error)
    return (
      <FloorMessage tone="text-dismiss" back={orgHref('/board')}>
        Failed to load: {error}
      </FloorMessage>
    )
  if (notFound || !run)
    return (
      <FloorMessage tone="text-text-tertiary" back={orgHref('/board')}>
        Run not found.
      </FloorMessage>
    )

  const actions: StationActions = {
    onBack: () => navigate(orgHref('/board')),
    onCancel: handleCancel,
    onRequeue: handleRequeue,
    onReview: handleReview,
    onOpenArtifact: handleOpenArtifact,
    onMessage: handleMessage,
    onInterrupt: handleInterrupt,
    interruptPending,
    onResolvePermission: resolvePermission,
  }

  const counts = approvalCounts(run)

  return (
    <div className="relative h-screen p-3">
      <GlassBackdrop />

      {/* The approval roster — one card per unresolved draft PR / ready review,
          each editable/approvable by id and dismissable in place. */}
      <ApprovalList
        open={listOpen}
        onClose={() => setListOpen(false)}
        items={unresolved}
        pendingExpected={hasUnresolvedArtifacts(run)}
        onOpenItem={handleOpenArtifact}
        onDismissed={softRefresh}
      />

      {/* Per-item editors — one open at a time, addressed by artifact id. A
          softRefresh on close re-derives the set in place (no spinner flash). */}
      <ReviewOverlay
        artifactId={activeArtifact?.kind === 'review' ? activeArtifact.id : ''}
        open={activeArtifact?.kind === 'review'}
        onClose={() => {
          setActiveArtifact(null)
          softRefresh()
        }}
      />
      <PendingPROverlay
        artifactId={activeArtifact?.kind === 'pr' ? activeArtifact.id : ''}
        open={activeArtifact?.kind === 'pr'}
        onClose={() => {
          setActiveArtifact(null)
          softRefresh()
        }}
      />

      {/* Resolve-all confirmation — the second gesture (Return to queue). */}
      <ResolveAllConfirm
        open={confirmRequeueOpen}
        prCount={counts.pr}
        reviewCount={counts.review}
        isLive={isActiveRun(run)}
        actionLabel="Return to queue"
        busy={requeueBusy}
        onConfirm={() => void doRequeue()}
        onCancel={() => setConfirmRequeueOpen(false)}
      />

      <RunStation
        run={run}
        task={task}
        messages={messages}
        now={now}
        chainSteps={chainSteps}
        actions={actions}
        pendingPermissions={pendingPermissions}
      />
    </div>
  )
}

// FloorMessage — loading / error / empty states, lit against the empty floor.
function FloorMessage({
  children,
  tone,
  back,
}: {
  children: React.ReactNode
  tone: string
  back?: string
}) {
  return (
    <div className="relative">
      <GlassBackdrop />
      <div className="flex min-h-screen flex-col items-center justify-center gap-3">
        <p className={`font-mono text-[12px] tracking-wide ${tone}`}>{children}</p>
        {back && (
          <Link
            to={back}
            className="font-mono text-[11px] uppercase tracking-[0.12em] text-accent hover:underline"
          >
            ← back to floor
          </Link>
        )}
      </div>
    </div>
  )
}
