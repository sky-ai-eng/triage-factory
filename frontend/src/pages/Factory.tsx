import { Tooltip } from '../ui/tooltip/Tooltip'
import {
  DndContext,
  DragOverlay,
  PointerSensor,
  useDraggable,
  useDroppable,
  useSensor,
  useSensors,
  type DragEndEvent,
  type DragStartEvent,
} from '@dnd-kit/core'
import { Clapperboard } from 'lucide-react'
import { useCallback, useEffect, useId, useMemo, useRef, useState } from 'react'
import PromptPicker from '../components/PromptPicker'
import TeamScopeSelect from '../components/TeamScopeSelect'
import { toast } from '../components/Toast/toastStore'
import { createIsoScene, type ClickedStationInfo, type IsoSceneHandle } from '../factory/iso-scene'
import { useWebSocket } from '../hooks/useWebSocket'
import { useOrgHref } from '../hooks/useOrgHref'
import { useImmersive } from '../contexts/ChromeContext'
import {
  useTeams,
  useTeamFilter,
  pickerDefault,
  noteWrittenTeam,
  teamFilterQuery,
} from '../hooks/useTeams'
import type {
  Conversation,
  FactoryEntity,
  FactorySnapshot,
  ConversationStatusValue,
  Task,
} from '../types'
import { isActiveStatus } from '../lib/conversationStatus'
import { apiErrors, apiJSON, httpErrorMessage } from '../lib/apiClient'

// Production factory page — Babylon scene driven by /api/factory/snapshot.
// The page itself does almost nothing visual: it fetches the snapshot,
// hands it to the scene, and pipes WS frames into a debounced refetch.
//
// All entity placement (which station they're parked at, whether
// they're mid-flight on a bridge and how far along) is derived inside
// the scene by a per-frame reconciler reading the snapshot's
// `current_event_type` + `last_event_at` + `recent_events`. The
// backend is therefore authoritative for both station tray counts and
// chip positions; the same projection feeds both, so they cannot drift.
// See `factory/place-entity.ts` for the projection function.

// Debounce window for snapshot refetches. The router publishes one
// `event` WS frame per detection plus a batched `tasks_updated` per
// poll cycle; collapsing those into one /api/factory/snapshot call
// keeps server load down. Kept short so chip motion appears promptly
// after the WS event lands — the snapshot determines when a transit
// becomes visible (it carries the new last_event_at), and a long
// debounce delays that.
const REFETCH_DEBOUNCE_MS = 250

// Drop target ID for the conversations tray inside the station drawer. A
// constant string is fine — the drawer only renders one station at a
// time, so there's never more than one drop target on the page.
const RUNS_DROP_ID = 'factory-runs-drop'

// Cinematic auto-attract: idle this long (ms) with the factory tab
// focused and nothing modal open → the camera engages itself (kiosk /
// wall-display attract-mode). Checked once a second.
const CINEMATIC_IDLE_MS = 45_000
const CINEMATIC_IDLE_CHECK_MS = 1_000

// prefers-reduced-motion gates auto-attract off entirely and tells the
// scene director to collapse to a single slow drift. Read live so a
// mid-session OS setting change is respected.
function prefersReducedMotion(): boolean {
  return (
    typeof window !== 'undefined' &&
    typeof window.matchMedia === 'function' &&
    window.matchMedia('(prefers-reduced-motion: reduce)').matches
  )
}

// In-flight delegate request: dropping a queued entity onto the runs
// tray populates this; the prompt picker reads it; on prompt selection
// it's POSTed to /api/tasks and then /api/tasks/{id}/delegate.
// Cleared on close/cancel.
interface PendingDelegate {
  entity: FactoryEntity
  eventType: string
  dedupKey: string
}

