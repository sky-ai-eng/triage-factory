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
import type { Prompt, PromptBinding } from '../types'

interface EventType {
  id: string
  source: string
  category: string
  label: string
  description: string
}

interface GraphProps {
  onPromptClick?: (promptId: string) => void
}

// --- Custom Nodes ---

function EventTypeNode({ data }: { data: { label: string; source: string; description: string; onRemove?: () => void } }) {
  const sourceColor: Record<string, string> = {
    github: 'border-l-emerald-500',
    jira: 'border-l-blue-500',
  }
  return (
    <div className={`group bg-white/90 backdrop-blur border border-border-subtle ${sourceColor[data.source] || 'border-l-gray-400'} border-l-[3px] rounded-lg px-3 py-2 min-w-[180px] max-w-[220px] shadow-sm`}>
      <button
        onClick={data.onRemove}
        className="absolute -top-1.5 -right-1.5 w-4 h-4 rounded-full bg-white border border-border-subtle text-text-tertiary text-[10px] leading-none flex items-center justify-center opacity-0 group-hover:opacity-100 hover:bg-red-50 hover:text-red-500 hover:border-red-200 transition-all shadow-sm"
      >
        &times;
      </button>
      <div className="text-[11px] font-semibold text-text-primary">{data.label}</div>
      <div className="text-[9px] text-text-tertiary mt-0.5 leading-relaxed">{data.description}</div>
      <Handle type="source" position={Position.Right} className="!w-2.5 !h-2.5 !bg-accent !border-2 !border-white" />
    </div>
  )
}

function PromptNode({ data }: { data: { label: string; source: string; usageCount: number; bodyPreview: string; onClick?: () => void } }) {
  return (
    <div
      onClick={data.onClick}
      className="bg-white/90 backdrop-blur border border-border-subtle rounded-lg px-3 py-2.5 min-w-[200px] max-w-[240px] shadow-sm hover:border-accent/30 hover:shadow-md transition-all cursor-pointer"
    >
      <Handle type="target" position={Position.Left} className="!w-2.5 !h-2.5 !bg-accent !border-2 !border-white" />
      <div className="flex items-center gap-2">
        <div className="text-[11px] font-semibold text-text-primary">{data.label}</div>
        {data.source === 'system' && (
          <span className="text-[8px] font-semibold uppercase tracking-wider px-1 py-0.5 rounded bg-black/5 text-text-tertiary">Sys</span>
        )}
      </div>
      {data.bodyPreview && (
        <div className="text-[9px] text-text-tertiary mt-1 line-clamp-2 leading-relaxed font-mono">{data.bodyPreview}</div>
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

  const allPlaced = Object.values(groups).every(g => g.length === 0)

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
          Object.entries(groups).map(([source, items]) => (
            items.length > 0 && (
              <div key={source}>
                <div className="flex items-center gap-1.5 px-1 mb-1.5">
                  <span className={`w-1.5 h-1.5 rounded-full ${sourceColors[source] || 'bg-gray-400'}`} />
                  <span className="text-[10px] font-semibold text-text-tertiary uppercase tracking-wider">{sourceLabels[source] || source}</span>
                </div>
                {items.map(et => (
                  <div
                    key={et.id}
                    draggable
                    onDragStart={e => onDragStart(e, et.id)}
                    className="px-2 py-1.5 rounded-md text-[10px] text-text-secondary hover:bg-accent/5 hover:text-accent cursor-grab active:cursor-grabbing transition-colors mb-0.5"
                  >
                    {et.label}
                  </div>
                ))}
              </div>
            )
          ))
        )}
      </div>
    </div>
  )
}

// --- Persistence ---
const STORAGE_KEY = 'binding-graph-layout'

interface SavedLayout {
  eventPositions: Record<string, { x: number; y: number }>
  promptPositions: Record<string, { x: number; y: number }>
}

function loadLayout(): SavedLayout {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    return raw ? JSON.parse(raw) : { eventPositions: {}, promptPositions: {} }
  } catch { return { eventPositions: {}, promptPositions: {} } }
}

function saveLayout(layout: SavedLayout) {
  try { localStorage.setItem(STORAGE_KEY, JSON.stringify(layout)) } catch {}
}

// --- Inner Graph ---

