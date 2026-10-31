import { useCallback, useEffect, useMemo, useState } from 'react'
import { ChevronRight } from 'lucide-react'
import type {
  ActivityAction,
  Conversation,
  ConversationArtifact,
  Deliverable,
  WSEvent,
} from '../../types'
import { apiList, apiListAll, httpErrorMessage } from '../../lib/apiClient'
import { artifactSetKey } from '../../lib/approval'
import { dismissDeliverable } from '../../lib/deliverables'
import { useWebSocket } from '../../hooks/useWebSocket'
import { toast } from '../Toast/toastStore'
import { DeliverableRow, DismissDeliverableButton, StateBadge } from '../DeliverableRow'
import {
  awaitsDecision,
  metaForKind,
  metaForObjectKind,
  overlayKindFor,
  rowTone,
} from '../deliverableMeta'
import { metaForAction } from '../actionMeta'
import ActionList from '../ActionList'
import { RailSection } from './RailSection'

// RunArtifacts — the Run page rail's Artifacts section: what this
// conversation changed, with the work waiting on a person marked.
//
// Three reads, in this order on screen:
//  1. Deliverables no artifact names — a staged review, which is a
//     deliverable before it is posted and so has no action behind it yet.
//  2. The artifacts, in the route's order. One whose deliverable is set is the
//     same object as that deliverable (a draft PR the agent opened) and
//     carries its controls.
//  3. A count of the refused and failed writes, which changed no object.
//
// The section is absent until one of the three has something in it.
interface Props {
  conversation: Conversation
  onOpenDeliverable?: (kind: 'review' | 'pr', deliverableId: string) => void
  /** Called after a successful dismiss so the owner re-pulls the conversation
   *  projection. The section refetches its own rows regardless. */
  onDeliverableResolved?: () => void
}

interface ArtifactSet {
  conversationId: string
  /** The pull requests and reviews no artifact names. */
  deliverables: Deliverable[]
  artifacts: ConversationArtifact[]
  refused: number
}

// loadArtifactSet reads the section's three sources. Both lists are read
// whole: which deliverable an artifact names decides which deliverables show,
// and the one it names is as likely to sit on the last page as the first.
async function loadArtifactSet(conversationId: string): Promise<ArtifactSet> {
  const base = `/api/agent/conversations/${encodeURIComponent(conversationId)}`
  const [deliverables, artifacts, refused] = await Promise.all([
    apiListAll<Deliverable>(`${base}/deliverables/list`),
    apiListAll<ConversationArtifact>(`${base}/artifacts/list`),
    apiList<ActivityAction>(`${base}/actions/list`, { refused: true, page_size: 0 }),
  ])
  const named = new Set(artifacts.flatMap((a) => (a.deliverable ? [a.deliverable.id] : [])))
  return {
    conversationId,
    // Only a pull request or a review is work a person acts on. A deliverable
    // row of any other kind records an object the artifacts already list.
    deliverables: deliverables.filter((d) => overlayKindFor(d.kind) !== null && !named.has(d.id)),
    artifacts,
    refused: refused.total_count ?? 0,
  }
}