export default function Factory() {
  const containerRef = useRef<HTMLDivElement>(null)
  const sceneRef = useRef<IsoSceneHandle | null>(null)
  const [picked, setPicked] = useState<ClickedStationInfo | null>(null)
  const [pendingDelegate, setPendingDelegate] = useState<PendingDelegate | null>(null)
  const [draggingEntity, setDraggingEntity] = useState<FactoryEntity | null>(null)
  const [cinematic, setCinematic] = useState(false)

  // Ask the frame to fade with the rest of the HUD. Declarative rather than a
  // call in enterCinematic/exitCinematic: navigating away mid-cinematic would
  // otherwise leave the app with no rail and no explanation.
  useImmersive(cinematic)

  // multi-team. teamFilter is the per-page read scope (narrows the
  // entity belt via ?team_id); delegateTeam is the acting team the
  // drag-to-station delegate stamps onto the synthesized task. Both
  // render their selectors only at ≥2 teams.
  const { teams, lastActingTeamId, multi: multiTeam, loaded: teamsLoaded } = useTeams()
  const [teamFilter, setTeamFilter] = useTeamFilter('factory')
  const teamFilterRef = useRef(teamFilter)
  useEffect(() => {
    teamFilterRef.current = teamFilter
  }, [teamFilter])
  const [delegateTeam, setDelegateTeam] = useState('')
  // Seed the acting team only once /api/teams has loaded — until then the
  // picker is hidden (multi=false) and delegateTeam='' would post a blank
  // team_id in the cold-load window. The PromptPicker's own loading prop
  // (below) keeps tiles unclickable until this resolves.
  useEffect(() => {
    if (!teamsLoaded) return
    // eslint-disable-next-line react-hooks/set-state-in-effect
    setDelegateTeam(pickerDefault(teams, lastActingTeamId))
  }, [teamsLoaded, teams, lastActingTeamId])

  useEffect(() => {
    const container = containerRef.current
    if (!container) return
    let cancelled = false
    let unsubClick: (() => void) | null = null
    let unsubEnd: (() => void) | null = null
    createIsoScene(container).then((scene) => {
      if (cancelled) {
        scene.destroy()
        return
      }
      sceneRef.current = scene
      unsubClick = scene.onStationClick(setPicked)
      // The director eases the camera back to the pre-entry pose on exit
      // and fires onEnd only once that restore completes — so we restore
      // the HUD/catcher then, never mid-move, and never while the camera
      // controls are still detached.
      unsubEnd = scene.onCinematicEnd(() => setCinematic(false))
    })
    return () => {
      cancelled = true
      unsubClick?.()
      unsubEnd?.()
      sceneRef.current?.destroy()
      sceneRef.current = null
    }
  }, [])

  // Snapshot loader. The window-level callback is what the WS
  // effect (next block) calls to schedule a debounced refetch
  // without forcing this effect's identity to change on every render.
  useEffect(() => {
    let cancelled = false
    let pending: ReturnType<typeof setTimeout> | null = null

    const load = () => {
      const q = teamFilterQuery(teamFilterRef.current)
      apiJSON<FactorySnapshot>('/api/factory/snapshot' + (q ? `?${q}` : ''))
        .then((data) => {
          if (cancelled) return
          sceneRef.current?.applySnapshot(data)
        })
        .catch((err) => {
          if (cancelled) return
          console.warn('[factory] snapshot load failed:', err)
        })
    }

    load()

    const schedule = () => {
      if (pending) return
      pending = setTimeout(() => {
        pending = null
        load()
      }, REFETCH_DEBOUNCE_MS)
    }

    ;(window as unknown as { __factoryRefetch?: () => void }).__factoryRefetch = schedule

    return () => {
      cancelled = true
      if (pending) clearTimeout(pending)
      delete (window as unknown as { __factoryRefetch?: () => void }).__factoryRefetch
    }
  }, [])

  // WS frames are refetch hints only. Backend authority means we
  // don't try to drive any chip or count update directly from the
  // event payload — the next snapshot carries the same information
  // in a form that's coherent with everything else on the floor.
  useWebSocket((evt) => {
    if (
      evt.type === 'event' ||
      evt.type === 'tasks_updated' ||
      evt.type === 'conversation_update' ||
      evt.type === 'artifact_updated'
    ) {
      const refetch = (window as unknown as { __factoryRefetch?: () => void }).__factoryRefetch
      refetch?.()
    }
  })

  // Re-snapshot when the team filter changes — load() reads the latest
  // filter via its ref, so the scheduled refetch picks up the new scope.
  useEffect(() => {
    const refetch = (window as unknown as { __factoryRefetch?: () => void }).__factoryRefetch
    refetch?.()
  }, [teamFilter])

  // Esc closes the drawer — common video-game-y dismiss gesture.
  useEffect(() => {
    if (!picked) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setPicked(null)
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [picked])

  // While the drawer is open for a station, mirror live changes from
  // the scene reconciler — entities entering or leaving the queue,
  // runs starting or completing. The scene fires only on real
  // content changes (per-station hash dedup), so this won't churn
  // re-renders on idle frames. Re-subscribes on station change;
  // unsubscribes when the drawer closes.
  const pickedId = picked?.id
  useEffect(() => {
    if (!pickedId) return
    const scene = sceneRef.current
    if (!scene) return
    return scene.onStationDataChange(pickedId, (info) => {
      setPicked(info)
    })
  }, [pickedId])

  // ─── Cinematic mode ───────────────────────────────────────────────
  // A camera-driven ambient showcase: the scene director takes over the
  // camera (factory/cinematic.ts) while React drops the HUD and watches
  // for the exit gesture. Entry is the clapperboard button or an idle
  // auto-attract.
  const enterCinematic = useCallback(() => {
    const scene = sceneRef.current
    if (!scene) return
    setPicked(null) // close any open drawer so it slides away with the HUD
    scene.enterCinematic(prefersReducedMotion())
    setCinematic(true)
  }, [])

  const exitCinematic = useCallback(() => {
    // Only kick off the eased restore here — the `cinematic` flag is
    // cleared by onCinematicEnd (scene-creation effect) once the camera
    // has fully glided home, so the HUD/catcher stay up through the move
    // and controls aren't live while still detached.
    sceneRef.current?.exitCinematic()
  }, [])

  // While cinematic, any key or wheel exits (click is handled by the
  // full-viewport catcher in the render). pointermove deliberately does
  // NOT exit — a jittery wall-display mouse shouldn't yank the camera.
  useEffect(() => {
    if (!cinematic) return
    const onKey = () => exitCinematic()
    const onWheel = () => exitCinematic()
    window.addEventListener('keydown', onKey)
    window.addEventListener('wheel', onWheel, { passive: true })
    return () => {
      window.removeEventListener('keydown', onKey)
      window.removeEventListener('wheel', onWheel)
    }
  }, [cinematic, exitCinematic])

  // Idle auto-attract. A 1Hz tick checks time since the last real
  // interaction; once it crosses the threshold — tab visible, reduced-
  // motion off, nothing modal open — the camera engages itself. The
  // guard snapshot keeps the interval closure cheap and current.
  // Seeded in the effect below (not here) — Date.now() during render is
  // impure and the idle clock should start at mount anyway.
  const lastInteractionRef = useRef(0)
  const cinematicGuardsRef = useRef({ active: false, busy: false })
  useEffect(() => {
    cinematicGuardsRef.current = {
      active: cinematic,
      busy: picked != null || pendingDelegate != null || draggingEntity != null,
    }
  }, [cinematic, picked, pendingDelegate, draggingEntity])

  useEffect(() => {
    lastInteractionRef.current = Date.now() // start the idle clock at mount
    const bump = () => {
      lastInteractionRef.current = Date.now()
    }
    const events: (keyof WindowEventMap)[] = ['pointermove', 'pointerdown', 'keydown', 'wheel']
    for (const e of events) window.addEventListener(e, bump, { passive: true })
    document.addEventListener('visibilitychange', bump)
    const id = window.setInterval(() => {
      if (prefersReducedMotion()) return
      if (document.visibilityState !== 'visible') return
      const g = cinematicGuardsRef.current
      if (g.active || g.busy) return
      if (Date.now() - lastInteractionRef.current < CINEMATIC_IDLE_MS) return
      enterCinematic()
    }, CINEMATIC_IDLE_CHECK_MS)
    return () => {
      for (const e of events) window.removeEventListener(e, bump)
      document.removeEventListener('visibilitychange', bump)
      window.clearInterval(id)
    }
  }, [enterCinematic])

  // 8px activation distance keeps click-to-open-PR (single click on a
  // queued row's anchor) distinct from a drag — same threshold pattern
  // Board.tsx uses for its task-card sortable.
  const sensors = useSensors(useSensor(PointerSensor, { activationConstraint: { distance: 8 } }))

  const stationEventType = picked?.id ?? null
  // Stable identity across renders so the useCallback dependency lists
  // below don't churn — the reconciler hands us a fresh ClickedStationInfo
  // on every change but the derived array shouldn't trigger re-creation
  // of the drag callbacks unless `picked` itself changed.
  const queuedEntities = useMemo(() => picked?.queued ?? [], [picked])

  const onDragStart = useCallback(
    (evt: DragStartEvent) => {
      const id = String(evt.active.id)
      const entity = queuedEntities.find((e) => e.id === id) ?? null
      setDraggingEntity(entity)
    },
    [queuedEntities],
  )

  const onDragEnd = useCallback(
    (evt: DragEndEvent) => {
      setDraggingEntity(null)
      if (!evt.over || evt.over.id !== RUNS_DROP_ID) return
      if (!stationEventType) return
      const entity = queuedEntities.find((e) => e.id === String(evt.active.id))
      if (!entity) return
      const dedupKey = entity.pending_tasks?.[stationEventType]?.[0]?.dedup_key ?? ''
      setPendingDelegate({ entity, eventType: stationEventType, dedupKey })
    },
    [queuedEntities, stationEventType],
  )

  const handlePromptSelected = useCallback(
    async (promptId: string) => {
      const pd = pendingDelegate
      setPendingDelegate(null)
      if (!pd) return
      // The drop is two writes: resolve the task at this station, then
      // delegate it. Three failure modes to surface, and which call raised one
      // doesn't change how it reads to the user:
      //   - Network error (fetch throws) — caught below.
      //   - Pre-claim rejection — input/state validation failures (entity not
      //     found, no matching event, viewer) as 400/403/404/409/422. Nothing
      //     landed on the claim axis; toast and stop.
      //   - Post-claim spawn failure — reason SPAWN_FAILED (422 bad blueprint
      //     reference, 500 spawn/DB fault) AFTER the claim stamped. The task is
      //     bot-claimed with no run, so refetch (the bot-claimed card must
      //     surface immediately) and tell the user to retry. The reason is the
      //     discriminator — pre-claim faults also answer 500, so status alone
      //     can't tell the two apart.
      const label = entityLabel(pd.entity)
      try {
        const task = await apiJSON<{ id: string }>('/api/tasks', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            entity_id: pd.entity.id,
            event_type: pd.eventType,
            dedup_key: pd.dedupKey,
            team_id: delegateTeam,
          }),
        })
        await apiJSON<{ conversation_id: string }>(
          `/api/tasks/${encodeURIComponent(task.id)}/delegate`,
          {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ blueprint_id: promptId }),
          },
        )
      } catch (err) {
        const spawnFailed = apiErrors(err).some((it) => it.reason === 'SPAWN_FAILED')
        if (spawnFailed) {
          if (delegateTeam) noteWrittenTeam(delegateTeam)
          const refetch = (window as unknown as { __factoryRefetch?: () => void }).__factoryRefetch
          refetch?.()
          toast.error(
            `${label} is bot-claimed but the run didn't start: ${httpErrorMessage(err, 'spawn failed')}. Retry from the Board.`,
            'Delegate run failed',
          )
        } else {
          toast.error(
            `Delegate ${label}: ${httpErrorMessage(err, 'the delegation could not be started.')}`,
            'Delegation failed',
          )
        }
        return
      }
      if (delegateTeam) noteWrittenTeam(delegateTeam)
      const refetch = (window as unknown as { __factoryRefetch?: () => void }).__factoryRefetch
      refetch?.()
    },
    [pendingDelegate, delegateTeam],
  )

  return (
    <DndContext sensors={sensors} onDragStart={onDragStart} onDragEnd={onDragEnd}>
      {/* The HUD layer. Sized to the page area — inside the rail and below the
          header — so every control below sits clear of the chrome, the way it
          would on any other page. */}
      <div className="relative -mx-8 -my-8 h-[calc(100%+4rem)] overflow-hidden">
        {/* The world. Fixed to the VIEWPORT, not to the page area, and it is the
            only element here that is.

            The rail animates its width over 220ms. A canvas sized to the page
            area follows that, which means a ResizeObserver firing every frame
            of the transition and engine.resize() reallocating the framebuffer
            each time — the scene visibly tearing down and coming back. Sizing
            it to the window instead means the rail moving is simply not an
            event the scene can observe: no resize, no reallocation, nothing to
            flash.

            It also reads better. This is the most game-like surface in the
            product, and in a game the world does not disappear when the HUD
            changes — the HUD is drawn over a world that was already there. The
            rail and header now pass over the scene rather than displacing it,
            which costs the pixels they cover and buys a world that holds
            still. */}
        <div ref={containerRef} className="fixed inset-0 z-0" />
        {/* Per-page team filter — narrows the entity belt.
            Gated on ≥2 teams so solo users get no empty overlay box. */}
        {teams.length >= 2 && (
          <div
            className={`absolute top-4 left-4 rounded-md bg-raised px-1 py-0.5 shadow transition-opacity duration-[var(--dur-content)] ${
              cinematic ? 'pointer-events-none opacity-0' : 'opacity-100'
            }`}
          >
            <TeamScopeSelect value={teamFilter} onChange={setTeamFilter} />
          </div>
        )}
        <div
          className={`absolute right-4 bottom-4 flex items-center gap-2 transition-opacity duration-[var(--dur-content)] ${
            cinematic ? 'pointer-events-none opacity-0' : 'opacity-100'
          }`}
        >
          <button
            type="button"
            onClick={enterCinematic}
            title="Cinematic mode"
            aria-label="Cinematic mode"
            className="rounded-md bg-raised p-2 text-ink-1 shadow transition hover:bg-sunk"
          >
            <Clapperboard className="h-4 w-4" strokeWidth={2} aria-hidden />
          </button>
          <button
            type="button"
            onClick={() => sceneRef.current?.resetView()}
            className="rounded-md bg-raised px-3 py-2 text-reported font-semibold text-ink-1 shadow transition hover:bg-sunk"
          >
            Reset view
          </button>
        </div>
        <StationDrawer info={picked} />
        {/* Exit catcher — swallows the first click (so it exits instead of
            orbiting). The cursor stays visible; key/wheel exits are wired in an
            effect.

            Fixed, not absolute: the world now fills the window and the chrome
            has faded off it, so the whole window is scene. An absolute catcher
            covered only the page area, which left the strip where the rail used
            to be as the one place a click did nothing. */}
        {cinematic && (
          <div className="fixed inset-0 z-40" onPointerDown={exitCinematic} aria-hidden />
        )}
      </div>
      <DragOverlay dropAnimation={null}>
        {draggingEntity ? (
          <div
            className="rounded-md px-2.5 py-1.5"
            style={{
              background: 'rgba(255,255,255,0.95)',
              boxShadow: '0 8px 24px rgba(0,0,0,0.18), inset 0 0 0 1px rgba(255,255,255,0.85)',
              opacity: 0.92,
            }}
          >
            <QueuedEntityRow entity={draggingEntity} />
          </div>
        ) : null}
      </DragOverlay>
      <PromptPicker
        open={pendingDelegate != null}
        source="blueprints"
        title="Choose a blueprint"
        subtitle="Select a blueprint to run for this task"
        selectLabel="Run"
        onSelect={handlePromptSelected}
        onClose={() => setPendingDelegate(null)}
        onEditPrompts={() => {
          setPendingDelegate(null)
          window.location.href = '/prompts'
        }}
        teamValue={delegateTeam}
        onTeamChange={setDelegateTeam}
        selectionDisabled={!teamsLoaded || (multiTeam && delegateTeam === '')}
      />
    </DndContext>
  )
}

