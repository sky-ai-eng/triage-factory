import { useState, useEffect, useCallback, useMemo, useRef, type DragEvent } from 'react'
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
import { Copy, Layers, MoreVertical, Trash2 } from 'lucide-react'
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
  // The editor scope: a single team's prompts+triggers, or the org
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
      onClick={(e) => {
        // A modifier-click is a multi-select gesture (add to / toggle the
        // selection), not an "open this prompt" click — let ReactFlow handle the
        // selection and don't open the drawer over it.
        if (e.shiftKey || e.metaKey || e.ctrlKey) return
        data.onClick?.()
      }}
      className="bg-white/90 backdrop-blur border border-border-subtle rounded-lg px-3 py-2.5 min-w-[200px] max-w-[240px] shadow-sm hover:border-accent/30 hover:shadow-md transition-all cursor-pointer"
    >
      {/* Target (left): a prompt's single input — an event (it becomes a
          blueprint's entry) OR an upstream step. Source (right): its single
          step-output to the next step. The ≤1-in/≤1-out invariant is enforced
          in onConnect, not by the handles (which would silently swallow). */}
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
      <Handle
        type="source"
        position={Position.Right}
        className="!w-2.5 !h-2.5 !bg-accent !border-2 !border-white"
      />
    </div>
  )
}

// BlueprintBoxNode is the procedural bounding box drawn around a multi-step
// blueprint's prompts (decision 4). It is a *decorative grouping element*, NOT a
// ReactFlow group/parent node: the member prompt nodes stay free-floating and
// independently draggable, and this box's rect is recomputed from their measured
// positions on every change (see computeBoxNodes). It sits behind the nodes.
//
// Pointer events: the box's ReactFlow wrapper is rendered `pointer-events: none`
// (set via node.style in computeBoxNodes) so its interior is *click-through* —
// a shift-click on a sequence edge that runs inside the box now reaches the edge
// instead of being swallowed by the box's rect. Interactivity (whole-blueprint
// drag + box selection) lives only on the chrome elements below, which re-enable
// pointer events; events they receive bubble up to the (otherwise inert) wrapper,
// driving ReactFlow's own drag + select machinery. The kebab carries `nodrag`
// (and stops propagation) so clicking it opens the details popup rather than
// starting a drag or selecting the box. The faint fill keeps the inner sequence
// edges visible.
function BlueprintBoxNode({
  data,
  selected,
}: {
  data: {
    name: string
    blueprintId: string
    stepCount: number
    onBoxEdit: (blueprintId: string, clientX: number, clientY: number) => void
  }
  selected?: boolean
}) {
  return (
    <div
      className={`relative w-full h-full rounded-2xl border bg-accent/[0.04] transition-colors ${
        selected ? 'border-accent ring-2 ring-accent/30' : 'border-border-subtle'
      }`}
    >
      {/* Header (top edge): the blueprint's name + the primary drag/select
          handle. `pr-8` clears the kebab so a long name truncates before it. */}
      <div className="pointer-events-auto absolute inset-x-0 top-0 flex h-[30px] items-center gap-1.5 pl-3 pr-8 cursor-grab active:cursor-grabbing">
        <Layers size={12} className="text-text-tertiary shrink-0" />
        <span className="text-[11px] font-semibold text-text-tertiary truncate">{data.name}</span>
        <span className="text-[10px] text-text-tertiary/60 shrink-0">· {data.stepCount} steps</span>
      </div>
      {/* Edit button (top-right) — opens the blueprint details popup. `nodrag`
          keeps a click from starting a box drag; stopPropagation keeps it from
          selecting the box. */}
      <button
        onClick={(e) => {
          e.stopPropagation()
          data.onBoxEdit(data.blueprintId, e.clientX, e.clientY)
        }}
        title="Blueprint details"
        className="nodrag pointer-events-auto absolute right-1.5 top-1.5 w-6 h-6 rounded-md flex items-center justify-center text-text-tertiary hover:text-text-secondary hover:bg-black/[0.04] transition-colors cursor-pointer"
      >
        <MoreVertical size={14} />
      </button>
      {/* Border strips (left/right/bottom; the header covers the top edge) — grab
          any edge to drag or click to select the whole blueprint. They live in
          the box's padding gutter, narrower than it so they stay clear of both
          the member prompt cards and the inner sequence edges. */}
      <div className="pointer-events-auto absolute left-0 top-[30px] bottom-0 w-2.5 cursor-grab active:cursor-grabbing" />
      <div className="pointer-events-auto absolute right-0 top-[30px] bottom-0 w-2.5 cursor-grab active:cursor-grabbing" />
      <div className="pointer-events-auto absolute inset-x-0 bottom-0 h-2.5 cursor-grab active:cursor-grabbing" />
    </div>
  )
}

const nodeTypes = {
  eventType: EventTypeNode,
  prompt: PromptNode,
  blueprintBox: BlueprintBoxNode,
}

// Box geometry. The box hugs its member prompt nodes with room at the top for
// the title row. Fallback dims cover the first render before ReactFlow has
// measured the nodes; the box snaps to true size on the following dimensions
// change.
const BOX_PAD = { left: 22, right: 22, top: 38, bottom: 22 }
const FALLBACK_NODE = { width: 220, height: 84 }

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

// --- Connection planning ---
// Every connect gesture is resolved against blueprint membership (decision 5 —
// always render/route from blueprint_id, never infer boxes from the live edge
// graph) into one concrete plan, or a refusal with a user-facing reason. This
// is the single place the linear + single-input invariant (decision 7) lives.

type ConnPlan =
  | { ok: false; reason: string }
  // event → a blueprint's entry prompt: bind/replace a trigger.
  | { ok: true; kind: 'trigger'; eventType: string; blueprintId: string }
  // tail of one chain → entry of a trigger-less chain: atomic merge.
  | { ok: true; kind: 'merge'; host: string; source: string }

interface Membership {
  // step-0 prompt id per blueprint (the event-edge target / chain entry).
  firstPrompt: Record<string, string>
  // ordered step prompt ids per blueprint.
  steps: Record<string, string[]>
  // prompt id → its owning blueprint (1:1 — prompts are copy-only).
  promptToBlueprint: Record<string, string>
  // blueprint ids that already have an event trigger.
  triggered: Set<string>
}

function planConnection(c: Connection, m: Membership): ConnPlan {
  const source = c.source ?? ''
  const target = c.target ?? ''
  if (!target.startsWith('p:')) {
    return { ok: false, reason: 'A connection has to end on a prompt' }
  }
  const targetPid = target.slice(2)
  const targetBp = m.promptToBlueprint[targetPid]
  if (!targetBp) {
    return { ok: false, reason: 'That prompt is not part of a blueprint' }
  }
  // A prompt takes a single input. The only free input slot is a chain's entry
  // (step 0) that isn't already triggered — a mid-chain prompt already has its
  // upstream step edge, a triggered entry already has its event.
  const targetIsEntry = m.firstPrompt[targetBp] === targetPid
  const targetTriggered = m.triggered.has(targetBp)
  if (!targetIsEntry) {
    return {
      ok: false,
      reason: 'That prompt already has an input — a step takes a single incoming edge',
    }
  }

  if (source.startsWith('et:')) {
    if (targetTriggered) {
      return {
        ok: false,
        reason: 'This blueprint already has a trigger — a blueprint is fired by exactly one event',
      }
    }
    return { ok: true, kind: 'trigger', eventType: source.slice(3), blueprintId: targetBp }
  }

  if (source.startsWith('p:')) {
    const srcPid = source.slice(2)
    const srcBp = m.promptToBlueprint[srcPid]
    if (!srcBp) {
      return { ok: false, reason: 'That prompt is not part of a blueprint' }
    }
    if (srcBp === targetBp) {
      return { ok: false, reason: 'A blueprint can’t wire into itself' }
    }
    // A step has a single output: only a chain's tail (last step) has a free
    // outgoing slot to extend onward.
    const srcSteps = m.steps[srcBp] ?? []
    const srcIsTail = srcSteps.length > 0 && srcSteps[srcSteps.length - 1] === srcPid
    if (!srcIsTail) {
      return {
        ok: false,
        reason: 'Only a blueprint’s last step can extend onward — a step has a single output',
      }
    }
    // Merge absorbs the trigger-less downstream chain onto this tail.
    if (targetTriggered) {
      return {
        ok: false,
        reason: 'Detach the downstream blueprint’s trigger before merging it in',
      }
    }
    return { ok: true, kind: 'merge', host: srcBp, source: targetBp }
  }

  return { ok: false, reason: 'Unsupported connection' }
}