function BindingGraphInner({ onPromptClick }: GraphProps) {
  const [eventTypes, setEventTypes] = useState<EventType[]>([])
  const [prompts, setPrompts] = useState<Prompt[]>([])
  const [bindings, setBindings] = useState<PromptBinding[]>([])
  const [nodes, setNodes] = useState<Node[]>([])
  const [loading, setLoading] = useState(true)
  const [activeEventIds, setActiveEventIds] = useState<Set<string>>(new Set())
  const [confirmPopup, setConfirmPopup] = useState<{ x: number; y: number; promptId: string; eventType: string } | null>(null)
  const layoutRef = useRef<SavedLayout>(loadLayout())
  const { screenToFlowPosition } = useReactFlow()

  // Refs so callbacks don't go stale
  const bindingsRef = useRef(bindings)
  bindingsRef.current = bindings
  const onPromptClickRef = useRef(onPromptClick)
  onPromptClickRef.current = onPromptClick

  const fetchAll = useCallback(async () => {
    try {
      const [etRes, pRes, bRes] = await Promise.all([
        fetch('/api/event-types').then(r => r.json()),
        fetch('/api/prompts').then(r => r.json()),
        fetch('/api/bindings').then(r => r.json()),
      ])
      setEventTypes(etRes)
      setPrompts(pRes)
      setBindings(bRes)

      const saved = layoutRef.current
      const boundIds = new Set((bRes as PromptBinding[]).map(b => b.event_type))
      const active = new Set<string>()
      for (const id of Object.keys(saved.eventPositions)) active.add(id)
      for (const id of boundIds) active.add(id)
      setActiveEventIds(active)
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => { fetchAll() }, [fetchAll])

  // Remove event from canvas
  const removeEvent = useCallback((eventTypeId: string) => {
    const toDelete = bindingsRef.current.filter(b => b.event_type === eventTypeId)
    Promise.all(
      toDelete.map(b =>
        fetch(`/api/bindings?prompt_id=${encodeURIComponent(b.prompt_id)}&event_type=${encodeURIComponent(b.event_type)}`, { method: 'DELETE' })
      )
    ).then(() => {
      setActiveEventIds(prev => {
        const next = new Set(prev)
        next.delete(eventTypeId)
        return next
      })
      const layout = layoutRef.current
      delete layout.eventPositions[eventTypeId]
      saveLayout(layout)
      fetchAll()
    })
  }, [fetchAll])

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
        data: { label: et.label, source: et.source, description: et.description, onRemove: () => removeEvent(et.id) },
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
  const edges: Edge[] = bindings
    .filter(b => activeEventIds.has(b.event_type))
    .map(b => ({
      id: `${b.prompt_id}__${b.event_type}`,
      source: `et:${b.event_type}`,
      target: `p:${b.prompt_id}`,
      type: 'default',
      animated: b.is_default,
      style: {
        stroke: b.is_default ? 'var(--color-claim)' : 'var(--color-text-tertiary)',
        strokeWidth: b.is_default ? 2 : 1,
        strokeDasharray: b.is_default ? undefined : '5 5',
        opacity: b.is_default ? 1 : 0.5,
      },
      markerEnd: b.is_default ? { type: MarkerType.ArrowClosed, color: 'var(--color-claim)' } : undefined,
      label: b.is_default ? 'default' : '',
      labelStyle: { fontSize: 9, fill: 'var(--color-claim)', fontWeight: 600 },
      labelBgStyle: { fill: 'white', fillOpacity: 0.8 },
    }))

  // Handle node changes (dragging) — apply to state + persist positions
  const onNodesChange = useCallback((changes: NodeChange[]) => {
    setNodes(nds => applyNodeChanges(changes, nds))

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
    if (dirty) saveLayout(layout)
  }, [])

  // Connect event -> prompt
  const onConnect = useCallback(async (connection: Connection) => {
    const eventType = connection.source?.replace('et:', '')
    const promptId = connection.target?.replace('p:', '')
    if (!eventType || !promptId) return

    const existingForEvent = bindingsRef.current.filter(b => b.event_type === eventType)
    const isDefault = existingForEvent.length === 0

    try {
      await fetch('/api/bindings', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ prompt_id: promptId, event_type: eventType, is_default: isDefault }),
      })
      fetchAll()
    } catch {}
  }, [fetchAll])

  const doDeleteBinding = useCallback(async (promptId: string, eventType: string) => {
    try {
      await fetch(`/api/bindings?prompt_id=${encodeURIComponent(promptId)}&event_type=${encodeURIComponent(eventType)}`, { method: 'DELETE' })
      fetchAll()
    } catch {}
  }, [fetchAll])

  // Click edge to toggle default or delete
  const onEdgeClick: EdgeMouseHandler = useCallback(async (event, edge) => {
    const [promptId, eventType] = edge.id.split('__')
    const binding = bindingsRef.current.find(b => b.prompt_id === promptId && b.event_type === eventType)
    if (!binding) return

    if (binding.is_default) {
      // Show confirm popup at click position
      const mouseEvent = event as unknown as MouseEvent
      setConfirmPopup({ x: mouseEvent.clientX, y: mouseEvent.clientY, promptId, eventType })
    } else {
      try {
        await fetch('/api/bindings/set-default', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ prompt_id: promptId, event_type: eventType, is_default: true }),
        })
        fetchAll()
      } catch {}
    }
  }, [fetchAll])

  // Handle drop from sidebar
  const onDragOver = useCallback((e: DragEvent) => {
    e.preventDefault()
    e.dataTransfer.dropEffect = 'move'
  }, [])

  const onDrop = useCallback((e: DragEvent) => {
    e.preventDefault()
    const eventTypeId = e.dataTransfer.getData('application/event-type-id')
    if (!eventTypeId) return

    const position = screenToFlowPosition({ x: e.clientX, y: e.clientY })

    layoutRef.current.eventPositions[eventTypeId] = position
    saveLayout(layoutRef.current)

    setActiveEventIds(prev => new Set([...prev, eventTypeId]))
  }, [screenToFlowPosition])

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
            <p className="text-[12px] text-text-primary font-medium mb-3">Remove this binding?</p>
            <div className="flex items-center justify-end gap-2">
              <button
                onClick={() => setConfirmPopup(null)}
                className="text-[11px] text-text-tertiary hover:text-text-secondary font-medium px-2.5 py-1 rounded-md transition-colors"
              >
                Cancel
              </button>
              <button
                onClick={() => {
                  doDeleteBinding(confirmPopup.promptId, confirmPopup.eventType)
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