// ─── Drawer (top-down chassis view of the clicked station) ────────

// Bottom slide-up sheet — top-down view of the clicked station as
// pure HTML. Reads as the station's chassis seen from above with two
// recessed trays (intake left, main right), each ringed by a cyan
// LED glow and a dark machined floor. Mirrors the 3D scene's
// material palette so the drawer feels like a HUD readout of the
// thing on screen, not a generic data panel.
function StationDrawer({ info }: { info: ClickedStationInfo | null }) {
  const open = info != null
  return (
    <div
      className={`pointer-events-none absolute inset-x-0 bottom-0 z-40 transition-transform duration-[var(--dur-content)] ease-out ${
        open ? 'translate-y-0' : 'translate-y-full'
      }`}
      style={{ height: '46vh' }}
      aria-hidden={!open}
    >
      <div className="pointer-events-auto relative h-full bg-raised/95 backdrop-blur-xl border-t border-line-1 shadow-float shadow-black/[0.12] flex items-stretch p-5">
        <StationChassis info={info} />
      </div>
    </div>
  )
}

// Cream chassis carrying the two trays. The chassis represents the
// physical station body; the trays inside are light glass surfaces
// (frosted white panels with thin colored accent strips) so the whole
// drawer reads as the project's warm/light palette rather than a
// dark gaming HUD.
function StationChassis({ info }: { info: ClickedStationInfo | null }) {
  const runs = info?.runs ?? []
  const orgHref = useOrgHref()
  // Memoized so the per-render TrayItem identities don't churn
  // useDraggable's data prop on every parent render.
  const queueItems: TrayItem[] = useMemo(
    () =>
      (info?.queued ?? []).map((e) => ({
        key: e.id,
        dot: '#a85a3a',
        body: <QueuedEntityRow entity={e} />,
        href: e.url || undefined,
        tooltip: <EntityTooltip entity={e} />,
        tooltipText: entityMetaText(e),
        // dragId enables the drag-to-delegate flow — see DndContext in
        // Factory.tsx's top-level component. The runs-tray drop target
        // reads this back from active.id to resolve the entity.
        dragId: e.id,
      })),
    [info],
  )
  return (
    <div
      className="relative flex w-full gap-4 rounded-2xl p-4"
      style={{
        background: 'linear-gradient(180deg, #f1ebdc 0%, #e6e0d2 100%)',
        boxShadow:
          'inset 0 1px 0 rgba(255,255,255,0.8), inset 0 -2px 0 rgba(0,0,0,0.06), 0 4px 16px rgba(0,0,0,0.05)',
      }}
    >
      <Tray
        label="Queue"
        accent="#a85a3a"
        widthClass="w-[28%]"
        emptyMessage="Idle — no entities waiting"
        items={queueItems}
      />
      <Tray
        label={info?.label ?? '—'}
        accent="#3f6b4d"
        widthClass="flex-1"
        emptyMessage="No runs in flight"
        items={runs.map((r) => ({
          key: r.run.ID,
          dot: conversationStatusColor(r.run.Status),
          body: <ConversationRow conversation={r.run} task={r.task} />,
          // Clicking a conversation opens its full-screen station page in a
          // new tab.
          href: orgHref(`/runs/${r.run.ID}`),
        }))}
        dropId={RUNS_DROP_ID}
      />
    </div>
  )
}