export function RunArtifacts({ conversation, onOpenDeliverable, onDeliverableResolved }: Props) {
  const conversationId = conversation.ID
  const refreshKey = artifactSetKey(conversation)
  const [tick, setTick] = useState(0)
  const [set, setSet] = useState<ArtifactSet | null>(null)
  const [failure, setFailure] = useState<{ conversationId: string; message: string } | null>(null)
  // Per-id in-flight guard so a row's [x] disables only itself while its
  // write is in flight, and a double click can't fire two.
  const [dismissing, setDismissing] = useState<Record<string, boolean>>({})

  // A deliverable of this run changing state, or a person resolving one
  // anywhere in the org, can change what the section shows without moving
  // the conversation row's key.
  useWebSocket(
    useCallback(
      (event: WSEvent) => {
        if (
          event.type === 'conversations_updated' ||
          (event.type === 'deliverable_updated' && event.conversation_id === conversationId)
        ) {
          setTick((t) => t + 1)
        }
      },
      [conversationId],
    ),
  )

  // A refetch keeps the rows on screen until the new set lands, and a failed
  // one keeps them too: they were true a moment ago. Rows and failures carry
  // the conversation they belong to, so a navigation never shows the previous
  // run's.
  useEffect(() => {
    if (!conversationId) return
    let cancelled = false
    loadArtifactSet(conversationId).then(
      (next) => {
        if (cancelled) return
        setSet(next)
        setFailure(null)
      },
      (err: unknown) => {
        if (cancelled) return
        setFailure({
          conversationId,
          message: httpErrorMessage(err, "Couldn't load this run's artifacts."),
        })
      },
    )
    return () => {
      cancelled = true
    }
  }, [conversationId, refreshKey, tick])

  const pending = useMemo(
    () => new Set(conversation.pending_deliverable_ids ?? []),
    [conversation.pending_deliverable_ids],
  )

  const dismiss = async (id: string, kind: string) => {
    if (dismissing[id]) return
    setDismissing((m) => ({ ...m, [id]: true }))
    try {
      await dismissDeliverable(id, kind)
      setTick((t) => t + 1)
      onDeliverableResolved?.()
    } catch (err) {
      toast.error(httpErrorMessage(err, 'Could not dismiss the deliverable.'))
    } finally {
      setDismissing((m) => {
        const next = { ...m }
        delete next[id]
        return next
      })
    }
  }

  const shown = set?.conversationId === conversationId ? set : null
  if (!shown) {
    if (!failure || failure.conversationId !== conversationId) return null
    return (
      <RailSection label="Artifacts">
        <p className="text-secondary leading-relaxed text-alarm">{failure.message}</p>
      </RailSection>
    )
  }
  if (shown.deliverables.length === 0 && shown.artifacts.length === 0 && shown.refused === 0) {
    return null
  }

  return (
    <RailSection label="Artifacts">
      <ul className="space-y-1.5">
        {shown.deliverables.map((d) => (
          <DeliverableRow
            key={d.id}
            deliverable={d}
            highlighted
            onOpenApproval={onOpenDeliverable}
            onDismiss={pending.has(d.id) ? () => void dismiss(d.id, d.kind) : undefined}
            dismissing={!!dismissing[d.id]}
          />
        ))}
        {shown.artifacts.map((a) => {
          const d = a.deliverable
          return (
            <ArtifactRow
              key={a.object_id}
              conversationId={conversationId}
              artifact={a}
              onOpenDeliverable={onOpenDeliverable}
              onDismiss={d && pending.has(d.id) ? () => void dismiss(d.id, d.kind) : undefined}
              dismissing={!!(d && dismissing[d.id])}
            />
          )
        })}
      </ul>
      {shown.refused > 0 && <RefusedLine conversationId={conversationId} count={shown.refused} />}
    </RailSection>
  )
}

