import { useCallback, useEffect, useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router'
import type { BlueprintStep, Conversation } from '../types'
import { setPresenceView } from '../hooks/useWebSocket'
import { useConversationDetail } from '../hooks/useConversationDetail'
import { useOrgHref } from '../hooks/useOrgHref'
import { isActiveConversation } from '../lib/conversationStatus'
import RunStation, {
  type ChainStepLabel,
  type StationActions,
} from '../components/runstation/RunStation'
import ReviewOverlay from '../components/ReviewOverlay'
import PendingPROverlay from '../components/PendingPROverlay'
import { GlassBackdrop } from './setup/glass'
import { toast } from '../components/Toast/toastStore'
import { apiFetch, apiJSON, httpErrorMessage } from '../lib/apiClient'

// RunDetail — the data shell for the full-screen run station. It loads the run +
// task + messages (live over websocket via useConversationDetail), wires the real
// actions (message/interrupt steering, cancel, requeue, review/PR approval),
// and hands it all to <RunStation>, which owns every pixel.
export default function RunDetail() {
  const { conversationID } = useParams<{ conversationID: string }>()
  const navigate = useNavigate()
  const orgHref = useOrgHref()
  const {
    conversation,
    task,
    messages,
    loading,
    notFound,
    error,
    pendingPermissions,
    resolvePermission,
    softRefresh,
    hasOlderMessages,
    loadingOlderMessages,
    loadOlderMessages,
  } = useConversationDetail(conversationID)

  const [chainSteps, setChainSteps] = useState<Conversation[] | null>(null)
  // Per-step labels for the chain track, index-aligned with chainSteps. Kept
  // beside them rather than folded in because the track's segments are
  // conversations, and a not-yet-spawned step has a name but no conversation.
  const [chainStepLabels, setChainStepLabels] = useState<ChainStepLabel[] | null>(null)
  const [now, setNow] = useState(() => Date.now())
  const [stopPending, setStopPending] = useState(false)
  // Approval is derived now, not a stored conversation status: the
  // conversation's unresolved artifact set (draft PRs + ready reviews) surfaces
  // at the top of the station's artifact lists (the dock popover + the
  // telemetry rail). `activeArtifact` is the one per-item editor currently open
  // (you edit one at a time — ReviewOverlay / PendingPROverlay key off a single
  // artifact id).
  const [activeArtifact, setActiveArtifact] = useState<{
    kind: 'review' | 'pr'
    id: string
  } | null>(null)

  // Presence (TFAC-392): this run's detail page is an answer-capable surface for
  // ITS run's permission prompts. Report run:<id> while mounted (re-firing if the
  // conversationID changes) and fall back to 'other' on unmount.
  useEffect(() => {
    if (!conversationID) return
    setPresenceView(`run:${conversationID}`)
    return () => setPresenceView('other')
  }, [conversationID])

  // Tick while live so elapsed + the vent-heat flare stay current.
  useEffect(() => {
    if (!conversation || !isActiveConversation(conversation)) return
    const id = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(id)
  }, [conversation])

  // Load blueprint chain steps, padding not-yet-spawned steps with synthetic
  // placeholders so the chain track renders its full length.
  useEffect(() => {
    if (!conversation?.blueprint_run_id) {
      setChainSteps(null)
      setChainStepLabels(null)
      return
    }
    let cancelled = false
    apiJSON<{ steps?: Array<{ step: BlueprintStep; run?: Conversation | null }> }>(
      `/api/blueprint-runs/${conversation.blueprint_run_id}`,
    )
      .then((data) => {
        if (cancelled || !data?.steps) return
        setChainStepLabels(
          data.steps.map((s) => ({ name: s.step?.name ?? '', brief: s.step?.brief ?? '' })),
        )
        const padded: Conversation[] = data.steps.map((s, i) => {
          if (s.run) return s.run
          // Synthetic row for a step that hasn't been spawned; its status is
          // empty because it has no conversation, not a name outside the
          // vocabulary.
          return {
            ID: `__pending-${conversation.blueprint_run_id}-${i}`,
            TaskID: conversation.TaskID,
            Status: '',
            Model: '',
            StartedAt: '',
            ResultSummary: '',
            blueprint_run_id: conversation.blueprint_run_id,
            blueprint_step_index: i,
          } as unknown as Conversation
        })
        setChainSteps(padded)
      })
      .catch((err) => console.warn('Failed to load blueprint chain steps:', err))
    return () => {
      cancelled = true
    }
  }, [conversation?.blueprint_run_id, conversation?.TaskID])

  // Stop the conversation: the agent stops, it parks `open`, and its
  // blueprint and task stay exactly where they were — so the composer's offer
  // to pick the work back up is true. stopPending disables the controls while
  // the POST is in flight so rapid clicks can't stack requests ahead of the WS
  // status flip.
  const handleStop = useCallback(async () => {
    if (!conversation || stopPending) return
    setStopPending(true)
    try {
      await apiFetch(`/api/agent/conversations/${conversation.ID}/stop`, { method: 'POST' })
    } catch (err) {
      toast.error(httpErrorMessage(err, 'Could not stop the run.'))
    } finally {
      setStopPending(false)
    }
  }, [conversation, stopPending])

  // Steer a conversation: a free-form message lands on the live process (or
  // wakes an `open` conversation via resume). The backend records +
  // broadcasts it as a `message` event, so useConversationDetail's append
  // renders it — no optimistic insert.
  const handleMessage = useCallback(
    async (text: string) => {
      if (!conversation) return
      try {
        await apiFetch(`/api/agent/conversations/${conversation.ID}/message`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ text }),
        })
      } catch (err) {
        toast.error(httpErrorMessage(err, 'Could not send the message.'))
      }
    },
    [conversation],
  )

  // handleRequeue returns the task to the queue. It cancels a live conversation
  // and nothing else: the draft PRs and staged reviews the run produced go back
  // to the pool with the task, for whoever picks it up next to finish or
  // dismiss. So it fires straight from the button — there is nothing
  // destructive to confirm.
  const handleRequeue = useCallback(async () => {
    if (!conversation?.TaskID) return
    try {
      await apiFetch(`/api/tasks/${conversation.TaskID}/requeue`, { method: 'POST' })
      navigate(orgHref('/board'))
    } catch (err) {
      toast.error(httpErrorMessage(err, 'Could not return the task to the queue.'))
    }
  }, [navigate, orgHref, conversation?.TaskID])

  // Open one artifact's editor overlay by id — from the dock's approval
  // popover, or from the rail's Artifacts list. The per-item editors
  // (ReviewOverlay / PendingPROverlay) each key off a single artifact id, so
  // they're addressed one at a time.
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

  if (loading) return <FloorMessage tone="text-ink-3">Spinning up the station…</FloorMessage>

  // Order matters: a 5xx leaves `conversation` null AND sets `error`; checking notFound
  // first would mask the real failure behind a misleading "not found".
  if (error)
    return (
      <FloorMessage tone="text-alarm" back={orgHref('/board')}>
        Failed to load: {error}
      </FloorMessage>
    )
  if (notFound || !conversation)
    return (
      <FloorMessage tone="text-ink-3" back={orgHref('/board')}>
        Run not found.
      </FloorMessage>
    )

  const actions: StationActions = {
    onBack: () => navigate(orgHref('/board')),
    onCancel: handleStop,
    onRequeue: handleRequeue,
    onOpenArtifact: handleOpenArtifact,
    onArtifactResolved: softRefresh,
    onMessage: handleMessage,
    onInterrupt: handleStop,
    stopPending,
    onResolvePermission: resolvePermission,
  }

  return (
    <div className="relative h-screen p-3">
      <GlassBackdrop />

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

      <RunStation
        conversation={conversation}
        task={task}
        messages={messages}
        now={now}
        chainSteps={chainSteps}
        chainStepLabels={chainStepLabels}
        actions={actions}
        pendingPermissions={pendingPermissions}
        hasOlderMessages={hasOlderMessages}
        loadingOlderMessages={loadingOlderMessages}
        onLoadOlderMessages={loadOlderMessages}
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
        <p className={`font-mono text-ui tracking-wide ${tone}`}>{children}</p>
        {back && (
          <Link
            to={back}
            className="font-mono text-reported uppercase tracking-[0.12em] text-warm hover:underline"
          >
            ← back to floor
          </Link>
        )}
      </div>
    </div>
  )
}