// One light glass tray panel. Frosted white interior, thin accent
// strip across the top header (project-palette color), warm hairline
// border, soft outer shadow for the floating-pane feel. Header text
// is dark on light — readable, restrained — with a small accent dot
// to the left and the tray label in tracked caps.
interface TrayItem {
  key: string
  dot: string
  body: React.ReactNode
  /** When set, the row becomes an `<a>` opening the URL in a new tab. */
  href?: string
  /** When set, hovering — or focusing — the row reveals this content in the
   *  house tooltip. */
  tooltip?: React.ReactNode
  /** The tooltip's words as plain text. Required beside `tooltip` on a row
   *  with an href: the hover panel is aria-hidden scenery there, and this is
   *  what the anchor's aria-describedby carries instead. */
  tooltipText?: string
  /** When set, the row registers as a draggable with this id under the
   *  enclosing DndContext. Used by the station drawer to drag queued
   *  entities onto the runs tray. */
  dragId?: string
}

function Tray({
  label,
  accent,
  widthClass,
  items,
  emptyMessage,
  dropId,
}: {
  label: string
  accent: string
  widthClass: string
  items: TrayItem[]
  emptyMessage: string
  /** When set, the tray's outer container registers as a drop target
   *  under the enclosing DndContext. The runs tray uses this to accept
   *  drags from the queue (drag-to-delegate). */
  dropId?: string
}) {
  // Always call useDroppable so the hook count is stable; `disabled`
  // makes it a no-op when the tray isn't a drop target.
  const { setNodeRef, isOver } = useDroppable({ id: dropId ?? 'tray-noop', disabled: !dropId })
  return (
    <div
      ref={dropId ? setNodeRef : undefined}
      className={`relative flex flex-col overflow-hidden rounded-xl transition-shadow ${widthClass}`}
      style={{
        background:
          'linear-gradient(180deg, rgba(255,255,255,0.85) 0%, rgba(255,255,255,0.6) 100%)',
        boxShadow: [
          'inset 0 1px 0 rgba(255,255,255,0.9)',
          // When a drag is over this drop target, replace the inset
          // hairline with a thicker accent ring matching the runs
          // tray's sage palette so the drop affordance reads cleanly.
          isOver && dropId
            ? `inset 0 0 0 2px ${hexToRgba(accent, 0.85)}`
            : 'inset 0 0 0 1px rgba(255,255,255,0.7)',
          '0 1px 2px rgba(0,0,0,0.04)',
          '0 6px 18px rgba(0,0,0,0.06)',
        ].join(', '),
      }}
    >
      {/* Thin accent strip — replaces the heavy LED border. Sits
          at the very top edge of the tray as a single colored line. */}
      <div className="absolute inset-x-0 top-0 h-[2px]" style={{ background: accent }} />
      <header
        className="flex items-center justify-center gap-2 px-4 py-2.5"
        style={{ borderBottom: '1px solid rgba(0,0,0,0.05)' }}
      >
        <span
          aria-hidden
          className="inline-block h-1.5 w-1.5 rounded-full"
          style={{ background: accent, boxShadow: `0 0 4px ${hexToRgba(accent, 0.55)}` }}
        />
        <span className="text-reported font-semibold uppercase tracking-[0.18em] text-ink-2">
          {label}
        </span>
      </header>
      <ul className="flex flex-1 flex-col gap-1.5 overflow-y-auto px-3 py-3">
        {items.length === 0 ? (
          <li className="px-2 py-1 text-reported italic text-ink-3">{emptyMessage}</li>
        ) : (
          items.map((it) => <TrayRow key={it.key} item={it} />)
        )}
      </ul>
    </div>
  )
}

