import { useState, useEffect, useCallback, useRef, type DragEvent } from 'react'
import {
  ReactFlow,
  Background,
  type Node,
  type Edge,
  type Connection,
  type EdgeMouseHandler,
  type NodeChange,
  Handle,
  Position,
  MarkerType,
  useReactFlow,
  ReactFlowProvider,
  applyNodeChanges,
} from '@xyflow/react'
import '@xyflow/react/dist/style.css'
import type { Blueprint, BlueprintStep, Prompt, TriggerHandler } from '../types'
import { toast } from './Toast/toastStore'
import { readError } from '../lib/api'
import {
  type BindingScope,
  isTemplateScope,
  blueprintsBase,
  promptsBase,
  handlersBase,
} from '../lib/scope'

interface EventType {
  id: string
  source: string
  category: string
  label: string
  description: string
}

interface GraphProps {
  // The editor scope (SKY-381): a single team's prompts+triggers, or the org
  // template. Picks the endpoint set; everything downstream is identical. For
  // team scope, teamId '' means solo/local (the server resolves the sole team).
  scope: BindingScope
  // False until the scope resolves. For team scope this gates on the active
  // team being a concrete id (a multi-team user can't post team_id:'' in the
  // cold-load window → 400); for template scope it gates on org-admin + multi
  // confirming. The connect gesture no-ops while false.
  scopeReady: boolean
  onPromptClick?: (promptId: string) => void
  onTriggerClick?: (trigger: TriggerHandler) => void
  onTriggerDeleted?: (eventType: string, predicate?: string | null) => void
}

// --- Custom Nodes ---

function EventTypeNode({
  data,
}: {
  data: { label: string; source: string; description: string; onRemove?: () => void }
}) {
  const sourceColor: Record<string, string> = {
    github: 'border-l-emerald-500',
    jira: 'border-l-blue-500',
  }
  return (
    <div
      className={`group bg-white/90 backdrop-blur border border-border-subtle ${sourceColor[data.source] || 'border-l-gray-400'} border-l-[3px] rounded-lg px-3 py-2 min-w-[180px] max-w-[220px] shadow-sm`}
    >
      <button
        onClick={data.onRemove}
        className="absolute -top-1.5 -right-1.5 w-4 h-4 rounded-full bg-white border border-border-subtle text-text-tertiary text-[10px] leading-none flex items-center justify-center opacity-0 group-hover:opacity-100 hover:bg-red-50 hover:text-red-500 hover:border-red-200 transition-all shadow-sm"
      >
        &times;
      </button>
      <div className="text-[11px] font-semibold text-text-primary">{data.label}</div>
      <div className="text-[9px] text-text-tertiary mt-0.5 leading-relaxed">{data.description}</div>
      <Handle
        type="source"
        position={Position.Right}
        className="!w-2.5 !h-2.5 !bg-accent !border-2 !border-white"
      />
    </div>
  )
}

function PromptNode({
  data,
}: {
  data: {
    label: string
    source: string
    usageCount: number
    bodyPreview: string
    onClick?: () => void
  }
}) {
  return (
    <div
      onClick={data.onClick}
      className="bg-white/90 backdrop-blur border border-border-subtle rounded-lg px-3 py-2.5 min-w-[200px] max-w-[240px] shadow-sm hover:border-accent/30 hover:shadow-md transition-all cursor-pointer"
    >
      <Handle
        type="target"
        position={Position.Left}
        className="!w-2.5 !h-2.5 !bg-accent !border-2 !border-white"
      />
      <div className="flex items-center gap-2">
        <div className="text-[11px] font-semibold text-text-primary">{data.label}</div>
        {data.source === 'system' && (
          <span className="text-[8px] font-semibold uppercase tracking-wider px-1 py-0.5 rounded bg-black/5 text-text-tertiary">
            Sys
          </span>
        )}
      </div>
      {data.bodyPreview && (
        <div className="text-[9px] text-text-tertiary mt-1 line-clamp-2 leading-relaxed font-mono">
          {data.bodyPreview}
        </div>
      )}
      {data.usageCount > 0 && (
        <div className="text-[9px] text-text-tertiary mt-1">Used {data.usageCount}x</div>
      )}
    </div>
  )
}