// --- Duplication ---
// Copies land at a fixed offset from their originals so a duplicate reads as a
// distinct, unstacked copy (and a group duplicate keeps the selection's relative
// layout, shifted as a whole).
const DUP_OFFSET = 40

// orderSelectedPromptIds reproduces the duplicate endpoint's deterministic output
// ordering (its PartitionDuplicationRuns) on the client, so a returned copy can be
// placed at its original's position. The endpoint groups the selected prompts by
// source blueprint (id-sorted), sorts each group by step_index, drops duplicate
// selections, and partitions into contiguous runs; flattening that in order is
// simply blueprints-by-id then steps-by-index. Reproducing exactly that flattened
// order means returned-copy[k] corresponds to original[k]. Unresolvable ids (a
// stale clipboard entry whose prompt is gone) are dropped, matching the endpoint.
function orderSelectedPromptIds(
  promptIds: string[],
  stepsByBlueprint: Record<string, string[]>,
  promptToBlueprint: Record<string, string>,
): string[] {
  const byBlueprint = new Map<string, number[]>()
  const seen = new Set<string>()
  for (const pid of promptIds) {
    if (seen.has(pid)) continue
    seen.add(pid)
    const bpId = promptToBlueprint[pid]
    if (!bpId) continue
    const idx = (stepsByBlueprint[bpId] ?? []).indexOf(pid)
    if (idx < 0) continue
    const arr = byBlueprint.get(bpId)
    if (arr) arr.push(idx)
    else byBlueprint.set(bpId, [idx])
  }
  const out: string[] = []
  for (const bpId of [...byBlueprint.keys()].sort()) {
    const steps = stepsByBlueprint[bpId] ?? []
    // The outer `seen` set already collapses repeated prompt ids, so each index
    // appears once per blueprint — just sort by index.
    for (const idx of byBlueprint.get(bpId)!.sort((a, b) => a - b)) {
      out.push(steps[idx])
    }
  }
  return out
}

// --- Deletion ---
// A canvas deletion (single Delete or a bulk selection) resolved to concrete
// backend operations, partitioned by element type. Edges aren't first-class
// here — a sequence/trigger edge is removed as a *consequence* of deleting the
// prompt (split semantics) or blueprint (cascade) it belongs to, or directly via
// the shift-click edge menu — so a plan is just the blueprints + prompts to
// delete. Member prompts of a blueprint that's also being deleted are dropped
// from `prompts` (the blueprint delete cascades them).
interface DeletePlan {
  blueprints: { id: string; name: string }[]
  prompts: { id: string; name: string; blueprintId: string; stepIndex: number }[]
}

// A short "2 blueprints and 1 prompt" phrase for the confirm dialog title.
function summarizeDeletePlan(plan: DeletePlan): string {
  const parts: string[] = []
  const b = plan.blueprints.length
  const p = plan.prompts.length
  if (b) parts.push(`${b} blueprint${b === 1 ? '' : 's'}`)
  if (p) parts.push(`${p} prompt${p === 1 ? '' : 's'}`)
  return parts.join(' and ')
}

// --- Inner Graph ---