// Single tray row. Conditionally renders the row content as an `<a>`
// when `href` is set so clicking opens the entity in a new tab; the
// inner content adopts `display: contents` so the anchor doesn't
// affect the row's flex layout. The tooltip wrap is also conditional —
// only rows that pass `tooltip` get the hover panel, which keeps Run
// rows untouched.
function TrayRow({ item }: { item: TrayItem }) {
  const interactive = !!item.href
  // useDraggable's disabled flag keeps the hook call stable across
  // drag-capable and non-drag-capable rows. Without dragId, the hook
  // returns sensors that never fire, so the row behaves exactly as
  // before.
  const drag = useDraggable({ id: item.dragId ?? `row-${item.key}`, disabled: !item.dragId })
  const innerClasses = 'flex flex-1 min-w-0 items-center gap-2.5'
  const inner = (
    <>
      <span
        aria-hidden
        className="inline-block h-1.5 w-1.5 shrink-0 rounded-full"
        style={{ background: item.dot, boxShadow: `0 0 6px ${item.dot}` }}
      />
      {item.body}
    </>
  )
  // The row's outer `<li>` keeps its inset LED border + bg; the
  // anchor sits inside as a "contents" element so its rect is the
  // same as the li's content area — clicking anywhere on the row
  // (except the dot, which is aria-hidden) hits the link.
  // The anchor's description is the tooltip's words: with an href the hover
  // panel is aria-hidden scenery, and this hidden sentence is the route its
  // metadata — which the visible row does not carry — reaches assistive
  // technology. The panel itself still opens on the anchor's focus (the host
  // follows focus within it), so it is not mouse-only either.
  const metaId = useId()
  const described = !!(item.href && item.tooltip && item.tooltipText)
  const rowContent = item.href ? (
    <a
      href={item.href}
      target="_blank"
      rel="noopener noreferrer"
      aria-describedby={described ? metaId : undefined}
      className={`${innerClasses} cursor-pointer`}
    >
      {inner}
    </a>
  ) : (
    <div className={innerClasses}>{inner}</div>
  )
  const draggable = !!item.dragId
  // The tooltip host lives INSIDE the <li> — a wrapper around it would break
  // the list's ul > li nesting — and takes the filling-cell layout so the row
  // reads exactly as it does without a hint. The mode follows the row's own
  // interactivity: with an anchor, the anchor is the interactive thing and the
  // hint is scenery beside it; without one (a queued entity with no URL) the
  // host takes the tab stop itself, or the hint has no keyboard route at all.
  return (
    <li
      ref={draggable ? drag.setNodeRef : undefined}
      {...(draggable ? drag.attributes : {})}
      {...(draggable ? drag.listeners : {})}
      className={`flex items-center gap-2.5 rounded-md px-2.5 py-1.5 transition-all ${
        interactive ? 'hover:-translate-y-px hover:bg-raised' : ''
      } ${draggable ? 'cursor-grab active:cursor-grabbing' : ''} ${
        drag.isDragging ? 'opacity-40' : ''
      }`}
      style={{
        background: 'rgba(255,255,255,0.55)',
        boxShadow: 'inset 0 0 0 1px rgba(255,255,255,0.85), 0 1px 2px rgba(0,0,0,0.03)',
      }}
    >
      {item.tooltip ? (
        <Tooltip
          content={item.tooltip}
          wrap={320}
          focusable={!item.href}
          className="flex-1 min-w-0"
        >
          {rowContent}
        </Tooltip>
      ) : (
        rowContent
      )}
      {described ? (
        <span id={metaId} className="sr-only">
          {item.tooltipText}
        </span>
      ) : null}
    </li>
  )
}

