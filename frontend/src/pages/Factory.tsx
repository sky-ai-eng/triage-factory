import { useEffect, useRef, useState } from 'react'
import {
  createFactoryScene,
  type SceneHandle,
  type SchemaIndex,
  type ViewSnapshot,
} from '../factory/scene'
import StationDetailOverlay, {
  type StationRunSummary,
  type StationThroughput,
} from '../factory/StationDetailOverlay'
import RunDrawer from '../factory/RunDrawer'
import { useWebSocket } from '../hooks/useWebSocket'
import type { AgentRun, FactorySnapshot, Task } from '../types'

type Phase = 'loading' | 'ready' | 'error'

// How long after a triggering WS event before we refetch the factory
// snapshot. Collapses rapid bursts (a poll cycle that emits a dozen events
// in quick succession) into one HTTP call. 1.5s feels instant to the user
// while giving the burst time to settle.
const REFETCH_DEBOUNCE_MS = 1500

export default function Factory() {
  const containerRef = useRef<HTMLDivElement>(null)
  const overlayRef = useRef<HTMLDivElement>(null)
  const sceneRef = useRef<SceneHandle | null>(null)
  const [phase, setPhase] = useState<Phase>('loading')
  const [error, setError] = useState('')
  const [schemas, setSchemas] = useState<SchemaIndex | null>(null)
  const [snapshot, setSnapshot] = useState<ViewSnapshot | null>(null)
  const [factoryData, setFactoryData] = useState<FactorySnapshot | null>(null)
  const [drawer, setDrawer] = useState<{ task: Task; run: AgentRun } | null>(null)

  // Fetch the predicate-field schemas once up front. Stations render their
  // filter chips from this data; mounting the Pixi scene before it's loaded
  // would leave the chips empty and force an ugly re-build.
  useEffect(() => {
    let cancelled = false
    fetch('/api/event-schemas')
      .then((r) => {
        if (!r.ok) throw new Error(`Failed to load event schemas (${r.status})`)
        return r.json() as Promise<SchemaIndex>
      })
      .then((data) => {
        if (cancelled) return
        setSchemas(data)
        setPhase('ready')
      })
      .catch((err) => {
        if (cancelled) return
        setError(err instanceof Error ? err.message : String(err))
        setPhase('error')
      })
    return () => {
      cancelled = true
    }
  }, [])

  // Fetch the factory snapshot — stations' runs + throughput, and the pool
  // of active entities that drive belt items. Separate hook because it
  // re-runs on WS events (debounced) while the schema fetch is one-shot.
  useEffect(() => {
    let cancelled = false
    let pending: ReturnType<typeof setTimeout> | null = null

    const load = () => {
      fetch('/api/factory/snapshot')
        .then((r) => {
          if (!r.ok) throw new Error(`Failed to load factory snapshot (${r.status})`)
          return r.json() as Promise<FactorySnapshot>
        })
        .then((data) => {
          if (cancelled) return
          setFactoryData(data)
          sceneRef.current?.setEntityPool(data.entities)
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

    // Events that plausibly invalidate the snapshot. `event` covers new
    // entities/transitions; `tasks_updated` covers task creation and
    // status flips; `agent_run_update` covers the run list inside
    // stations.
    ;(window as unknown as { __factoryRefetch?: () => void }).__factoryRefetch = schedule

    return () => {
      cancelled = true
      if (pending) clearTimeout(pending)
      delete (window as unknown as { __factoryRefetch?: () => void }).__factoryRefetch
    }
  }, [])

  // WS hookup — we route interesting events through the window callback
  // the effect above installs. Keeping the debounce in the effect closure
  // avoids stale-state issues if the WS handler re-identifies.
  useWebSocket((evt) => {
    if (evt.type === 'event' || evt.type === 'tasks_updated' || evt.type === 'agent_run_update') {
      const refetch = (window as unknown as { __factoryRefetch?: () => void }).__factoryRefetch
      refetch?.()
    }
  })

  useEffect(() => {
    if (phase !== 'ready' || !schemas) return
    const container = containerRef.current
    if (!container) return

    let unsubscribeView: (() => void) | null = null
    let cancelled = false

    createFactoryScene(container, schemas).then((scene) => {
      if (cancelled) {
        scene.destroy()
        return
      }
      sceneRef.current = scene
      // If a snapshot already landed while the scene was initializing,
      // hand it off now so items stop using demo data the moment the
      // pool is available.
      if (factoryData) scene.setEntityPool(factoryData.entities)
      // Snapshot drives overlay placement. Setting via state is the simplest
      // integration — at ~13 stations and view events only firing during pan
      // and zoom interactions, React's reconciliation cost is negligible.
      unsubscribeView = scene.onView((snap) => {
        setSnapshot(snap)
      })
    })

    return () => {
      cancelled = true
      unsubscribeView?.()
      sceneRef.current?.destroy()
      sceneRef.current = null
    }
    // factoryData intentionally excluded — we don't rebuild the scene when
    // the snapshot changes, we just push the new pool into the existing
    // scene via sceneRef.current.setEntityPool in the fetch effect.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [phase, schemas])

  const nearZoom = snapshot?.nearZoom ?? false
  const stations = snapshot?.stations ?? []

  const handleOpenRun = (summary: StationRunSummary) => {
    setDrawer({ task: summary.task, run: summary.run })
  }

  // Resolve per-station overlay props from the factory snapshot. Stations
  // with no activity get undefined so the overlay falls back to its own
  // empty-state rendering.
  const stationData = (eventType: string) => {
    const fs = factoryData?.stations[eventType]
    if (!fs) return { runs: undefined, throughput: undefined }
    const runs: StationRunSummary[] = fs.runs.map((r) => ({
      task: r.task,
      run: r.run,
      mine: r.mine,
    }))
    const throughput: StationThroughput = {
      items24h: fs.items_24h,
      triggered24h: fs.triggered_24h,
      active: fs.active_runs,
    }
    return { runs, throughput }
  }

  return (
    <div className="-mx-8 -my-8">
      {/* overflow-hidden on the canvas container clips the HTML overlay
          layer to the viewport. Without it, zooming in pushes stations at
          the edges of the world to screen coordinates like top: 2400px,
          and those absolutely-positioned overlays balloon the document's
          scrollable area — adding vertical scrollbars to the whole page. */}
      <div
        ref={containerRef}
        className="relative w-full overflow-hidden"
        style={{ height: 'calc(100vh - 69px)' }}
      >
        {phase === 'loading' && (
          <div className="absolute inset-0 flex items-center justify-center text-[13px] text-text-tertiary">
            Loading factory…
          </div>
        )}
        {phase === 'error' && (
          <div className="absolute inset-0 flex items-center justify-center text-[13px] text-dismiss">
            {error}
          </div>
        )}
        {/* Overlay layer — positioned absolutely over the Pixi canvas.
            Individual StationDetailOverlay instances render only at near
            zoom; `pointer-events: none` on this wrapper means the canvas
            still receives drag/scroll events through the gaps between
            overlays, while each overlay's interior re-enables its own
            pointer events for clickable run rows. */}
        <div
          ref={overlayRef}
          className="absolute inset-0 overflow-hidden pointer-events-none"
          style={{ display: nearZoom ? 'block' : 'none' }}
        >
          {nearZoom &&
            stations.map((placement) => {
              const { runs, throughput } = stationData(placement.eventType)
              return (
                <StationDetailOverlay
                  key={placement.id}
                  placement={placement}
                  runs={runs}
                  throughput={throughput}
                  onOpenRun={handleOpenRun}
                />
              )
            })}
        </div>
      </div>

      <RunDrawer
        task={drawer?.task ?? null}
        run={drawer?.run ?? null}
        onClose={() => setDrawer(null)}
      />
    </div>
  )
}
