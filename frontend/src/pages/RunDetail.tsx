import { useCallback, useEffect, useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import type { AgentRun } from '../types'
import { useRunDetail } from '../hooks/useRunDetail'
import { useOrgHref } from '../hooks/useOrgHref'
import { isActiveRun } from '../lib/runStatus'
import RunStation, { type StationActions } from '../components/runstation/RunStation'
import ReviewOverlay from '../components/ReviewOverlay'
import PendingPROverlay from '../components/PendingPROverlay'
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
    loading,
    notFound,
    error,
    refetch,
    pendingPermissions,
    resolvePermission,
  } = useRunDetail(runID)

  const [chainSteps, setChainSteps] = useState<AgentRun[] | null>(null)
  const [now, setNow] = useState(() => Date.now())
  const [interruptPending, setInterruptPending] = useState(false)
  const [approval, setApproval] = useState<{ runID: string; kind: 'review' | 'pr' } | null>(null)

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
          data: { steps?: Array<{ step: { step_index: number }; run?: AgentRun | null }> } | null,
        ) => {
          if (cancelled || !data?.steps) return
          const padded: AgentRun[] = data.steps.map((s, i) => {
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
            } as unknown as AgentRun
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
      const res = await fetch(`/api/agent/runs/${run.ID}/cancel`, { method: 'POST' })
      if (!res.ok) toast.error(await readError(res, 'Failed to cancel run'))
    } catch (err) {
      toast.error(`Failed to cancel run: ${(err as Error).message}`)
    }
  }, [run])

  // Steer a run: a free-form message lands on the live process (or wakes an
  // `open` run via resume). The backend records + broadcasts it as an
  // agent_message, so useRunDetail's append renders it — no optimistic insert.
  const handleMessage = useCallback(
    async (text: string) => {
      if (!run) return
      try {
        const res = await fetch(`/api/agent/runs/${run.ID}/message`, {
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
      const res = await fetch(`/api/agent/runs/${run.ID}/interrupt`, { method: 'POST' })
      if (!res.ok) toast.error(await readError(res, 'Failed to interrupt run'))
    } catch (err) {
      toast.error(`Failed to interrupt run: ${(err as Error).message}`)
    } finally {
      setInterruptPending(false)
    }
  }, [run, interruptPending])

  const handleRequeue = useCallback(async () => {
    if (!run?.TaskID) return
    try {
      const res = await fetch(`/api/tasks/${run.TaskID}/requeue`, { method: 'POST' })
      if (!res.ok) {
        toast.error(await readError(res, 'Failed to return to queue'))
        return
      }
      navigate(orgHref('/board'))
    } catch (err) {
      toast.error(`Failed to return to queue: ${(err as Error).message}`)
    }
  }, [navigate, orgHref, run?.TaskID])

  const handleReview = useCallback(() => {
    if (!run) return
    setApproval({ runID: run.ID, kind: run.pending_kind === 'pr' ? 'pr' : 'review' })
  }, [run])

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
    onMessage: handleMessage,
    onInterrupt: handleInterrupt,
    interruptPending,
    onResolvePermission: resolvePermission,
  }

  return (
    <div className="relative h-screen p-3">
      <GlassBackdrop />
      <ReviewOverlay
        runID={approval?.kind === 'review' ? approval.runID : ''}
        open={approval?.kind === 'review'}
        onClose={() => {
          setApproval(null)
          refetch()
        }}
      />
      <PendingPROverlay
        runID={approval?.kind === 'pr' ? approval.runID : ''}
        open={approval?.kind === 'pr'}
        onClose={() => {
          setApproval(null)
          refetch()
        }}
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