function QueuedEntityRow({ entity }: { entity: FactoryEntity }) {
  const label = entityLabel(entity)
  const title = entity.title || entity.source_id
  return (
    <div className="flex min-w-0 flex-1 items-baseline gap-2">
      <span className="font-mono text-reported text-ink-1">{label}</span>
      <span className="truncate text-label text-ink-2">{title}</span>
    </div>
  )
}

// Hover popup for a queued entity row. Shows the full title (no
// truncation) plus source-specific metadata so the user can read the
// long context without having to click through. Mirrors the columns
// the old factory's StationDetailOverlay surfaced.
function entityMeta(entity: FactoryEntity): { k: string; v: string }[] {
  const meta: { k: string; v: string }[] = []
  if (entity.source === 'github') {
    if (entity.repo) meta.push({ k: 'repo', v: entity.repo })
    if (entity.number != null) meta.push({ k: 'number', v: `#${entity.number}` })
    if (entity.author) meta.push({ k: 'author', v: `@${entity.author}` })
    if (entity.additions != null || entity.deletions != null) {
      const add = entity.additions ?? 0
      const del = entity.deletions ?? 0
      meta.push({ k: 'diff', v: `+${add} −${del}` })
    }
  } else if (entity.source === 'jira') {
    if (entity.source_id) meta.push({ k: 'key', v: entity.source_id })
    if (entity.status) meta.push({ k: 'status', v: entity.status })
    if (entity.priority) meta.push({ k: 'priority', v: entity.priority })
    if (entity.assignee) meta.push({ k: 'assignee', v: entity.assignee })
  }
  return meta
}