function BindingGraphInner({
  scope,
  scopeReady,
  onPromptClick,
  onTriggerClick,
  onTriggerDeleted,
}: GraphProps) {
  // The graph is scoped to one team OR the org template. For team
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
  const [blueprints, setBlueprints] = useState<Blueprint[]>([])
  // blueprintFirstPrompt maps a blueprint id → its step-0 prompt id so an event
  // edge targets the entry of the blueprint it fires; blueprintSteps holds each
  // blueprint's ordered step prompt ids (drives the procedural boxes + the
  // prompt→prompt sequence edges).
  const [blueprintFirstPrompt, setBlueprintFirstPrompt] = useState<Record<string, string>>({})
  const [blueprintSteps, setBlueprintSteps] = useState<Record<string, string[]>>({})
  const [nodes, setNodes] = useState<Node[]>([])
  const [loading, setLoading] = useState(true)
  const [activeEventIds, setActiveEventIds] = useState<Set<string>>(new Set())
  // Edge context menu (shift-click an edge). For a trigger edge it offers edit
  // (→ the trigger config panel) + remove; for a sequence edge, split-here.
  const [edgeMenu, setEdgeMenu] = useState<
    | { x: number; y: number; kind: 'trigger'; trigger: TriggerHandler }
    | { x: number; y: number; kind: 'sequence'; blueprintId: string; atStepIndex: number }
    | null
  >(null)
  // Blueprint details popup (the box's edit button).
  const [boxMenu, setBoxMenu] = useState<{ x: number; y: number; blueprintId: string } | null>(null)
  // Draft for the blueprint rename field in the details popup, seeded from the
  // current name when the popup opens.
  const [boxNameDraft, setBoxNameDraft] = useState('')
  // Two-stage guard for the box menu's destructive Delete action — false shows
  // the "Delete blueprint" button, true swaps in an inline confirm naming the
  // blueprint + its step count. Reset whenever the menu opens or closes so a
  // stale "armed" state never carries to the next blueprint.
  const [boxDeleteConfirm, setBoxDeleteConfirm] = useState(false)
  // A pending bulk/blueprint deletion awaiting confirmation. Set by onBeforeDelete
  // when the Delete-key target needs a confirm (any blueprint, or >1 element);
  // the dialog names what's affected and runs executeDeletePlan on confirm.
  const [deleteConfirm, setDeleteConfirm] = useState<DeletePlan | null>(null)
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

  // Derived membership maps. Kept as memos (for the edge builder + render) and
  // mirrored to refs (so the connect/reorder/box callbacks read fresh values
  // without going stale on their narrow dep arrays).
  const blueprintNames = useMemo(
    () => Object.fromEntries(blueprints.map((b) => [b.id, b.name])),
    [blueprints],
  )
  const promptToBlueprint = useMemo(() => {
    const m: Record<string, string> = {}
    for (const [bpId, ids] of Object.entries(blueprintSteps)) {
      for (const pid of ids) m[pid] = bpId
    }
    return m
  }, [blueprintSteps])
  const triggeredBlueprints = useMemo(
    () => new Set(triggers.map((t) => t.blueprint_id)),
    [triggers],
  )

  // Refs so callbacks don't go stale.
  const triggersRef = useRef(triggers)
  triggersRef.current = triggers
  const firstPromptRef = useRef(blueprintFirstPrompt)
  firstPromptRef.current = blueprintFirstPrompt
  const blueprintStepsRef = useRef(blueprintSteps)
  blueprintStepsRef.current = blueprintSteps
  const blueprintNamesRef = useRef(blueprintNames)
  blueprintNamesRef.current = blueprintNames
  const promptToBlueprintRef = useRef(promptToBlueprint)
  promptToBlueprintRef.current = promptToBlueprint
  const triggeredRef = useRef(triggeredBlueprints)
  triggeredRef.current = triggeredBlueprints
  // Full step objects per blueprint (carry the per-step brief), needed to
  // reconstruct a PUT-steps body on reorder without dropping briefs.
  const stepObjsRef = useRef<Record<string, BlueprintStep[]>>({})
  const nodesRef = useRef<Node[]>(nodes)
  nodesRef.current = nodes
  const onPromptClickRef = useRef(onPromptClick)
  onPromptClickRef.current = onPromptClick
  const onTriggerClickRef = useRef(onTriggerClick)
  onTriggerClickRef.current = onTriggerClick
  const onTriggerDeletedRef = useRef(onTriggerDeleted)
  onTriggerDeletedRef.current = onTriggerDeleted
  // In-canvas clipboard backing Cmd/Ctrl+C → V. Same-scope only: it carries the
  // scope's storage key so a copy in one team/template can't paste into another
  // (the endpoint would reject the cross-scope ids anyway; this fails the gesture
  // cleanly first). Page-local — not persisted across reloads (a non-goal).
  const clipboardRef = useRef<{ scopeKey: string; promptIds: string[] } | null>(null)
  // Prompt ids to mark selected after the next fetch rebuilds the nodes — set by a
  // duplicate so the freshly-pasted copies come back selected and immediately
  // wireable. Read + cleared in the node-build effect.
  const pendingSelectRef = useRef<Set<string>>(new Set())
  // True while a duplicate request is in flight, so a rapid second gesture (Cmd+D
  // held, double paste) doesn't race two refetches/selections.
  const duplicatingRef = useRef(false)
  // True while a delete batch is in flight, so a held/double Delete (or a
  // double-click of the confirm dialog's Delete) doesn't dispatch twice.
  const deletingRef = useRef(false)

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
    // The canvas renders PROMPTS (a blueprint's steps) as nodes, with a
    // procedural box drawn around each multi-step blueprint. We need the
    // blueprint list + each blueprint's steps (the step→prompt mapping, the
    // entry prompt, and the order that draws the sequence edges), plus the
    // prompt list for titles + bodies. Both scopes are symmetric — team from
    // /api/*, template from /api/org-template/*.
    const blueprintsURL = `${blueprintsBase(template)}${teamQuery ? `?${teamQuery}` : ''}`
    const promptsURL = `${promptsBase(template)}${teamQuery ? `?${teamQuery}` : ''}`
    const triggersURL = `${handlersBase(template)}?kind=trigger${teamQuery ? `&${teamQuery}` : ''}`
    // One bulk steps read for the whole scope (grouped client-side) rather than
    // a GET .../{id}/steps per blueprint — folded into the parallel fetch below
    // so the canvas loads in a single round-trip.
    const stepsURL = `${template ? '/api/org-template/blueprint-steps' : '/api/blueprint-steps'}${teamQuery ? `?${teamQuery}` : ''}`
    try {
      const [etRes, bpRes, promptRes, tRes, stepsRes] = await Promise.all([
        fetch('/api/event-types').then((r) => parseOrThrow(r, 'event-types')),
        fetch(blueprintsURL).then((r) => parseOrThrow(r, 'blueprints')),
        fetch(promptsURL).then((r) => parseOrThrow(r, 'prompts')),
        fetch(triggersURL).then((r) => parseOrThrow(r, 'triggers')),
        fetch(stepsURL).then((r) => parseOrThrow(r, 'blueprint-steps')),
      ])
      setEventTypes(etRes)
      setTriggers(tRes)

      const blueprintList = bpRes as Blueprint[]
      const promptById = new Map((promptRes as Prompt[]).map((p): [string, Prompt] => [p.id, p]))

      // Group the scope's steps (one bulk read) by blueprint_id.
      const stepsByBp = new Map<string, BlueprintStep[]>()
      for (const st of stepsRes as BlueprintStep[]) {
        const arr = stepsByBp.get(st.blueprint_id)
        if (arr) arr.push(st)
        else stepsByBp.set(st.blueprint_id, [st])
      }

      // One node per blueprint step (= a prompt). Because prompts are copy-only
      // — a prompt belongs to exactly one blueprint, so every step prompt is a
      // member of exactly one box — no shared-prompt-across-boxes collision.
      // firstPrompt: blueprint → its entry prompt; stepsByBlueprint: ordered
      // prompt ids; stepObjs: full step rows (for the reorder PUT).
      const firstPrompt: Record<string, string> = {}
      const stepsByBlueprint: Record<string, string[]> = {}
      const stepObjs: Record<string, BlueprintStep[]> = {}
      const nodeList: Prompt[] = []
      const seen = new Set<string>()
      for (const b of blueprintList) {
        const ordered = [...(stepsByBp.get(b.id) ?? [])].sort((x, y) => x.step_index - y.step_index)
        stepObjs[b.id] = ordered
        stepsByBlueprint[b.id] = ordered.map((s) => s.step_prompt_id)
        if (ordered.length > 0) {
          firstPrompt[b.id] = ordered[0].step_prompt_id
        }
        for (const step of ordered) {
          if (seen.has(step.step_prompt_id)) continue
          seen.add(step.step_prompt_id)
          const prompt = promptById.get(step.step_prompt_id)
          if (prompt) nodeList.push(prompt)
        }
      }
      setPrompts(nodeList)
      setBlueprints(blueprintList)
      setBlueprintFirstPrompt(firstPrompt)
      setBlueprintSteps(stepsByBlueprint)
      stepObjsRef.current = stepObjs

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

  // Close any open context menu / pending delete when the scope changes — a
  // held-open edge/box menu or confirm dialog would otherwise carry a stale
  // trigger / blueprint reference from the previous team or template.
  useEffect(() => {
    setEdgeMenu(null)
    setBoxMenu(null)
    setBoxDeleteConfirm(false)
    setDeleteConfirm(null)
  }, [template, teamId])

  // Remove event from canvas
  const removeEvent = useCallback(
    async (eventTypeId: string) => {
      const toDelete = triggersRef.current.filter((t) => t.event_type === eventTypeId)
      try {
        const results = await Promise.all(
          toDelete.map((t) =>
            fetch(`${handlersBase(template)}/${encodeURIComponent(t.id)}`, { method: 'DELETE' }),
          ),
        )
        const failed = results.find((r) => !r.ok)
        if (failed) {
          toast.error(await readError(failed, 'Failed to remove event'))
          return
        }
        // Drop the node + its saved position only once the backend confirms the
        // triggers are gone — otherwise the canvas would diverge from the server.
        setActiveEventIds((prev) => {
          const next = new Set(prev)
          next.delete(eventTypeId)
          return next
        })
        const layout = layoutRef.current
        delete layout.eventPositions[eventTypeId]
        saveLayout(storageKeyRef.current, layout)
        fetchAll()
      } catch (err) {
        toast.error(`Failed to remove event: ${err instanceof Error ? err.message : String(err)}`)
      }
    },
    [fetchAll, template],
  )

  // Open the blueprint details popup at the edit button's location.
  const onBoxEdit = useCallback((blueprintId: string, clientX: number, clientY: number) => {
    setBoxNameDraft(blueprintNamesRef.current[blueprintId] ?? '')
    setBoxDeleteConfirm(false)
    setBoxMenu({ x: clientX, y: clientY, blueprintId })
  }, [])

  // Rename a blueprint (its name is independent of its entry prompt's). Both
  // scopes have the endpoint — team via PUT /api/blueprints/{id}, template via
  // the org-template mirror — so the URL is scope-derived like every other call.
  const saveBoxName = useCallback(
    async (blueprintId: string, rawName: string) => {
      const name = rawName.trim()
      if (!name) return
      try {
        const res = await fetch(`${blueprintsBase(template)}/${encodeURIComponent(blueprintId)}`, {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ name }),
        })
        if (!res.ok) {
          toast.error(await readError(res, 'Failed to rename blueprint'))
          return
        }
        setBoxMenu(null)
        fetchAll()
      } catch (err) {
        toast.error(
          `Failed to rename blueprint: ${err instanceof Error ? err.message : String(err)}`,
        )
      }
    },
    [template, fetchAll],
  )

  // Delete a whole blueprint (server soft-deletes the box + its copy-only step
  // prompts and detaches the bound trigger). Core does the request + error toast
  // only — no UI side effects, no refetch — so it composes inside a bulk batch
  // (one refetch at the end). Scope-derived URL like rename/duplicate; both the
  // team and org-template families expose the DELETE.
  const deleteBlueprintCore = useCallback(
    async (blueprintId: string): Promise<boolean> => {
      try {
        const res = await fetch(`${blueprintsBase(template)}/${encodeURIComponent(blueprintId)}`, {
          method: 'DELETE',
        })
        if (!res.ok) {
          toast.error(await readError(res, 'Failed to delete blueprint'))
          return false
        }
        return true
      } catch (err) {
        toast.error(
          `Failed to delete blueprint: ${err instanceof Error ? err.message : String(err)}`,
        )
        return false
      }
    },
    [template],
  )

  // Delete a prompt (split semantics: a mid-blueprint prompt fragments its
  // chain; the steps after it become a new untriggered blueprint, flagged by
  // `orphaned_blueprint`). Core does the request + error toast only; the caller
  // surfaces the orphaned toast + refetch so it composes inside a bulk batch.
  const deletePromptCore = useCallback(
    async (promptId: string): Promise<{ ok: boolean; orphaned: boolean }> => {
      try {
        const res = await fetch(`${promptsBase(template)}/${encodeURIComponent(promptId)}`, {
          method: 'DELETE',
        })
        if (!res.ok) {
          toast.error(await readError(res, 'Failed to delete prompt'))
          return { ok: false, orphaned: false }
        }
        const body = (await res.json().catch(() => null)) as { orphaned_blueprint?: boolean } | null
        return { ok: true, orphaned: !!body?.orphaned_blueprint }
      } catch (err) {
        toast.error(`Failed to delete prompt: ${err instanceof Error ? err.message : String(err)}`)
        return { ok: false, orphaned: false }
      }
    },
    [template],
  )

  // Whole-blueprint delete from the box details popup (kept as its own affordance
  // with the popup's inline two-stage confirm). Refetches on success.
  const deleteBlueprint = useCallback(
    async (blueprintId: string) => {
      if (!(await deleteBlueprintCore(blueprintId))) return
      setBoxMenu(null)
      setBoxDeleteConfirm(false)
      fetchAll()
    },
    [deleteBlueprintCore, fetchAll],
  )

  // Run a resolved deletion plan: blueprints first (their cascade removes member
  // prompts), then standalone prompts — grouped by blueprint and deleted
  // tail-first (descending step index) so deleting several steps of one blueprint
  // in a batch drops trailing steps without spawning intermediate untriggered
  // downstream blueprints (a head/mid delete orphans; a tail delete doesn't).
  // One refetch at the end reconciles the canvas with server state.
  const executeDeletePlan = useCallback(
    async (plan: DeletePlan) => {
      if (deletingRef.current) return
      deletingRef.current = true
      try {
        for (const bp of plan.blueprints) {
          await deleteBlueprintCore(bp.id)
        }
        const byBlueprint = new Map<string, DeletePlan['prompts']>()
        for (const p of plan.prompts) {
          const arr = byBlueprint.get(p.blueprintId)
          if (arr) arr.push(p)
          else byBlueprint.set(p.blueprintId, [p])
        }
        let orphaned = false
        for (const group of byBlueprint.values()) {
          for (const p of [...group].sort((a, b) => b.stepIndex - a.stepIndex)) {
            const r = await deletePromptCore(p.id)
            if (r.orphaned) orphaned = true
          }
        }
        if (orphaned) {
          toast.info('Steps after a deleted prompt are now an untriggered blueprint.')
        }
      } finally {
        deletingRef.current = false
        setDeleteConfirm(null)
        await fetchAll()
      }
    },
    [deleteBlueprintCore, deletePromptCore, fetchAll],
  )

  // Recompute the procedural boxes from the current member node geometry. A box
  // is drawn only at ≥2 steps (decision 3); it hugs its members with a header
  // gap on top, sits behind the nodes, and is click-through. Reads membership
  // from refs so it stays cheap + stale-free inside onNodesChange.
  const computeBoxNodes = useCallback(
    (nonBox: Node[]): Node[] => {
      const byNodeId = new Map<string, Node>()
      for (const n of nonBox) {
        if (n.type === 'prompt') byNodeId.set(n.id, n)
      }
      const boxes: Node[] = []
      for (const [bpId, promptIds] of Object.entries(blueprintStepsRef.current)) {
        if (promptIds.length < 2) continue
        let minX = Infinity
        let minY = Infinity
        let maxX = -Infinity
        let maxY = -Infinity
        let found = 0
        for (const pid of promptIds) {
          const node = byNodeId.get(`p:${pid}`)
          if (!node) continue
          found++
          const w = node.measured?.width ?? FALLBACK_NODE.width
          const h = node.measured?.height ?? FALLBACK_NODE.height
          const { x, y } = node.position
          if (x < minX) minX = x
          if (y < minY) minY = y
          if (x + w > maxX) maxX = x + w
          if (y + h > maxY) maxY = y + h
        }
        // Need at least two placed members for a meaningful box.
        if (found < 2) continue
        const width = maxX - minX + BOX_PAD.left + BOX_PAD.right
        const height = maxY - minY + BOX_PAD.top + BOX_PAD.bottom
        boxes.push({
          id: `bp:${bpId}`,
          type: 'blueprintBox',
          position: { x: minX - BOX_PAD.left, y: minY - BOX_PAD.top },
          data: {
            name: blueprintNamesRef.current[bpId] ?? 'Blueprint',
            blueprintId: bpId,
            stepCount: promptIds.length,
            onBoxEdit,
          },
          // Draggable so the box moves the whole blueprint (onNodesChange
          // translates the box's delta onto its members). Selectable + deletable
          // so it's a Delete-key target (→ whole-blueprint delete). Behind the
          // nodes (zIndex 0, not elevated on select — see elevateNodesOnSelect)
          // so a selected box never paints over its own prompts/edges.
          draggable: true,
          selectable: true,
          connectable: false,
          deletable: true,
          zIndex: 0,
          // We compute the exact rect, so hand ReactFlow the dimensions as
          // already-"measured". Otherwise it renders the node visibility:hidden
          // pending its own measure pass — which this onNodesChange box rebuild
          // discards every cycle, so the box would stay hidden forever.
          width,
          height,
          measured: { width, height },
          // pointer-events:none makes the wrapper (and thus the box interior)
          // click-through to the inner edges/prompts; the chrome elements in
          // BlueprintBoxNode re-enable events for drag + selection.
          style: { width, height, pointerEvents: 'none' },
        })
      }
      return boxes
    },
    [onBoxEdit],
  )

  // Rebuild nodes when data changes. Prompt nodes default to a left→right layout
  // in step order (so a chain reads naturally and the box hugs a row), stacked
  // per blueprint; saved positions always win. Boxes are prepended so that, at
  // equal z-index, they paint beneath the prompt nodes.
  useEffect(() => {
    const layout = layoutRef.current
    const promptMap = new Map(prompts.map((p): [string, Prompt] => [p.id, p]))

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
        // Events aren't duplication targets — only prompt (step) nodes are
        // selectable so a marquee/Ctrl+D never sweeps an event in. Still draggable.
        selectable: false,
        data: {
          label: et.label,
          source: et.source,
          description: et.description,
          onRemove: () => removeEvent(et.id),
        },
      })
      defaultY += 70
    }

    const promptNodes: Node[] = []
    const seen = new Set<string>()
    let row = 0
    for (const bp of blueprints) {
      const ids = blueprintSteps[bp.id] ?? []
      const baseY = 40 + row * 150
      ids.forEach((pid, i) => {
        if (seen.has(pid)) return
        seen.add(pid)
        const p = promptMap.get(pid)
        if (!p) return
        const def = { x: 560 + i * 300, y: baseY }
        const pos = layout.promptPositions[pid] || def
        if (!layout.promptPositions[pid]) {
          layout.promptPositions[pid] = pos
        }
        promptNodes.push({
          id: `p:${pid}`,
          type: 'prompt',
          position: pos,
          // Freshly-duplicated copies come back selected so a follow-up drag/wire
          // is immediate (the pending set is cleared once consumed, below).
          selected: pendingSelectRef.current.has(pid),
          data: {
            label: p.name,
            source: p.source,
            usageCount: p.usage_count,
            bodyPreview: p.body.slice(0, 80) + (p.body.length > 80 ? '...' : ''),
            onClick: () => onPromptClickRef.current?.(p.id),
          },
        })
      })
      row++
    }

    const nonBox = [...eventNodes, ...promptNodes]
    setNodes([...computeBoxNodes(nonBox), ...nonBox])
    // Pending selection is one-shot — consumed by this rebuild so a later,
    // unrelated fetch doesn't re-select stale ids.
    if (pendingSelectRef.current.size > 0) pendingSelectRef.current = new Set()
  }, [
    eventTypes,
    prompts,
    blueprints,
    blueprintSteps,
    activeEventIds,
    removeEvent,
    computeBoxNodes,
  ])

  // Build edges: event → entry prompt (trigger) and prompt → prompt (sequence).
  // Every edge is deletable:false — an edge isn't a first-class deletion target;
  // it's removed as a consequence of deleting its incident prompt (split) or
  // blueprint (cascade), or directly via the shift-click edge menu (split /
  // remove trigger). Marking them non-deletable keeps the Delete key (and any
  // node-incident edges ReactFlow auto-collects) from ever removing an edge.
  const edges: Edge[] = useMemo(() => {
    // Only wire to prompts that are actually on the canvas — a step whose prompt
    // is absent (e.g. mid-fetch or a deleted prompt) gets no dangling edge.
    const presentPrompts = new Set(prompts.map((p) => p.id))
    const triggerEdges: Edge[] = triggers
      // Drop triggers whose blueprint has no resolvable entry prompt so we never
      // emit an edge pointing at a node that isn't on the canvas.
      .filter(
        (t) =>
          activeEventIds.has(t.event_type) &&
          blueprintFirstPrompt[t.blueprint_id] &&
          presentPrompts.has(blueprintFirstPrompt[t.blueprint_id]),
      )
      .map((t) => ({
        id: t.id,
        source: `et:${t.event_type}`,
        target: `p:${blueprintFirstPrompt[t.blueprint_id]}`,
        type: 'default',
        animated: t.enabled,
        data: { kind: 'trigger' },
        deletable: false,
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

    const sequenceEdges: Edge[] = []
    for (const [bpId, ids] of Object.entries(blueprintSteps)) {
      for (let i = 0; i < ids.length - 1; i++) {
        if (!presentPrompts.has(ids[i]) || !presentPrompts.has(ids[i + 1])) continue
        sequenceEdges.push({
          id: `seq:${bpId}:${i}`,
          source: `p:${ids[i]}`,
          target: `p:${ids[i + 1]}`,
          // Smooth bezier (like the trigger edges), solid (not dashed) to read as
          // a committed step link rather than an unfired binding.
          type: 'default',
          // Disconnecting this edge splits the blueprint before step i+1.
          data: { kind: 'sequence', blueprintId: bpId, atStepIndex: i + 1 },
          deletable: false,
          style: { stroke: 'var(--color-text-tertiary)', strokeWidth: 1.5 },
          markerEnd: { type: MarkerType.ArrowClosed, color: 'var(--color-text-tertiary)' },
        })
      }
    }
    return [...sequenceEdges, ...triggerEdges]
  }, [triggers, activeEventIds, blueprintFirstPrompt, blueprintSteps, prompts])

  // Persist a reordered step list (drag-to-reorder within a box → PUT steps).
  const persistReorder = useCallback(
    async (blueprintId: string, orderedPromptIds: string[]) => {
      const briefByPrompt = new Map(
        (stepObjsRef.current[blueprintId] ?? []).map((s) => [s.step_prompt_id, s.brief]),
      )
      const steps = orderedPromptIds.map((pid) => ({
        step_prompt_id: pid,
        brief: briefByPrompt.get(pid) ?? '',
      }))
      try {
        const res = await fetch(
          `${blueprintsBase(template)}/${encodeURIComponent(blueprintId)}/steps`,
          {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ steps }),
          },
        )
        if (!res.ok) {
          toast.error(await readError(res, 'Failed to reorder steps'))
        }
      } catch (err) {
        toast.error(`Failed to reorder steps: ${err instanceof Error ? err.message : String(err)}`)
      } finally {
        // Re-render from the backend's truth either way (success applies the new
        // order; failure reverts to the persisted order).
        fetchAll()
      }
    },
    [template, fetchAll],
  )

  // After a drag ends, if the dragged prompt's blueprint members now sort into a
  // different order along the flow axis, persist the reorder. Spatial order is
  // the gesture: nudging without crossing a sibling is a no-op.
  const maybeReorder = useCallback(
    (nonBox: Node[], draggedNodeId: string) => {
      const pid = draggedNodeId.slice(2)
      const bp = promptToBlueprintRef.current[pid]
      if (!bp) return
      const stepIds = blueprintStepsRef.current[bp] ?? []
      if (stepIds.length < 2) return
      const members = stepIds.map((id) => nonBox.find((n) => n.id === `p:${id}`))
      if (members.some((n) => !n)) return
      const sorted = [...(members as Node[])].sort(
        (a, b) => a.position.x - b.position.x || a.position.y - b.position.y,
      )
      const newOrder = sorted.map((n) => n.id.slice(2))
      if (newOrder.join('|') === stepIds.join('|')) return
      persistReorder(bp, newOrder)
    },
    [persistReorder],
  )

  // Handle node changes (dragging) — apply, persist positions, recompute boxes,
  // and detect a reorder on drag-end.
  const onNodesChange = useCallback(
    (changes: NodeChange[]) => {
      // A blueprint box drags as a unit: translate its position delta onto its
      // member prompt nodes and drop the box's own change — the box is recomputed
      // from its members, so it re-hugs them at the new spot. The delta tracks
      // ReactFlow's drag exactly (box pos = minX(members) − pad), so no drift.
      // Members that already carry their own position change this batch (a
      // multi-selection that includes both the box and some of its prompts, where
      // ReactFlow moves each selected node directly) are skipped here so they
      // don't get the delta applied twice.
      const directlyMoved = new Set<string>()
      for (const c of changes) {
        if (c.type === 'position' && c.id.startsWith('p:')) directlyMoved.add(c.id)
      }
      const memberMoves: NodeChange[] = []
      for (const c of changes) {
        if (c.type === 'position' && c.id.startsWith('bp:') && c.position) {
          const box = nodesRef.current.find((n) => n.id === c.id)
          if (!box) continue
          const dx = c.position.x - box.position.x
          const dy = c.position.y - box.position.y
          if (dx === 0 && dy === 0) continue
          for (const pid of blueprintStepsRef.current[c.id.slice(3)] ?? []) {
            if (directlyMoved.has(`p:${pid}`)) continue
            const node = nodesRef.current.find((n) => n.id === `p:${pid}`)
            if (!node) continue
            memberMoves.push({
              id: `p:${pid}`,
              type: 'position',
              position: { x: node.position.x + dx, y: node.position.y + dy },
              dragging: c.dragging,
            })
          }
        }
      }
      const effective: NodeChange[] = [
        ...changes.filter((c) => !(c.type === 'position' && c.id.startsWith('bp:'))),
        ...memberMoves,
      ]
      const applied = applyNodeChanges(effective, nodesRef.current)

      // Persist position changes (events + prompts; box drags persist via the
      // synthesized member moves).
      const layout = layoutRef.current
      let dirty = false
      for (const change of effective) {
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

      // Recompute boxes from the updated member geometry. computeBoxNodes mints
      // fresh box nodes (geometry only), so carry over each box's selected state
      // from `applied` (which has any select change already applied) — otherwise
      // a box would deselect itself on the next drag/select cycle.
      const nonBox = applied.filter((n) => n.type !== 'blueprintBox')
      const boxSelected = new Map(
        applied.filter((n) => n.type === 'blueprintBox').map((n) => [n.id, !!n.selected]),
      )
      const boxes = computeBoxNodes(nonBox).map((b) =>
        boxSelected.get(b.id) ? { ...b, selected: true } : b,
      )
      const next = [...boxes, ...nonBox]
      nodesRef.current = next
      setNodes(next)

      // Reorder detection on drag-end — only a direct prompt drag can reorder; a
      // box drag moves every member by the same delta, so order is unchanged.
      for (const change of changes) {
        if (change.type === 'position' && change.dragging === false && change.id.startsWith('p:')) {
          maybeReorder(nonBox, change.id)
        }
      }
    },
    [computeBoxNodes, maybeReorder],
  )

  // Connect: route the gesture (decision 7 enforcement lives in planConnection).
  // event → entry prompt creates/updates a trigger; tail → trigger-less entry is
  // an atomic merge. Both re-render from the resulting membership.
  const onConnect = useCallback(
    async (connection: Connection) => {
      // Wait for the scope to resolve. For team scope before /api/teams loads,
      // teamId is '' and posting it would 400 (ambiguous); for template scope,
      // scopeReady gates on org-admin + multi. The gesture no-ops rather than
      // failing silently — the edge simply doesn't stick (edges derive from
      // backend state, not the live graph).
      if (!scopeReady) return

      const plan = planConnection(connection, {
        firstPrompt: firstPromptRef.current,
        steps: blueprintStepsRef.current,
        promptToBlueprint: promptToBlueprintRef.current,
        triggered: triggeredRef.current,
      })
      if (!plan.ok) {
        toast.error(plan.reason)
        return
      }

      if (plan.kind === 'trigger') {
        // Team scope stamps the acting team; template scope is org-scoped.
        const body: Record<string, unknown> = {
          kind: 'trigger',
          blueprint_id: plan.blueprintId,
          event_type: plan.eventType,
        }
        if (!template) body.team_id = teamId
        try {
          const res = await fetch(handlersBase(template), {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(body),
          })
          if (!res.ok) {
            toast.error(await readError(res, 'Failed to create trigger'))
            return
          }
          fetchAll()
        } catch (err) {
          toast.error(
            `Failed to create trigger: ${err instanceof Error ? err.message : String(err)}`,
          )
        }
        return
      }

      // Merge the trigger-less downstream chain onto this tail. The atomic
      // endpoint exists at both scopes (team + org template), so the URL is
      // scope-derived like every other call.
      try {
        const res = await fetch(
          `${blueprintsBase(template)}/${encodeURIComponent(plan.host)}/merge`,
          {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ source_blueprint_id: plan.source }),
          },
        )
        if (!res.ok) {
          toast.error(await readError(res, 'Failed to merge blueprints'))
          return
        }
        fetchAll()
      } catch (err) {
        toast.error(
          `Failed to merge blueprints: ${err instanceof Error ? err.message : String(err)}`,
        )
      }
    },
    [scopeReady, template, teamId, fetchAll],
  )

  // Deep-copy a flat set of prompt ids into new trigger-less blueprint(s) via the
  // duplicate endpoint, then place + select the copies and refetch. The endpoint
  // owns the grouping (induced contiguous runs → trigger-less blueprint copies),
  // so the canvas just resolves the selection to prompt ids and sends the set; the
  // returned blueprints come back in a deterministic order that orderSelectedPromptIds
  // mirrors, letting each copy land at its original's position + offset (relative
  // layout preserved, shifted as a group). Both scopes share the call — only the
  // REST root differs (blueprintsBase).
  const duplicatePromptIds = useCallback(
    async (promptIds: string[]) => {
      if (!scopeReady) return
      // Serialize duplications: a second Cmd+D / paste before the first fetchAll
      // resolves would race two refetches + pendingSelect writes (last-resolved
      // wins the selection). The guard makes the gesture a no-op while in flight.
      if (duplicatingRef.current) {
        toast.info('Duplication in progress…')
        return
      }
      const origOrdered = orderSelectedPromptIds(
        promptIds,
        blueprintStepsRef.current,
        promptToBlueprintRef.current,
      )
      if (origOrdered.length === 0) return
      duplicatingRef.current = true
      // Team scope stamps the acting team; template scope is org-scoped.
      const body: Record<string, unknown> = { prompt_ids: origOrdered }
      if (!template) body.team_id = teamId
      try {
        const res = await fetch(`${blueprintsBase(template)}/duplicate`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        })
        if (!res.ok) {
          toast.error(await readError(res, 'Failed to duplicate'))
          return
        }
        const created = (await res.json()) as { blueprint: Blueprint; steps: BlueprintStep[] }[]
        // Flatten the returned steps in the endpoint's order (blueprints as
        // returned, steps by step_index); this aligns 1:1 with origOrdered.
        const newOrdered: string[] = []
        for (const bp of created) {
          for (const s of [...bp.steps].sort((a, b) => a.step_index - b.step_index)) {
            newOrdered.push(s.step_prompt_id)
          }
        }
        // The position map relies on origOrdered[k] ↔ newOrdered[k]. A length
        // mismatch (e.g. a prompt deleted between copy and paste, so the endpoint
        // partitions a smaller set) means the tail copies fall back to the default
        // grid; warn rather than seed positions against misaligned indices.
        if (newOrdered.length !== origOrdered.length) {
          console.warn(
            `BindingGraph duplicate: expected ${origOrdered.length} copies, got ${newOrdered.length} — placement seeded only for the aligned prefix`,
          )
        }
        // Pre-seed each copy's layout position at its original + offset so it
        // renders offset (and distinct) on the refetch rather than at the default
        // grid spot. Original position comes from the live node (most current,
        // including unsaved drags), falling back to the persisted layout.
        const layout = layoutRef.current
        const n = Math.min(origOrdered.length, newOrdered.length)
        for (let k = 0; k < n; k++) {
          const origNode = nodesRef.current.find((nd) => nd.id === `p:${origOrdered[k]}`)
          const origPos = origNode?.position ?? layout.promptPositions[origOrdered[k]]
          if (origPos) {
            layout.promptPositions[newOrdered[k]] = {
              x: origPos.x + DUP_OFFSET,
              y: origPos.y + DUP_OFFSET,
            }
          }
        }
        saveLayout(storageKeyRef.current, layout)
        pendingSelectRef.current = new Set(newOrdered)
        await fetchAll()
        const count = newOrdered.length
        toast.success(`Duplicated ${count} prompt${count === 1 ? '' : 's'}`)
      } catch (err) {
        toast.error(`Failed to duplicate: ${err instanceof Error ? err.message : String(err)}`)
      } finally {
        duplicatingRef.current = false
      }
    },
    [scopeReady, template, teamId, fetchAll],
  )

  // The currently-selected prompt nodes' prompt ids (event nodes are
  // non-selectable, so a selection is always prompts).
  const selectedPromptIds = useCallback(
    () =>
      nodesRef.current.filter((n) => n.type === 'prompt' && n.selected).map((n) => n.id.slice(2)),
    [],
  )

  // Cmd/Ctrl+D — duplicate the selection in place. Returns whether it acted (so
  // the key handler only swallows the event when it did).
  const duplicateSelection = useCallback(() => {
    const ids = selectedPromptIds()
    if (ids.length === 0) return false
    void duplicatePromptIds(ids)
    return true
  }, [selectedPromptIds, duplicatePromptIds])

  // Cmd/Ctrl+C — snapshot the selected prompt ids into the in-canvas clipboard.
  const copySelection = useCallback(() => {
    const ids = selectedPromptIds()
    if (ids.length === 0) return false
    clipboardRef.current = { scopeKey: storageKeyRef.current, promptIds: ids }
    // "to canvas clipboard" — this is page-local, not the system clipboard, so a
    // Cmd+V in another app won't paste these.
    toast.success(`Copied ${ids.length} prompt${ids.length === 1 ? '' : 's'} to canvas clipboard`)
    return true
  }, [selectedPromptIds])

  // Cmd/Ctrl+V — paste the clipboard via the duplicate endpoint. Same-scope only:
  // a clipboard from another team/template is ignored.
  const pasteClipboard = useCallback(() => {
    const clip = clipboardRef.current
    if (!clip || clip.promptIds.length === 0) return false
    if (clip.scopeKey !== storageKeyRef.current) return false
    if (duplicatingRef.current) return false
    void duplicatePromptIds(clip.promptIds)
    return true
  }, [duplicatePromptIds])

  // Keyboard duplication gestures. Window-level (the canvas has no single focus
  // target), guarded so typing Cmd+C/D/V in an input/textarea/contenteditable —
  // e.g. the blueprint rename field — is left to the browser.
  useEffect(() => {
    const onKeyDown = (e: KeyboardEvent) => {
      if (!(e.metaKey || e.ctrlKey) || e.altKey) return
      // Ignore auto-repeat so holding the combo can't fire a burst of duplicates.
      if (e.repeat) return
      const t = e.target as HTMLElement | null
      if (
        t &&
        (t.tagName === 'INPUT' ||
          t.tagName === 'TEXTAREA' ||
          t.tagName === 'SELECT' ||
          t.isContentEditable)
      ) {
        return
      }
      // A live text selection (e.g. dragging across a prompt node's body) means
      // the user wants the browser's native copy/cut/paste — defer to it rather
      // than hijacking the combo to act on the node selection.
      const domSelection = window.getSelection()
      if (domSelection && !domSelection.isCollapsed && domSelection.toString().length > 0) {
        return
      }
      let handled = false
      switch (e.key.toLowerCase()) {
        case 'd':
          handled = duplicateSelection()
          break
        case 'c':
          handled = copySelection()
          break
        case 'v':
          handled = pasteClipboard()
          break
      }
      if (handled) e.preventDefault()
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [duplicateSelection, copySelection, pasteClipboard])

  const doDeleteTrigger = useCallback(
    async (triggerId: string) => {
      // Capture trigger info before deletion for the forgiving banner callback.
      const deleted = triggersRef.current.find((t) => t.id === triggerId)
      try {
        const res = await fetch(`${handlersBase(template)}/${encodeURIComponent(triggerId)}`, {
          method: 'DELETE',
        })
        if (!res.ok) {
          toast.error(await readError(res, 'Failed to remove trigger'))
          return
        }
        await fetchAll()
        // Notify parent so it can check coverage and show the forgiving banner.
        if (deleted) {
          onTriggerDeletedRef.current?.(deleted.event_type)
        }
      } catch (err) {
        toast.error(`Failed to remove trigger: ${err instanceof Error ? err.message : String(err)}`)
      }
    },
    [fetchAll, template],
  )

  // Split the blueprint before the given step index. Available at both scopes.
  const doSplit = useCallback(
    async (blueprintId: string, atStepIndex: number) => {
      try {
        const res = await fetch(
          `${blueprintsBase(template)}/${encodeURIComponent(blueprintId)}/split`,
          {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ at_step_index: atStepIndex }),
          },
        )
        if (!res.ok) {
          toast.error(await readError(res, 'Failed to split blueprint'))
          return
        }
        fetchAll()
      } catch (err) {
        toast.error(
          `Failed to split blueprint: ${err instanceof Error ? err.message : String(err)}`,
        )
      }
    },
    [template, fetchAll],
  )

  // Click an edge: plain click on a trigger edge opens its config; shift-click
  // any edge opens a contextual menu (trigger: edit/remove; sequence: split).
  const onEdgeClick: EdgeMouseHandler = useCallback((event, edge) => {
    const mouseEvent = event as unknown as MouseEvent
    const kind = (edge.data as { kind?: string } | undefined)?.kind
    if (kind === 'sequence') {
      if (event.shiftKey) {
        const data = edge.data as { blueprintId: string; atStepIndex: number }
        setEdgeMenu({
          x: mouseEvent.clientX,
          y: mouseEvent.clientY,
          kind: 'sequence',
          blueprintId: data.blueprintId,
          atStepIndex: data.atStepIndex,
        })
      }
      return
    }
    // Trigger edge — resolve the backing trigger by edge id.
    const trigger = triggersRef.current.find((t) => t.id === edge.id)
    if (!trigger) return
    if (event.shiftKey) {
      setEdgeMenu({ x: mouseEvent.clientX, y: mouseEvent.clientY, kind: 'trigger', trigger })
    } else {
      onTriggerClickRef.current?.(trigger)
    }
  }, [])

  // Delete key (and any future deleteElements call) routes through here. This is
  // the single async gate that sees the whole selection at once: it resolves the
  // selected nodes into a typed DeletePlan, runs it immediately for a single
  // prompt, or stages the confirm dialog for a blueprint / bulk selection — then
  // ALWAYS returns false so ReactFlow never optimistically removes anything. The
  // canvas is derived from server state, so the plan's API calls + the refetch
  // in executeDeletePlan are what actually update it. Edges are deletable:false
  // and never reach here as targets; ReactFlow does fold node-incident edges into
  // the payload, but we dispatch by node and ignore those (the prompt/blueprint
  // delete already removes the edge server-side) — that's the redundant-split
  // guard, by construction.
  const onBeforeDelete = useCallback(
    async ({ nodes }: { nodes: Node[]; edges: Edge[] }): Promise<boolean> => {
      if (deletingRef.current) return false
      const blueprintIds = new Set<string>()
      const blueprints: DeletePlan['blueprints'] = []
      for (const n of nodes) {
        if (!n.id.startsWith('bp:')) continue
        const id = n.id.slice(3)
        if (blueprintIds.has(id)) continue
        blueprintIds.add(id)
        blueprints.push({ id, name: (n.data as { name?: string }).name ?? 'Blueprint' })
      }
      const prompts: DeletePlan['prompts'] = []
      for (const n of nodes) {
        if (!n.id.startsWith('p:')) continue
        const pid = n.id.slice(2)
        const bp = promptToBlueprintRef.current[pid]
        // Skip a prompt whose blueprint is also being deleted — that delete
        // cascades its copy-only step prompts, so deleting the prompt separately
        // would race / 404.
        if (bp && blueprintIds.has(bp)) continue
        const stepIndex = bp ? (blueprintStepsRef.current[bp] ?? []).indexOf(pid) : -1
        prompts.push({
          id: pid,
          name: (n.data as { label?: string }).label ?? 'this prompt',
          blueprintId: bp ?? '',
          stepIndex,
        })
      }
      const plan: DeletePlan = { blueprints, prompts }
      const total = blueprints.length + prompts.length
      if (total === 0) return false
      // Per the project decision: any blueprint deletion, or any bulk (>1)
      // deletion, confirms first; a single prompt deletes immediately.
      if (blueprints.length > 0 || total > 1) {
        setDeleteConfirm(plan)
      } else {
        void executeDeletePlan(plan)
      }
      return false
    },
    [executeDeletePlan],
  )

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

  // Resolve the blueprint + its trigger for the details popup at click time so
  // it always reflects the latest fetch.
  const boxMenuTrigger = boxMenu
    ? triggers.find((t) => t.blueprint_id === boxMenu.blueprintId)
    : undefined

  const deleteSummary = deleteConfirm ? summarizeDeletePlan(deleteConfirm) : ''

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
        onBeforeDelete={onBeforeDelete}
        fitView
        fitViewOptions={{ padding: 0.4 }}
        proOptions={{ hideAttribution: true }}
        minZoom={0.4}
        maxZoom={1.5}
        defaultEdgeOptions={{ type: 'default' }}
        // Multi-select prompt nodes + blueprint boxes: Shift+drag the pane for a
        // marquee, Shift/Cmd/Ctrl-click to add to the selection. Plain drag still
        // pans (panOnDrag defaults to true, which also makes an explicit
        // selectionOnDrag a no-op — see the xyflow precedence — so the selection
        // key is what drives the marquee). The selected set backs Cmd/Ctrl+D and
        // C/V (see the keydown effect) and the Delete key (see onBeforeDelete).
        selectionKeyCode="Shift"
        multiSelectionKeyCode={['Meta', 'Control', 'Shift']}
        // Delete/Backspace removes the selection; onBeforeDelete intercepts it,
        // dispatches the right per-type backend op, and returns false so nothing
        // is removed optimistically (the canvas re-derives from the refetch).
        deleteKeyCode={['Delete', 'Backspace']}
        // Don't elevate a selected node's z-index — the blueprint box lives at
        // zIndex 0 behind its prompts/edges, and elevating it on selection would
        // float it over them, swallowing their clicks.
        elevateNodesOnSelect={false}
      >
        <Background color="var(--color-border-subtle)" gap={20} size={1} />
      </ReactFlow>

      {/* Edge context menu (shift-click) */}
      {edgeMenu && (
        <>
          <div className="fixed inset-0 z-50" onClick={() => setEdgeMenu(null)} />
          <div
            className="fixed z-50 bg-surface-raised/95 backdrop-blur-xl border border-border-glass rounded-xl shadow-xl shadow-black/10 p-1.5 w-[200px]"
            style={{
              left: Math.max(8, Math.min(edgeMenu.x - 100, window.innerWidth - 208)),
              top: edgeMenu.y - 12,
            }}
          >
            {edgeMenu.kind === 'trigger' ? (
              <>
                <button
                  onClick={() => {
                    onTriggerClickRef.current?.(edgeMenu.trigger)
                    setEdgeMenu(null)
                  }}
                  className="w-full text-left text-[12px] text-text-secondary hover:text-text-primary hover:bg-black/[0.04] font-medium px-2.5 py-1.5 rounded-lg transition-colors"
                >
                  Edit trigger config
                </button>
                <button
                  onClick={() => {
                    doDeleteTrigger(edgeMenu.trigger.id)
                    setEdgeMenu(null)
                  }}
                  className="w-full text-left text-[12px] text-red-500 hover:bg-red-50 font-medium px-2.5 py-1.5 rounded-lg transition-colors"
                >
                  Remove trigger
                </button>
              </>
            ) : (
              <>
                <div className="px-2.5 pt-1.5 pb-1 text-[10px] text-text-tertiary leading-snug">
                  Split into two blueprints at this boundary.
                </div>
                <button
                  onClick={() => {
                    doSplit(edgeMenu.blueprintId, edgeMenu.atStepIndex)
                    setEdgeMenu(null)
                  }}
                  className="w-full text-left text-[12px] text-text-secondary hover:text-text-primary hover:bg-black/[0.04] font-medium px-2.5 py-1.5 rounded-lg transition-colors"
                >
                  Split here
                </button>
              </>
            )}
          </div>
        </>
      )}

      {/* Blueprint details popup (box edit button) */}
      {boxMenu && (
        <>
          <div
            className="fixed inset-0 z-50"
            onClick={() => {
              setBoxMenu(null)
              setBoxDeleteConfirm(false)
            }}
          />
          <div
            className="fixed z-50 bg-surface-raised/95 backdrop-blur-xl border border-border-glass rounded-xl shadow-xl shadow-black/10 p-3 w-[260px]"
            style={{
              left: Math.max(8, Math.min(boxMenu.x - 240, window.innerWidth - 268)),
              top: boxMenu.y + 6,
            }}
          >
            {/* Rename — the blueprint's name is its own, not the entry prompt's. */}
            <label className="flex items-center gap-1.5 text-[10px] font-semibold uppercase tracking-wider text-text-tertiary mb-1">
              <Layers size={11} className="shrink-0" />
              Blueprint name
            </label>
            <div className="flex items-center gap-1.5 mb-2.5">
              <input
                autoFocus
                value={boxNameDraft}
                onChange={(e) => setBoxNameDraft(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter') saveBoxName(boxMenu.blueprintId, boxNameDraft)
                }}
                className="flex-1 min-w-0 px-2 py-1 rounded-md border border-border-subtle bg-white/60 text-[12px] text-text-primary focus:outline-none focus:border-accent/40 focus:ring-1 focus:ring-accent/20 transition-colors"
              />
              <button
                onClick={() => saveBoxName(boxMenu.blueprintId, boxNameDraft)}
                disabled={
                  !boxNameDraft.trim() ||
                  boxNameDraft.trim() === (blueprintNames[boxMenu.blueprintId] ?? '')
                }
                className="text-[11px] font-semibold text-white bg-accent hover:bg-accent/90 disabled:opacity-40 px-2.5 py-1 rounded-md transition-colors"
              >
                Save
              </button>
            </div>
            <div className="text-[11px] text-text-tertiary mb-2.5">
              {(blueprintSteps[boxMenu.blueprintId]?.length ?? 0) + ' steps · composed on canvas'}
            </div>
            {/* Duplicate the whole blueprint — passes all its step prompt ids to the
                same endpoint, yielding a trigger-less full copy. Disabled for a
                step-less blueprint (shouldn't exist, but reachable on stale data),
                where there'd be nothing to copy and the gesture would silently no-op. */}
            <button
              onClick={() => {
                const ids = blueprintSteps[boxMenu.blueprintId] ?? []
                setBoxMenu(null)
                void duplicatePromptIds(ids)
              }}
              disabled={(blueprintSteps[boxMenu.blueprintId]?.length ?? 0) === 0}
              className="w-full flex items-center gap-1.5 text-left text-[12px] text-text-secondary hover:text-text-primary hover:bg-black/[0.04] disabled:opacity-40 disabled:hover:bg-transparent disabled:hover:text-text-secondary disabled:cursor-not-allowed font-medium px-2.5 py-1.5 rounded-lg border border-border-subtle transition-colors mb-1.5"
            >
              <Copy size={12} className="shrink-0" />
              Duplicate blueprint
            </button>
            {boxMenuTrigger ? (
              <button
                onClick={() => {
                  onTriggerClickRef.current?.(boxMenuTrigger)
                  setBoxMenu(null)
                }}
                className="w-full text-left text-[12px] text-text-secondary hover:text-text-primary hover:bg-black/[0.04] font-medium px-2.5 py-1.5 rounded-lg border border-border-subtle transition-colors"
              >
                Edit trigger config
              </button>
            ) : (
              <p className="text-[11px] text-text-tertiary leading-snug">
                No trigger yet — wire an event to this blueprint’s first step to fire it.
              </p>
            )}
            {/* Delete the whole blueprint — soft-deletes the box + its step
                prompts and detaches the trigger server-side; the canvas refetch
                drops the box and its nodes. Two-stage so an accidental click in
                the rename/duplicate menu can't destroy a blueprint: the confirm
                names it and how many steps go with it. */}
            <div className="mt-2.5 pt-2.5 border-t border-border-subtle">
              {boxDeleteConfirm ? (
                <div className="space-y-1.5">
                  <p className="text-[11px] text-text-secondary leading-snug">
                    Delete{' '}
                    <span className="font-semibold text-text-primary">
                      {blueprintNames[boxMenu.blueprintId] ?? 'this blueprint'}
                    </span>{' '}
                    and its {blueprintSteps[boxMenu.blueprintId]?.length ?? 0} step
                    {(blueprintSteps[boxMenu.blueprintId]?.length ?? 0) === 1 ? '' : 's'}? This
                    can’t be undone.
                  </p>
                  <div className="flex items-center gap-1.5">
                    <button
                      onClick={() => setBoxDeleteConfirm(false)}
                      className="flex-1 text-[11px] font-medium text-text-secondary hover:text-text-primary px-2.5 py-1 rounded-md border border-border-subtle transition-colors"
                    >
                      Cancel
                    </button>
                    <button
                      onClick={() => void deleteBlueprint(boxMenu.blueprintId)}
                      className="flex-1 text-[11px] font-semibold text-white bg-red-500 hover:bg-red-600 px-2.5 py-1 rounded-md transition-colors"
                    >
                      Delete
                    </button>
                  </div>
                </div>
              ) : (
                <button
                  onClick={() => setBoxDeleteConfirm(true)}
                  className="w-full flex items-center gap-1.5 text-left text-[12px] text-text-tertiary hover:text-red-500 font-medium px-2.5 py-1.5 rounded-lg transition-colors"
                >
                  <Trash2 size={12} className="shrink-0" />
                  Delete blueprint
                </button>
              )}
            </div>
          </div>
        </>
      )}

      {/* Bulk / blueprint delete confirmation (Delete key). Names everything the
          plan removes; a single prompt delete skips this and runs immediately. */}
      {deleteConfirm && (
        <>
          <div
            className="fixed inset-0 z-[60] bg-black/20"
            onClick={() => setDeleteConfirm(null)}
          />
          <div
            role="dialog"
            aria-modal="true"
            aria-label={`Delete ${deleteSummary}?`}
            className="fixed z-[60] left-1/2 top-1/2 -translate-x-1/2 -translate-y-1/2 w-[360px] bg-surface-raised/95 backdrop-blur-xl border border-border-glass rounded-xl shadow-xl shadow-black/10 p-4"
            onClick={(e) => e.stopPropagation()}
          >
            <div className="flex items-center gap-2 mb-2.5">
              <Trash2 size={14} className="text-red-500 shrink-0" />
              <h3 className="text-[13px] font-semibold text-text-primary">
                Delete {deleteSummary}?
              </h3>
            </div>
            <ul className="text-[12px] text-text-secondary space-y-1 mb-3 max-h-[180px] overflow-y-auto">
              {deleteConfirm.blueprints.map((b) => (
                <li key={`bp:${b.id}`} className="flex items-center gap-1.5">
                  <Layers size={11} className="text-text-tertiary shrink-0" />
                  <span className="truncate">
                    <span className="font-medium text-text-primary">{b.name}</span>
                    <span className="text-text-tertiary"> — blueprint &amp; its steps</span>
                  </span>
                </li>
              ))}
              {deleteConfirm.prompts.map((p) => (
                <li key={`p:${p.id}`} className="flex items-center gap-1.5 pl-[18px]">
                  <span className="truncate">
                    <span className="font-medium text-text-primary">{p.name}</span>
                    <span className="text-text-tertiary"> — prompt</span>
                  </span>
                </li>
              ))}
            </ul>
            <p className="text-[11px] text-text-tertiary leading-snug mb-3">
              {deleteConfirm.prompts.length > 0 &&
                'Deleting a prompt mid-blueprint splits the steps after it into a new untriggered blueprint. '}
              This can’t be undone.
            </p>
            <div className="flex items-center justify-end gap-1.5">
              <button
                onClick={() => setDeleteConfirm(null)}
                className="text-[11px] font-medium text-text-secondary hover:text-text-primary px-3 py-1.5 rounded-md border border-border-subtle transition-colors"
              >
                Cancel
              </button>
              <button
                onClick={() => void executeDeletePlan(deleteConfirm)}
                className="text-[11px] font-semibold text-white bg-red-500 hover:bg-red-600 px-3 py-1.5 rounded-md transition-colors"
              >
                Delete
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