const nodeTypes = {
  eventType: EventTypeNode,
  prompt: PromptNode,
}

// --- Sidebar ---

function Sidebar({ eventTypes, activeIds }: { eventTypes: EventType[]; activeIds: Set<string> }) {
  const groups: Record<string, EventType[]> = {}
  for (const et of eventTypes) {
    if (et.source === 'system') continue
    if (activeIds.has(et.id)) continue
    if (!groups[et.source]) groups[et.source] = []
    groups[et.source].push(et)
  }

  const onDragStart = (e: DragEvent, eventTypeId: string) => {
    e.dataTransfer.setData('application/event-type-id', eventTypeId)
    e.dataTransfer.effectAllowed = 'move'
  }

  const sourceLabels: Record<string, string> = { github: 'GitHub', jira: 'Jira' }
  const sourceColors: Record<string, string> = { github: 'bg-emerald-500', jira: 'bg-blue-500' }

  const allPlaced = Object.values(groups).every((g) => g.length === 0)

  return (
    <div className="absolute left-3 top-3 bottom-3 w-[190px] z-10 bg-white/80 backdrop-blur-xl border border-border-subtle rounded-xl shadow-lg overflow-hidden flex flex-col">
      <div className="px-3 py-2.5 border-b border-border-subtle shrink-0">
        <div className="text-[11px] font-semibold text-text-primary">Events</div>
        <div className="text-[9px] text-text-tertiary mt-0.5">Drag onto canvas to bind</div>
      </div>
      <div className="flex-1 overflow-y-auto px-2 py-2 space-y-3">
        {allPlaced ? (
          <p className="text-[10px] text-text-tertiary text-center py-4">All events placed</p>
        ) : (
          Object.entries(groups).map(
            ([source, items]) =>
              items.length > 0 && (
                <div key={source}>
                  <div className="flex items-center gap-1.5 px-1 mb-1.5">
                    <span
                      className={`w-1.5 h-1.5 rounded-full ${sourceColors[source] || 'bg-gray-400'}`}
                    />
                    <span className="text-[10px] font-semibold text-text-tertiary uppercase tracking-wider">
                      {sourceLabels[source] || source}
                    </span>
                  </div>
                  {items.map((et) => (
                    <div
                      key={et.id}
                      draggable
                      onDragStart={(e) => onDragStart(e, et.id)}
                      className="px-2 py-1.5 rounded-md text-[10px] text-text-secondary hover:bg-accent/5 hover:text-accent cursor-grab active:cursor-grabbing transition-colors mb-0.5"
                    >
                      {et.label}
                    </div>
                  ))}
                </div>
              ),
          )
        )}
      </div>
    </div>
  )
}

// --- Persistence ---
// Node positions are persisted per scope so a team's canvas and the org
// template's canvas (and one team vs another) don't share — and overwrite —
// each other's layout (event-type node ids are the same system registry across
// scopes, so a shared key would bleed positions and make an event placed on
// one canvas appear on the other). The key is derived from the scope below.
const STORAGE_KEY_PREFIX = 'binding-graph-layout'

function layoutStorageKey(template: boolean, teamId: string): string {
  return template
    ? `${STORAGE_KEY_PREFIX}:template`
    : `${STORAGE_KEY_PREFIX}:team:${teamId || 'local'}`
}

interface SavedLayout {
  eventPositions: Record<string, { x: number; y: number }>
  promptPositions: Record<string, { x: number; y: number }>
}