// The tooltip's words as one plain sentence, for the row's aria-describedby:
// the hover panel is aria-hidden scenery, so this is the route by which its
// metadata — none of which appears in the visible row — reaches assistive
// technology.
function entityMetaText(entity: FactoryEntity): string {
  const title = entity.title || entity.source_id || entity.id
  return [title, ...entityMeta(entity).map((m) => `${m.k} ${m.v}`)].join(' · ')
}

function EntityTooltip({ entity }: { entity: FactoryEntity }) {
  const meta = entityMeta(entity)
  return (
    <div className="space-y-2">
      <div className="font-medium text-card-title leading-snug text-ink-1">
        {entity.title || entity.source_id || entity.id}
      </div>
      {meta.length > 0 && (
        <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-reported">
          {meta.map((m) => (
            <div key={m.k} className="contents">
              <dt className="uppercase tracking-wider text-ink-3">{m.k}</dt>
              <dd className="font-mono text-ink-2">{m.v}</dd>
            </div>
          ))}
        </dl>
      )}
      {entity.url && (
        <div className="border-t border-line-1 pt-1.5 text-label text-ink-3">
          Click to open in new tab →
        </div>
      )}
    </div>
  )
}

function ConversationRow({ conversation, task }: { conversation: Conversation; task: Task }) {
  const ref = task.source_id || task.entity_id
  const isOpen = conversation.Status === 'open'
  return (
    <div className="flex min-w-0 flex-1 items-baseline gap-2">
      {isOpen && (
        <span
          className="inline-flex items-center text-ui leading-none"
          style={{ color: conversationStatusColor(conversation.Status) }}
          title="Run is open — not concluded, not currently executing"
        >
          ◌
        </span>
      )}
      <span className="font-mono text-reported text-ink-1">{ref}</span>
      <span
        className="text-label uppercase tracking-wider"
        style={{ color: conversationStatusColor(conversation.Status) }}
      >
        {conversationStatusLabel(conversation.Status)}
      </span>
      <span className="ml-auto font-mono text-label text-ink-3">
        {formatConversationMeta(conversation)}
      </span>
    </div>
  )
}