// ArtifactRow — one object the run changed: its kind, its target linked out,
// the last thing the run did to it, and how many times it acted on it.
// Expanding it lists those actions.
function ArtifactRow({
  conversationId,
  artifact,
  onOpenDeliverable,
  onDismiss,
  dismissing,
}: {
  conversationId: string
  artifact: ConversationArtifact
  onOpenDeliverable?: (kind: 'review' | 'pr', deliverableId: string) => void
  onDismiss?: () => void
  dismissing: boolean
}) {
  const [expanded, setExpanded] = useState(false)
  const meta = metaForObjectKind(artifact.object_kind)
  const Icon = meta.icon
  const target = artifact.target || meta.label
  const deliverable = artifact.deliverable
  const overlayKind = deliverable ? overlayKindFor(deliverable.kind) : null
  const decision = deliverable != null && awaitsDecision(deliverable.kind, deliverable.state)

  return (
    <li>
      <div
        className={`flex items-start gap-1.5 rounded-[4px] border px-2 py-1.5 transition-colors ${rowTone(deliverable != null)}`}
      >
        <button
          type="button"
          onClick={() => setExpanded((v) => !v)}
          aria-expanded={expanded}
          aria-label={`${expanded ? 'Hide' : 'Show'} the actions on ${target}`}
          className="mt-0.5 shrink-0 rounded-[3px] text-ink-3/70 transition-colors hover:text-ink-2"
        >
          <ChevronRight
            size={12}
            className={`transition-transform ${expanded ? 'rotate-90' : ''}`}
            aria-hidden
          />
        </button>
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-1.5">
            <Icon size={13} className={`shrink-0 ${meta.text}`} aria-hidden />
            {artifact.url ? (
              <a
                href={artifact.url}
                target="_blank"
                rel="noopener noreferrer"
                title={`Open ${meta.label} (new tab)`}
                className="min-w-0 truncate font-mono text-reported text-ink-2 hover:text-ink-1 hover:underline"
              >
                {target}
              </a>
            ) : (
              <span className="min-w-0 truncate font-mono text-reported text-ink-2">{target}</span>
            )}
            {/* Where the deliverable stands now, which the last action does
                not say: a draft the reconciler saw merged on GitHub reads
                merged here. */}
            {deliverable && (
              <span className="ml-auto">
                <StateBadge state={deliverable.state} />
              </span>
            )}
          </div>
          <div className="mt-0.5 flex items-baseline gap-1.5 font-mono text-label text-ink-3">
            <span className="shrink-0">{meta.label}</span>
            <span aria-hidden>·</span>
            <span className="min-w-0 truncate">{metaForAction(artifact.last_action).label}</span>
            {artifact.action_count > 1 && (
              <span className="shrink-0 tabular-nums">×{artifact.action_count}</span>
            )}
          </div>
        </div>
        {deliverable && overlayKind && decision && onOpenDeliverable && (
          <button
            type="button"
            onClick={() => onOpenDeliverable(overlayKind, deliverable.id)}
            aria-label={`Open ${metaForKind(deliverable.kind).label}: ${target}`}
            className="shrink-0 rounded-[4px] px-1.5 py-0.5 font-mono text-label font-semibold text-warm transition-colors hover:bg-warm/[0.1]"
          >
            Open
          </button>
        )}
        {deliverable && decision && onDismiss && (
          <DismissDeliverableButton
            kind={deliverable.kind}
            onDismiss={onDismiss}
            dismissing={dismissing}
          />
        )}
      </div>
      {expanded && (
        <div className="mt-1.5 pl-3">
          <ActionList
            conversationId={conversationId}
            objectId={artifact.object_id}
            refreshKey={`${artifact.action_count}:${artifact.last_at}`}
          />
        </div>
      )}
    </li>
  )
}

// RefusedLine — the writes the run attempted that changed nothing: a denied
// push, a blocked host, a failed branch push. Expanding it lists them.
function RefusedLine({ conversationId, count }: { conversationId: string; count: number }) {
  const [expanded, setExpanded] = useState(false)
  return (
    <div>
      <button
        type="button"
        onClick={() => setExpanded((v) => !v)}
        aria-expanded={expanded}
        className="flex items-center gap-1.5 font-mono text-label text-ink-3 transition-colors hover:text-ink-2"
      >
        <ChevronRight
          size={12}
          className={`shrink-0 transition-transform ${expanded ? 'rotate-90' : ''}`}
          aria-hidden
        />
        {`${count} refused or failed`}
      </button>
      {expanded && (
        <div className="mt-1.5 pl-3">
          <ActionList conversationId={conversationId} refused refreshKey={String(count)} />
        </div>
      )}
    </div>
  )
}