function loadLayout(key: string): SavedLayout {
  try {
    const raw = localStorage.getItem(key)
    return raw ? JSON.parse(raw) : { eventPositions: {}, promptPositions: {} }
  } catch {
    return { eventPositions: {}, promptPositions: {} }
  }
}

function saveLayout(key: string, layout: SavedLayout) {
  try {
    localStorage.setItem(key, JSON.stringify(layout))
  } catch {
    // best effort — localStorage may be full or disabled
  }
}

// --- Inner Graph ---

function BindingGraphInner({
  scope,
  scopeReady,
  onPromptClick,
  onTriggerClick,
  onTriggerDeleted,
}: GraphProps) {
  // The graph is scoped to one team OR the org template (SKY-381). For team
  // scope it shows that team's prompts + triggers and stamps new triggers with
  // it; because only the active team's prompts are ever on the canvas, a
  // connect can't bind another team's prompt (the trigger's team always
  // matches the prompt's, closing the cross-team hole by construction).
  // Template scope is org-scoped — no team_id is sent. Solo/local team scope →
  // teamId '' → the server resolves the sole team.
  const template = isTemplateScope(scope)
  const teamId = scope.kind === 'team' ? scope.teamId : ''
  const [eventTypes, setEventTypes] = useState<EventType[]>([])
  const [prompts, setPrompts] = useState<Prompt[]>([])
  const [triggers, setTriggers] = useState<TriggerHandler[]>([])
  // Each canvas node is a prompt (a blueprint step); a trigger fires a
  // blueprint. blueprintFirstPrompt maps a blueprint id → its step-0 prompt id
  // so an event edge targets the first prompt of the blueprint it fires.
  const [blueprintFirstPrompt, setBlueprintFirstPrompt] = useState<Record<string, string>>({})
  const [nodes, setNodes] = useState<Node[]>([])
  const [loading, setLoading] = useState(true)
  const [activeEventIds, setActiveEventIds] = useState<Set<string>>(new Set())
  const [confirmPopup, setConfirmPopup] = useState<{
    x: number
    y: number
    triggerId: string
  } | null>(null)
  // Persisted layout, keyed per scope (team vs template, and per team) so the
  // canvases don't overwrite each other's node positions. storageKeyRef keeps
  // the key fresh for the save callbacks (which have empty/narrow dep arrays),
  // matching this component's "refs so callbacks don't go stale" idiom; the
  // effect below reloads layoutRef when the scope changes (the /prompts page
  // switches teams without remounting).
  const storageKey = layoutStorageKey(template, teamId)
  const storageKeyRef = useRef(storageKey)
  storageKeyRef.current = storageKey
  const layoutRef = useRef<SavedLayout>(loadLayout(storageKey))
  useEffect(() => {
    layoutRef.current = loadLayout(storageKey)
  }, [storageKey])
  const { screenToFlowPosition } = useReactFlow()

  // Refs so callbacks don't go stale
  const triggersRef = useRef(triggers)
  triggersRef.current = triggers
  // Inverse of blueprintFirstPrompt (step-0 prompt id → blueprint id), in a ref
  // so the connect gesture can resolve a dropped prompt back to the blueprint it
  // fronts without going stale.
  const blueprintByFirstPromptRef = useRef<Record<string, string>>({})
  const onPromptClickRef = useRef(onPromptClick)
  onPromptClickRef.current = onPromptClick
  const onTriggerClickRef = useRef(onTriggerClick)
  onTriggerClickRef.current = onTriggerClick
  const onTriggerDeletedRef = useRef(onTriggerDeleted)
  onTriggerDeletedRef.current = onTriggerDeleted

  const fetchAll = useCallback(async () => {
    // Hold in the loading state until the scope resolves. For team scope, a
    // multi-team user before /api/teams loads has an unvalidated teamId (''
    // or a stale stored id) and fetching now would pull every visible team's
    // prompts + triggers onto the canvas; for template scope, scopeReady gates
    // on org-admin + multi confirming. scopeReady is in this callback's deps so
    // the effect re-runs the real (scoped) fetch once it flips true. Re-assert
    // loading so a ready→not-ready transition (an org switch) clears the prior
    // canvas rather than leaving it interactive during the swap.
    if (!scopeReady) {
      setLoading(true)
      return
    }
    const parseOrThrow = async (r: Response, label: string) => {
      if (!r.ok) throw new Error(`${label}: HTTP ${r.status}`)
      return r.json()
    }
    // Team scope narrows prompts + triggers to the active team (empty =
    // unfiltered, the solo/local case); template scope is org-scoped (no
    // team_id). event-types is a system registry — never scoped.
    const teamQuery = !template && teamId ? `team_id=${encodeURIComponent(teamId)}` : ''
    // The canvas renders PROMPTS (a blueprint's steps), not blueprints: a node is
    // one blueprint step, carrying a real prompt id so clicking it opens the
    // PromptDrawer (/api/prompts/{id}). The blueprint is just the grouping an
    // event fires; the box drawn around its prompts is a separate ticket. We need
    // the blueprint list + each blueprint's steps to learn the step → prompt
    // mapping (and which prompt is step 0 — the event-edge target), plus the
    // prompt list for titles + bodies. Both scopes are symmetric — team from
    // /api/*, template from /api/org-template/*.
    const blueprintsURL = `${blueprintsBase(template)}${teamQuery ? `?${teamQuery}` : ''}`
    const promptsURL = `${promptsBase(template)}${teamQuery ? `?${teamQuery}` : ''}`
    const triggersURL = `${handlersBase(template)}?kind=trigger${teamQuery ? `&${teamQuery}` : ''}`
    try {
      const [etRes, bpRes, promptRes, tRes] = await Promise.all([
        fetch('/api/event-types').then((r) => parseOrThrow(r, 'event-types')),
        fetch(blueprintsURL).then((r) => parseOrThrow(r, 'blueprints')),
        fetch(promptsURL).then((r) => parseOrThrow(r, 'prompts')),
        fetch(triggersURL).then((r) => parseOrThrow(r, 'triggers')),
      ])
      setEventTypes(etRes)
      setTriggers(tRes)

      const blueprints = bpRes as Blueprint[]
      const promptById = new Map((promptRes as Prompt[]).map((p): [string, Prompt] => [p.id, p]))

      // Fetch each blueprint's steps (N+1 over the handful of blueprints — a bulk
      // "blueprints-with-steps" read is a tracked, optional optimization).
      const stepsURL = (id: string) => `${blueprintsBase(template)}/${encodeURIComponent(id)}/steps`
      const stepLists = await Promise.all(
        blueprints.map((b) =>
          fetch(stepsURL(b.id))
            .then((r) => parseOrThrow(r, 'blueprint-steps'))
            .then((steps) => ({ blueprintId: b.id, steps: steps as BlueprintStep[] })),
        ),
      )

      // One node per blueprint step (= a prompt), deduped by prompt id so a
      // prompt reused across blueprints doesn't collide on the React Flow node
      // id. firstPrompt: blueprint → its step-0 prompt (the event-edge target);
      // byFirstPrompt is the inverse the connect gesture resolves through.
      const firstPrompt: Record<string, string> = {}
      const byFirstPrompt: Record<string, string> = {}
      const nodeList: Prompt[] = []
      const seen = new Set<string>()
      for (const { blueprintId, steps } of stepLists) {
        const ordered = [...steps].sort((a, b) => a.step_index - b.step_index)
        if (ordered.length > 0) {
          firstPrompt[blueprintId] = ordered[0].step_prompt_id
          byFirstPrompt[ordered[0].step_prompt_id] = blueprintId
        }
        for (const step of ordered) {
          if (seen.has(step.step_prompt_id)) continue
          seen.add(step.step_prompt_id)
          const prompt = promptById.get(step.step_prompt_id)
          if (prompt) nodeList.push(prompt)
        }
      }
      setPrompts(nodeList)
      setBlueprintFirstPrompt(firstPrompt)
      blueprintByFirstPromptRef.current = byFirstPrompt

      const saved = layoutRef.current
      const boundIds = new Set((tRes as TriggerHandler[]).map((t) => t.event_type))
      const active = new Set<string>()
      for (const id of Object.keys(saved.eventPositions)) active.add(id)
      for (const id of boundIds) active.add(id)
      setActiveEventIds(active)
    } catch (err) {
      // Surface to the console so devs see it; user sees an empty graph.
      // TODO: proper error banner in the graph canvas (Linear tracked).
      console.error('BindingGraph fetchAll failed:', err)
    } finally {
      setLoading(false)
    }
  }, [template, teamId, scopeReady])

  useEffect(() => {
    fetchAll()
  }, [fetchAll])

  // Remove event from canvas
  const removeEvent = useCallback(
    (eventTypeId: string) => {
      const toDelete = triggersRef.current.filter((t) => t.event_type === eventTypeId)
      Promise.all(
        toDelete.map((t) =>
          fetch(`${handlersBase(template)}/${encodeURIComponent(t.id)}`, { method: 'DELETE' }),
        ),
      ).then(() => {
        setActiveEventIds((prev) => {
          const next = new Set(prev)
          next.delete(eventTypeId)
          return next
        })
        const layout = layoutRef.current
        delete layout.eventPositions[eventTypeId]
        saveLayout(storageKeyRef.current, layout)
        fetchAll()
      })
    },
    [fetchAll, template],
  )

  // Rebuild nodes when data changes
  useEffect(() => {
    const layout = layoutRef.current
    const eventNodes: Node[] = []
    let defaultY = 40

    for (const et of eventTypes) {
      if (!activeEventIds.has(et.id)) continue
      const pos = layout.eventPositions[et.id] || { x: 240, y: defaultY }
      if (!layout.eventPositions[et.id]) {
        layout.eventPositions[et.id] = pos
      }
      eventNodes.push({
        id: `et:${et.id}`,
        type: 'eventType',
        position: pos,
        data: {
          label: et.label,
          source: et.source,
          description: et.description,
          onRemove: () => removeEvent(et.id),
        },
      })
      defaultY += 70
    }

    let promptDefaultY = 40
    const promptNodes: Node[] = prompts.map((p) => {
      const pos = layout.promptPositions[p.id] || { x: 600, y: promptDefaultY }
      if (!layout.promptPositions[p.id]) {
        layout.promptPositions[p.id] = pos
      }
      promptDefaultY += 90
      return {
        id: `p:${p.id}`,
        type: 'prompt',
        position: pos,
        data: {
          label: p.name,
          source: p.source,
          usageCount: p.usage_count,
          bodyPreview: p.body.slice(0, 80) + (p.body.length > 80 ? '...' : ''),
          onClick: () => onPromptClickRef.current?.(p.id),
        },
      }
    })

    setNodes([...eventNodes, ...promptNodes])
  }, [eventTypes, prompts, activeEventIds, removeEvent])

  // Build edges
  const edges: Edge[] = triggers
    // Drop triggers whose blueprint has no resolvable step-0 prompt so we never
    // emit an edge pointing at a node that isn't on the canvas.
    .filter((t) => activeEventIds.has(t.event_type) && blueprintFirstPrompt[t.blueprint_id])
    .map((t) => ({
      id: t.id,
      source: `et:${t.event_type}`,
      // The event wires to the blueprint's first step (a prompt node).
      target: `p:${blueprintFirstPrompt[t.blueprint_id]}`,
      type: 'default',
      animated: t.enabled,
      style: {
        stroke: t.enabled ? 'var(--color-accent)' : 'var(--color-text-tertiary)',
        strokeWidth: t.enabled ? 2 : 1,
        strokeDasharray: t.enabled ? undefined : '5 5',
        opacity: t.enabled ? 1 : 0.5,
      },
      markerEnd: {
        type: MarkerType.ArrowClosed,
        color: t.enabled ? 'var(--color-accent)' : 'var(--color-text-tertiary)',
      },
      label: t.enabled ? 'auto' : 'disabled',
      labelStyle: {
        fontSize: 9,
        fill: t.enabled ? 'var(--color-accent)' : 'var(--color-text-tertiary)',
        fontWeight: 600,
      },
      labelBgStyle: { fill: 'white', fillOpacity: 0.8 },
    }))

  // Handle node changes (dragging) — apply to state + persist positions
  const onNodesChange = useCallback((changes: NodeChange[]) => {
    setNodes((nds) => applyNodeChanges(changes, nds))

    // Persist position changes
    const layout = layoutRef.current
    let dirty = false
    for (const change of changes) {
      if (change.type === 'position' && !change.dragging && change.position) {
        const id = change.id
        if (id.startsWith('et:')) {
          layout.eventPositions[id.replace('et:', '')] = change.position
          dirty = true
        } else if (id.startsWith('p:')) {
          layout.promptPositions[id.replace('p:', '')] = change.position
          dirty = true
        }
      }
    }
    if (dirty) saveLayout(storageKeyRef.current, layout)
  }, [])

  // Connect event -> prompt (creates a new trigger). The canvas nodes are
  // prompts, but a trigger fires a blueprint, so resolve the dropped prompt back
  // to the blueprint it fronts.
  const onConnect = useCallback(
    async (connection: Connection) => {
      const eventType = connection.source?.replace('et:', '')
      const promptId = connection.target?.replace('p:', '')
      if (!eventType || !promptId) return
      // Wait for the scope to resolve. For team scope before /api/teams loads,
      // teamId is '' and posting it would 400 (ambiguous); for template scope,
      // scopeReady gates on org-admin + multi. The gesture no-ops rather than
      // failing silently — the edge simply doesn't stick (edges derive from
      // triggers).
      if (!scopeReady) return

      // Resolve the dropped prompt to the blueprint it heads. Today every prompt
      // node is a 1-step blueprint's sole (step-0) prompt, so this is a clean
      // 1:1 lookup; wiring into a multi-step blueprint's interior is a routing
      // constraint owned by a follow-up ticket.
      const blueprintId = blueprintByFirstPromptRef.current[promptId]
      if (!blueprintId) {
        toast.error('That prompt is not a blueprint entry point — cannot bind an event to it')
        return
      }

      // Team scope stamps the acting team; template scope is org-scoped (no
      // team_id — the row lands on the org template).
      const body: Record<string, unknown> = {
        kind: 'trigger',
        blueprint_id: blueprintId,
        event_type: eventType,
      }
      if (!template) {
        body.team_id = teamId
      }
      try {
        const res = await fetch(handlersBase(template), {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        })
        if (!res.ok) {
          // Surface the rejection instead of swallowing it — otherwise the
          // dragged edge just vanishes with no explanation.
          toast.error(await readError(res, 'Failed to create trigger'))
          return
        }
        fetchAll()
      } catch (err) {
        toast.error(`Failed to create trigger: ${err instanceof Error ? err.message : String(err)}`)
      }
    },
    [fetchAll, template, teamId, scopeReady],
  )

  const doDeleteTrigger = useCallback(
    async (triggerId: string) => {
      // Capture trigger info before deletion for the forgiving banner callback.
      const deleted = triggersRef.current.find((t) => t.id === triggerId)
      try {
        await fetch(`${handlersBase(template)}/${encodeURIComponent(triggerId)}`, {
          method: 'DELETE',
        })
        await fetchAll()
        // Notify parent so it can check coverage and show the forgiving banner.
        if (deleted) {
          onTriggerDeletedRef.current?.(deleted.event_type)
        }
      } catch {
        // ignore
      }
    },
    [fetchAll, template],
  )

  // Click edge to open config panel; shift-click to open delete confirmation
  const onEdgeClick: EdgeMouseHandler = useCallback((event, edge) => {
    const trigger = triggersRef.current.find((t) => t.id === edge.id)
    if (!trigger) return

    if (event.shiftKey) {
      // Shift-click: show delete confirm
      const mouseEvent = event as unknown as MouseEvent
      setConfirmPopup({ x: mouseEvent.clientX, y: mouseEvent.clientY, triggerId: trigger.id })
    } else {
      // Regular click: open trigger config panel
      onTriggerClickRef.current?.(trigger)
    }
  }, [])

  // Handle drop from sidebar
  const onDragOver = useCallback((e: DragEvent) => {
    e.preventDefault()
    e.dataTransfer.dropEffect = 'move'
  }, [])

  const onDrop = useCallback(
    (e: DragEvent) => {
      e.preventDefault()
      const eventTypeId = e.dataTransfer.getData('application/event-type-id')
      if (!eventTypeId) return

      const position = screenToFlowPosition({ x: e.clientX, y: e.clientY })

      layoutRef.current.eventPositions[eventTypeId] = position
      saveLayout(storageKeyRef.current, layoutRef.current)

      setActiveEventIds((prev) => new Set([...prev, eventTypeId]))
    },
    [screenToFlowPosition],
  )

  if (loading) {
    return (
      <div className="flex items-center justify-center h-full text-text-tertiary text-sm">
        Loading graph...
      </div>
    )
  }

  return (
    <div className="h-full relative" onDragOver={onDragOver} onDrop={onDrop}>
      <Sidebar eventTypes={eventTypes} activeIds={activeEventIds} />
      <ReactFlow
        nodes={nodes}
        edges={edges}
        nodeTypes={nodeTypes}
        onNodesChange={onNodesChange}
        onConnect={onConnect}
        onEdgeClick={onEdgeClick}
        fitView
        fitViewOptions={{ padding: 0.4 }}
        proOptions={{ hideAttribution: true }}
        minZoom={0.4}
        maxZoom={1.5}
        defaultEdgeOptions={{ type: 'default' }}
      >
        <Background color="var(--color-border-subtle)" gap={20} size={1} />
      </ReactFlow>

      {/* Confirm delete popup */}
      {confirmPopup && (
        <>
          <div className="fixed inset-0 z-50" onClick={() => setConfirmPopup(null)} />
          <div
            className="fixed z-50 bg-surface-raised/95 backdrop-blur-xl border border-border-glass rounded-xl shadow-xl shadow-black/10 px-4 py-3 w-[220px]"
            style={{ left: confirmPopup.x - 110, top: confirmPopup.y - 80 }}
          >
            <p className="text-[12px] text-text-primary font-medium mb-3">Remove this trigger?</p>
            <div className="flex items-center justify-end gap-2">
              <button
                onClick={() => setConfirmPopup(null)}
                className="text-[11px] text-text-tertiary hover:text-text-secondary font-medium px-2.5 py-1 rounded-md transition-colors"
              >
                Cancel
              </button>
              <button
                onClick={() => {
                  doDeleteTrigger(confirmPopup.triggerId)
                  setConfirmPopup(null)
                }}
                className="text-[11px] font-semibold text-white bg-red-500 hover:bg-red-600 px-3 py-1 rounded-md transition-colors"
              >
                Remove
              </button>
            </div>
          </div>
        </>
      )}
    </div>
  )
}

// --- Wrapper with Provider ---

export default function BindingGraph(props: GraphProps) {
  return (
    <div className="h-full rounded-2xl border border-border-subtle bg-white/30 overflow-hidden">
      <ReactFlowProvider>
        <BindingGraphInner {...props} />
      </ReactFlowProvider>
    </div>
  )
}