function entityLabel(e: FactoryEntity): string {
  if (e.source === 'github' && e.number != null) return `#${e.number}`
  return e.source_id || e.id.slice(0, 8)
}

// Status-keyed colors for the conversation-row indicator dot and the inline
// status label. Pulled from the project palette tokens so the trays
// feel cohesive with the rest of the app: claim/sage for active,
// snooze/amber for parked, dismiss/rose for failed, secondary for the
// rest (queued, and completed — a finished conversation is unremarkable
// here).
//
// The active arm is the shared predicate rather than a list of names, so
// every claim phase — including a conversation parked awaiting its
// credential bundle — reads as working instead of falling through to the
// inert grey.
function conversationStatusColor(status: ConversationStatusValue): string {
  if (isActiveStatus(status)) return '#3f6b4d' // --color-claim (sage)
  switch (status) {
    case 'open':
      return '#8a6e1f' // --color-snooze (warm amber)
    case 'failed':
      return '#a84545' // --color-alarm (warm rose)
    default:
      return '#4a4541' // --color-ink-2
  }
}

// Shorter copy for the statuses whose raw name reads badly in a tray row;
// everything else renders as itself.
function conversationStatusLabel(status: ConversationStatusValue): string {
  switch (status) {
    case 'agent_starting':
      return 'starting'
    case 'awaiting_credentials':
      return 'awaiting credentials'
    default:
      return status
  }
}

function formatConversationMeta(conversation: Conversation): string {
  const parts: string[] = []
  if (conversation.DurationMs && conversation.DurationMs > 0) {
    const sec = Math.round(conversation.DurationMs / 1000)
    if (sec < 60) parts.push(`${sec}s`)
    else parts.push(`${Math.floor(sec / 60)}m ${sec % 60}s`)
  }
  if (conversation.TotalCostUSD && conversation.TotalCostUSD > 0) {
    parts.push(`$${conversation.TotalCostUSD.toFixed(2)}`)
  }
  return parts.join(' · ')
}

function hexToRgba(hex: string, alpha: number): string {
  const h = hex.replace('#', '')
  const r = parseInt(h.slice(0, 2), 16)
  const g = parseInt(h.slice(2, 4), 16)
  const b = parseInt(h.slice(4, 6), 16)
  return `rgba(${r}, ${g}, ${b}, ${alpha})`
}
