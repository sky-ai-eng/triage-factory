// First-slice factory POC: two stations + one canonical belt between them.
//
// Stations are built from a (FactoryEvent, FieldSchema[]) pair and rendered
// as procedural liquid-glass machines (see station.ts). The belt is a
// quadratic curve with small pulses flowing along it; items periodically
// emerge from the left station's right port, traverse the belt, and enter
// the right station's left port.
//
// Coordinate system is a fixed 1400x700 world. The Viewport handles pan
// (drag), zoom (wheel / pinch), and decelerated release; on mount it fits
// the world into whatever canvas size we're rendered into, centered.

import { Application, Container, Graphics, Text, Ticker } from 'pixi.js'
import { Viewport } from 'pixi-viewport'
import type { FactoryEntity, FieldSchema } from '../types'
import { FACTORY_EVENTS } from './events'
import {
  buildStation,
  BELT_WIDTH,
  FAR_ZOOM_THRESHOLD,
  NEAR_ZOOM_THRESHOLD,
  type Port,
  type StationHandle,
} from './station'
import {
  buildNode,
  buildPole,
  buildTunnel,
  type NodeHandle,
  type PoleHandle,
  type TunnelHandle,
} from './nodes'

const COLOR_ACCENT = 0xc47a5a
const TEXT_PRIMARY = 0x1a1a1a
const TEXT_TERTIARY = 0xa09a94
const TEXT_SECONDARY = 0x6b6560
/** Grid cell size. All primitives position their centers on integer grid
 * coordinates, so axis-aligned belts between them are guaranteed straight. */
const GRID = 80
const g = (col: number) => col * GRID
const SCENE_W = g(44)
const SCENE_H = g(18)

type Side = 'left' | 'right' | 'top' | 'bottom'
type GraphNode = StationHandle | NodeHandle | PoleHandle | TunnelHandle

type NodeDef =
  | { kind: 'station'; eventType: string; col: number; row: number }
  | { kind: 'splitter' | 'merger'; col: number; row: number; label?: string; orientation?: Side }
  | { kind: 'pole'; col: number; row: number }
  | {
      kind: 'tunnel_entrance' | 'tunnel_exit'
      col: number
      row: number
      side: Side
    }

// Graph — stations carry event semantics, splitters/mergers are pure
// routing, poles are pass-through waypoints. All centers sit on integer
// grid cells (cell size = GRID = 80px), so axis-aligned belts between
// same-row or same-column primitives are guaranteed perfectly straight.
//
// Layout:
//  - Main line runs across row 6. Build branch dips to row 8 via poles +
//    a merger right before new_commits; ci_failed sits at row 10.
//  - CI-failed loopback wraps through poles at row 12 (below everything)
//    to reach new_commits from the left without sharing any port.
//  - Direct-approval arc is still splitter A.top → merger_main.top.
//  - Review-outcome branches hang off splitter B at col 36.
const NODE_DEFS: NodeDef[] = [
  { kind: 'station', eventType: 'github:pr:opened', col: 4, row: 6 }, //                     0
  { kind: 'station', eventType: 'github:pr:ready_for_review', col: 8, row: 6 }, //           1
  { kind: 'splitter', label: 'next?', col: 12, row: 6, orientation: 'left' }, //             2
  { kind: 'merger', col: 13, row: 8, orientation: 'right' }, //                              3  merger_nc
  { kind: 'station', eventType: 'github:pr:new_commits', col: 16, row: 8 }, //               4
  { kind: 'splitter', label: 'ci?', col: 20, row: 8, orientation: 'left' }, //               5  shifted right 1
  { kind: 'station', eventType: 'github:pr:conflicts', col: 23, row: 5 }, //                 6  up 1, right 1
  { kind: 'station', eventType: 'github:pr:ci_check_passed', col: 23, row: 8 }, //           7  shifted right 1
  { kind: 'station', eventType: 'github:pr:ci_check_failed', col: 23, row: 11 }, //          8  shifted right 1
  { kind: 'pole', col: 26, row: 11 }, //                                                     9  east of ci_failed (right 1)
  { kind: 'pole', col: 26, row: 13 }, //                                                     10 south of pole 9 (right 1)
  { kind: 'pole', col: 13, row: 13 }, //                                                     11 west of pole 10, below merger_nc
  // Review cycle on row 4. Flow: merger_main → review_requested, then loops
  // back down via poles (34, 35, 36, 37) to splitter_B. The intermediate
  // "review_received" station was a synthetic visualization-only event with
  // no backing in the domain — removed in SKY-189-followup so the topology
  // matches what the backend can actually emit.
  { kind: 'merger', col: 26, row: 4, orientation: 'right' }, //                              12 merger_main
  { kind: 'station', eventType: 'github:pr:review_requested', col: 30, row: 4 }, //          13 row 4
  { kind: 'splitter', label: 'review result', col: 30, row: 11, orientation: 'left' }, //    14 splitter_B (row 11, matches review_approved)
  { kind: 'pole', col: 30, row: 8 }, //                                                      15 E-shape top corner (review_commented row)
  { kind: 'pole', col: 30, row: 14 }, //                                                     16 E-shape bottom corner (changes_requested row)
  { kind: 'station', eventType: 'github:pr:review_commented', col: 34, row: 8 }, //          17 spaced 3 from approved
  { kind: 'station', eventType: 'github:pr:review_approved', col: 34, row: 11 }, //          18 middle
  { kind: 'station', eventType: 'github:pr:review_changes_requested', col: 34, row: 14 }, // 19 spaced 3 from approved
  { kind: 'station', eventType: 'github:pr:merged', col: 41, row: 11 }, //                   20 col 41 so closed can clear bus merger 28 at col 38
  { kind: 'station', eventType: 'github:pr:closed', col: 41, row: 16 }, //                   21 bus has belt room between merger 28 (col 38) and closed

  // ─── Closed-bus infrastructure ─────────────────────────────────────────────
  { kind: 'splitter', label: 'continue?', col: 11, row: 6, orientation: 'left' }, //         22 s_rfr
  { kind: 'splitter', label: 'continue?', col: 19, row: 8, orientation: 'left' }, //         23 s_commits
  { kind: 'splitter', label: 'respond?', col: 37, row: 14, orientation: 'left' }, //         24 s_changes (changes_requested row)
  { kind: 'pole', col: 11, row: 16 }, //                                                     25 bus entry
  { kind: 'merger', col: 19, row: 16, orientation: 'right' }, //                             26 bus + new_commits drop (col 19, aligned with s_commits)
  { kind: 'pole', col: 38, row: 8 }, //                                                      27 review_commented east-turn (east of review_commented row 8)
  { kind: 'merger', col: 38, row: 16, orientation: 'right' }, //                             28 bus + review_commented drop
  { kind: 'merger', col: 37, row: 16, orientation: 'right' }, //                             29 bus + changes_requested drop

  // ─── changes_requested retry path ───────────────────────────────────────────
  // Vertical tunnel from s_changes straight up to near retry pole 30.
  // The old row-2 horizontal tunnel is now a regular belt (nothing in the
  // way anymore since the review chain moved down to row 5).
  { kind: 'pole', col: 37, row: 2 }, //                                                      30 retry: east end, turns south→west
  { kind: 'tunnel_entrance', col: 37, row: 12, side: 'bottom' }, //                          31 vertical tunnel entrance (near s_changes)
  { kind: 'tunnel_exit', col: 37, row: 7, side: 'top' }, //                                  32 vertical tunnel exit (near retry pole 30)
  { kind: 'pole', col: 13, row: 2 }, //                                                      33 retry: west end, turns east→south into merger_nc.top

  // ─── Review-cycle return path (row 4 → row 6 → row 11) ─────────────────────
  // Loop-back poles route review_requested.right south then west, finally
  // east into splitter_B.left. Two turns at the top (east→south→west),
  // two at the left (west→south→east). Pole_R1/R2 sit at col 34 — pulled in
  // from col 36 when review_received was removed, since the row-4 horizontal
  // run is now shorter (review_requested directly to Pole_R1).
  { kind: 'pole', col: 33, row: 4 }, //                                                      34 Pole_R1
  { kind: 'pole', col: 33, row: 6 }, //                                                      35 Pole_R2
  { kind: 'pole', col: 29, row: 6 }, //                                                      36 Pole_R3
  { kind: 'pole', col: 29, row: 11 }, //                                                     37 Pole_R4: aligned with splitter_B row 11, feeds splitter_B.left horizontally

  // ─── splitter_A top path (replaces direct arc to merger_main.top) ──────────
  // splitter_A.top goes up to pole A, east across to pole B (2 rows above
  // merger_main), then down into merger_main.top. Keeps the top path
  // axis-aligned with two gentle 90° bends.
  { kind: 'pole', col: 12, row: 3 }, //                                                      38 pole A above splitter_A
  { kind: 'pole', col: 26, row: 3 }, //                                                      39 pole B above merger_main

  // ─── Tunnel on the review_commented drop where it crosses approved→merged ─
  // Review_commented.right → east-turn pole 27 → VERTICAL DROP at col 38 →
  // bus merger 28. With merged at row 11, the approved→merged belt crosses
  // col 38 row 11 — this tunnel lets the drop pass under it.
  { kind: 'tunnel_entrance', col: 38, row: 10, side: 'top' }, //                             40 above approved→merged belt
  { kind: 'tunnel_exit', col: 38, row: 12, side: 'bottom' }, //                              41 below approved→merged belt

  // ─── Conflicts retry: routes up to the row-2 retry line ─────────────────────
  // Conflicts.right → east-turn pole → vertical up to a new merger on the
  // retry line. The merger sits mid-line, splitting the old direct belt
  // from pole 30 → pole 33 into two segments with conflicts joining from
  // below.
  //
  // T-merger now: conflicts enters from the left, the ci-passed "refresh"
  // path enters from the right, and the combined flow heads north to the
  // retry line. Before SKY-CI-refresh this was a plain pole — only one
  // input (conflicts) and one output (north).
  { kind: 'merger', col: 27, row: 5, orientation: 'top' }, //                                42 T-merger: conflicts (L) + ci-passed refresh (R) → north
  { kind: 'merger', col: 27, row: 2, orientation: 'left' }, //                               43 retry-line merger (conflicts drops in from south)

  // ─── CI-passed split: continue to review OR refresh new_commits ────────────
  // CI passed used to flow diagonally straight to merger_main. Split it so
  // some runs bypass review and re-enter the build branch (as if they
  // pushed a new commit that needs re-validation). The right-hand branch
  // steps one column east then heads straight north into the T-merger at
  // node 42, which feeds up to the retry line and back into merger_nc →
  // new_commits.
  { kind: 'splitter', label: 'next?', col: 26, row: 8, orientation: 'left' }, //             44 ci-passed split (input L, outputs T/R)
  { kind: 'pole', col: 27, row: 8 }, //                                                      45 east-turn on ci-passed refresh path; col-aligned with T-merger 42

  // ─── Tunnel on the s_commits abandon-drop where it crosses the
  // ci_failed loopback ──────────────────────────────────────────────────────
  // The abandon-drop runs vertically at col 19 from s_commits (row 8) down
  // to the bus merger (row 16). At row 13 it crosses the ci_failed → new_commits
  // retry line (node 10 at col 26 row 13 → node 11 at col 13 row 13, running
  // leftward). Diving under via a vertical tunnel at col 19 rows 12/14
  // keeps both paths clear without adding a merger on the horizontal run.
  { kind: 'tunnel_entrance', col: 19, row: 12, side: 'top' }, //                             46 above the retry loopback
  { kind: 'tunnel_exit', col: 19, row: 14, side: 'bottom' }, //                              47 below the retry loopback
]

interface EdgeDef {
  from: number
  to: number
  fromSide: Side
  toSide: Side
}

const BELT_EDGES: EdgeDef[] = [
  // ─── Main flow row 6 ───────────────────────────────────────────────────────
  { from: 0, to: 1, fromSide: 'right', toSide: 'left' }, //   opened → rfr
  { from: 1, to: 22, fromSide: 'right', toSide: 'left' }, //  rfr → s_rfr
  { from: 22, to: 2, fromSide: 'right', toSide: 'left' }, //  s_rfr.right → splitter A
  { from: 22, to: 25, fromSide: 'bottom', toSide: 'top' }, // s_rfr.bottom → bus entry pole (abandon drop)
  { from: 2, to: 3, fromSide: 'bottom', toSide: 'left' }, //  A.bottom → merger_nc  (dip into build branch)
  // splitter_A top path: up to pole 38, east to pole 39, down to merger_main.top
  { from: 2, to: 38, fromSide: 'top', toSide: 'bottom' }, //  A.top → pole 38.bottom  (vertical up)
  { from: 38, to: 39, fromSide: 'right', toSide: 'left' }, // pole 38 → pole 39  (horizontal east at row 3)
  { from: 39, to: 12, fromSide: 'bottom', toSide: 'top' }, // pole 39 → merger_main.top  (vertical down)
  { from: 3, to: 4, fromSide: 'right', toSide: 'left' }, //   merger_nc → new_commits
  { from: 4, to: 23, fromSide: 'right', toSide: 'left' }, //  new_commits → s_commits
  { from: 23, to: 5, fromSide: 'right', toSide: 'left' }, //  s_commits.right → ci_splitter
  // s_commits abandon drop — splits into three segments via a vertical
  // tunnel so the belt visually passes UNDER the ci_failed → new_commits
  // retry line that runs east-west across row 13.
  { from: 23, to: 46, fromSide: 'bottom', toSide: 'top' }, //  s_commits → tunnel entrance (row 8 → row 12)
  { from: 46, to: 47, fromSide: 'top', toSide: 'bottom' }, //  invisible tunnel span (crosses row 13)
  { from: 47, to: 26, fromSide: 'bottom', toSide: 'top' }, //  tunnel exit → bus merger (row 14 → row 16)

  // ─── CI branch (row 8 + loopback) ──────────────────────────────────────────
  // Conflicts is now reachable via a direct diagonal belt from ci_splitter.top.
  // The old tunnel was needed because the skip-build arc was at the same row
  // as conflicts; with merger_main moved up to row 2, the arc lifts and
  // conflicts drops to row 4, clearing the path.
  { from: 5, to: 6, fromSide: 'top', toSide: 'left' }, //     ci_splitter.top → conflicts.left (direct diagonal)
  { from: 5, to: 7, fromSide: 'right', toSide: 'left' }, //   ci_splitter → ci_passed
  { from: 5, to: 8, fromSide: 'bottom', toSide: 'left' }, //  ci_splitter.bottom → ci_failed
  // ci_passed splits: continue to review (top → merger_main) OR loop back
  // to new_commits via the T-merger at node 42 (right → pole 45 → merger
  // 42 → retry line).
  { from: 7, to: 44, fromSide: 'right', toSide: 'left' }, //   ci_passed → splitter
  { from: 44, to: 12, fromSide: 'top', toSide: 'bottom' }, //  splitter.top → merger_main.bottom (row 8 → row 4)
  { from: 44, to: 45, fromSide: 'right', toSide: 'left' }, //  splitter.right → pole 45 (east, one step)
  { from: 45, to: 42, fromSide: 'top', toSide: 'bottom' }, //  pole 45 → T-merger.bottom (vertical north, col 27)
  { from: 8, to: 9, fromSide: 'right', toSide: 'left' }, //   ci_failed.right → pole(9)
  { from: 9, to: 10, fromSide: 'bottom', toSide: 'top' }, //  pole(9) → pole(10)
  { from: 10, to: 11, fromSide: 'left', toSide: 'right' }, // pole(10) → pole(11)
  { from: 11, to: 3, fromSide: 'top', toSide: 'bottom' }, //  pole(11) → merger_nc.bottom

  // ─── Review cycle row 4 + loop-back via poles ──────────────────────────────
  // merger_main → review_requested on row 4, then loop back down through
  // Pole_R1..R4 (nodes 34..37) into splitter_B.left on row 11.
  { from: 12, to: 13, fromSide: 'right', toSide: 'left' }, // merger_main → review_requested
  { from: 13, to: 34, fromSide: 'right', toSide: 'left' }, // review_requested → Pole_R1 (row 4 east)
  { from: 34, to: 35, fromSide: 'bottom', toSide: 'top' }, // Pole_R1 → Pole_R2 (south, row 4→6)
  { from: 35, to: 36, fromSide: 'left', toSide: 'right' }, // Pole_R2 → Pole_R3 (west along row 6)
  { from: 36, to: 37, fromSide: 'bottom', toSide: 'top' }, // Pole_R3 → Pole_R4 (south, row 6→11)
  { from: 37, to: 14, fromSide: 'right', toSide: 'left' }, // Pole_R4 → splitter_B.left (east into splitter)

  // ─── E-shape review outcomes ───────────────────────────────────────────────
  { from: 14, to: 15, fromSide: 'top', toSide: 'bottom' }, // B.top → top pole
  { from: 15, to: 17, fromSide: 'right', toSide: 'left' }, // top pole → review_commented
  { from: 14, to: 18, fromSide: 'right', toSide: 'left' }, // B.right → review_approved
  { from: 14, to: 16, fromSide: 'bottom', toSide: 'top' }, // B.bottom → bottom pole
  { from: 16, to: 19, fromSide: 'right', toSide: 'left' }, // bottom pole → changes_requested
  { from: 18, to: 20, fromSide: 'right', toSide: 'left' }, // review_approved → merged

  // ─── Closed-bus chain ──────────────────────────────────────────────────────
  { from: 17, to: 27, fromSide: 'right', toSide: 'left' }, //  review_commented → east-turn pole
  // Drop passes under the approved→merged belt at row 11 via a vertical tunnel.
  { from: 27, to: 40, fromSide: 'bottom', toSide: 'top' }, //  east-turn pole → tunnel entrance (short vertical)
  { from: 40, to: 41, fromSide: 'top', toSide: 'bottom' }, //  vertical tunnel (invisible, crosses row 11)
  { from: 41, to: 28, fromSide: 'bottom', toSide: 'top' }, //  tunnel exit → bus merger (drop continues south)
  { from: 19, to: 24, fromSide: 'right', toSide: 'left' }, //  changes_requested → s_changes
  { from: 24, to: 29, fromSide: 'bottom', toSide: 'top' }, //  s_changes.bottom → bus merger (abandon drop)
  { from: 25, to: 26, fromSide: 'right', toSide: 'left' }, //  bus entry pole → bus merger 1
  // Bus mergers sit at cols 37 (node 29) and 38 (node 28), so the chain has
  // to hit 29 BEFORE 28 to stay monotonically left-to-right.
  { from: 26, to: 29, fromSide: 'right', toSide: 'left' }, //  bus merger 1 → bus merger (col 37, picks up changes_requested)
  { from: 29, to: 28, fromSide: 'right', toSide: 'left' }, //  col 37 → col 38 (picks up review_commented)
  { from: 28, to: 21, fromSide: 'right', toSide: 'left' }, //  col 38 → closed

  // ─── changes_requested retry loopback ──────────────────────────────────────
  // New: vertical tunnel from s_changes straight up to near retry pole 30.
  // Row-2 horizontal is now a regular belt (review chain moved, nothing
  // in the way).
  { from: 24, to: 31, fromSide: 'top', toSide: 'bottom' }, //  s_changes.top → tunnel entrance (below, col 37)
  { from: 31, to: 32, fromSide: 'bottom', toSide: 'top' }, //  vertical tunnel (invisible, col 37 rows 12→4)
  { from: 32, to: 30, fromSide: 'top', toSide: 'bottom' }, //  tunnel exit → retry pole 30 (short vertical up)
  // Retry line is split by merger 43 so conflicts can join from below.
  { from: 30, to: 43, fromSide: 'left', toSide: 'right' }, //  retry pole 30 → merger 43 (row 2 west)
  { from: 43, to: 33, fromSide: 'left', toSide: 'right' }, //  merger 43 → retry end pole 33 (row 2 west)
  { from: 33, to: 3, fromSide: 'bottom', toSide: 'top' }, //   retry end pole → merger_nc.top
  // Conflicts routes up to the retry line.
  { from: 6, to: 42, fromSide: 'right', toSide: 'left' }, //   conflicts.right → pole 42 (horizontal east)
  { from: 42, to: 43, fromSide: 'top', toSide: 'bottom' }, //  pole 42.top → merger 43.bottom (vertical north to retry line)
]

function getPort(node: GraphNode, side: Side): Port | undefined {
  switch (side) {
    case 'left':
      return node.leftPort
    case 'right':
      return node.rightPort
    case 'top':
      return node.topPort
    case 'bottom':
      return node.bottomPort
  }
}

interface Belt {
  pointAt: (t: number) => { x: number; y: number }
}

/** Snapshot of one station's screen-space placement. Recomputed each time
 * the viewport pans or zooms and published via ViewSnapshot — the HTML
 * detail overlay uses these rects to position itself over the Pixi stations
 * without having to know anything about the world coordinate system. */
export interface StationScreenPlacement {
  /** Node index in the scene's node list. Stable across frames. */
  id: number
  eventType: string
  /** Station frame top-left, in CSS pixels relative to the canvas container. */
  screenX: number
  screenY: number
  /** Station frame dimensions, in CSS pixels (already multiplied by scale). */
  screenW: number
  screenH: number
}

export interface ViewSnapshot {
  /** Current viewport scale — 1 = neutral, <1 = zoomed out, >1 = zoomed in. */
  scale: number
  /** True once `scale >= NEAR_ZOOM_THRESHOLD`; the overlay layer uses this
   * to decide whether to render station-detail cards. */
  nearZoom: boolean
  stations: StationScreenPlacement[]
}

export interface SceneHandle {
  destroy: () => void
  /** Subscribe to viewport changes (pan + zoom). Invoked once immediately
   * with the current snapshot, then on every `moved` / `zoomed` event, and
   * on container resize. Returns an unsubscribe function. */
  onView: (cb: (snapshot: ViewSnapshot) => void) => () => void
  /** Replace the pool of entities items pull their metadata from. Passing
   * an empty array falls back to the demo synthetic pool — useful on a
   * fresh install before the poller has surfaced real data. Items already
   * on-belt keep the metadata they were spawned with; new spawns pick
   * from the updated pool. */
  setEntityPool: (entities: FactoryEntity[]) => void
}

/** Predicate-field schemas keyed by event_type — what `GET /api/event-schemas` returns. */
export type SchemaIndex = Record<string, { event_type: string; fields: FieldSchema[] }>

export async function createFactoryScene(
  container: HTMLDivElement,
  schemas: SchemaIndex,
): Promise<SceneHandle> {
  const app = new Application()
  await app.init({
    resizeTo: container,
    background: 0xf7f5f2,
    antialias: true,
    resolution: window.devicePixelRatio || 1,
    autoDensity: true,
  })
  container.appendChild(app.canvas)

  const scene = new Viewport({
    screenWidth: app.screen.width,
    screenHeight: app.screen.height,
    worldWidth: SCENE_W,
    worldHeight: SCENE_H,
    events: app.renderer.events,
  })
  app.stage.addChild(scene)

  // Pan (drag), mouse-wheel zoom, touchpad pinch, inertia on release.
  // Clamping the zoom keeps users from zooming into the pixel grid or out
  // to where the world is a speck.
  scene
    .drag()
    .pinch()
    .wheel({ smooth: 5, percent: 0.1 })
    .decelerate({ friction: 0.94 })
    .clampZoom({ minScale: 0.35, maxScale: 3 })

  // Initial zoom floor. `scene.fit(true)` alone drops small screens down
  // to ~0.4, well below FAR_ZOOM_THRESHOLD (0.6), so the page would open
  // stuck in the simplified far view even on a normal laptop. Floor the
  // initial scale at 0.7 — comfortably inside the mid tier — and let the
  // user zoom out further themselves if they want the overview.
  const INITIAL_MIN_SCALE = 0.7
  const fitAndCenter = () => {
    scene.resize(app.screen.width, app.screen.height, SCENE_W, SCENE_H)
    scene.fit(true, SCENE_W, SCENE_H)
    if (scene.scaled < INITIAL_MIN_SCALE) {
      scene.setZoom(INITIAL_MIN_SCALE, true)
    }
    scene.moveCenter(SCENE_W / 2, SCENE_H / 2)
  }
  fitAndCenter()
  const ro = new ResizeObserver(() => {
    // Preserve the user's current center/scale on resize — refitting mid-
    // interaction would fight with their pan/zoom intent.
    scene.resize(app.screen.width, app.screen.height, SCENE_W, SCENE_H)
  })
  ro.observe(container)

  // Bucket edges by pole index so each buildPole call gets its {in, out}
  // sides without nested-loop scanning. Poles are defined to have at most
  // one incoming and one outgoing edge each (pure routing waypoints, not
  // splitters or mergers). Single O(N + M) pass at scene init, outside
  // the per-frame hot path entirely.
  const poleIn = new Map<number, Side>()
  const poleOut = new Map<number, Side>()
  for (const edge of BELT_EDGES) {
    if (NODE_DEFS[edge.to].kind === 'pole') poleIn.set(edge.to, edge.toSide)
    if (NODE_DEFS[edge.from].kind === 'pole') poleOut.set(edge.from, edge.fromSide)
  }
  const poleConnection = (idx: number) => {
    const inSide = poleIn.get(idx)
    const outSide = poleOut.get(idx)
    if (inSide && outSide) return { in: inSide, out: outSide }
    return undefined
  }

  // Build all graph nodes at their grid-defined positions.
  const nodes: GraphNode[] = NODE_DEFS.map((def, idx): GraphNode => {
    const center = { x: g(def.col), y: g(def.row) }
    switch (def.kind) {
      case 'station':
        return buildStation(scene, {
          event: FACTORY_EVENTS[def.eventType],
          fields: schemas[def.eventType]?.fields ?? [],
          enabled: true,
          center,
        })
      case 'pole':
        return buildPole(scene, center, poleConnection(idx))
      case 'tunnel_entrance':
      case 'tunnel_exit':
        return buildTunnel(scene, {
          role: def.kind === 'tunnel_entrance' ? 'entrance' : 'exit',
          side: def.side,
          center,
        })
      case 'splitter':
      case 'merger':
        return buildNode(scene, {
          kind: def.kind,
          center,
          label: def.label,
          orientation: def.orientation,
        })
    }
  })

  // Build belts from the edge list. Each edge names the source and target
  // sides explicitly so splitter/merger multi-port wiring is unambiguous.
  const edges = BELT_EDGES.map((def) => {
    const fromNode = nodes[def.from]
    const toNode = nodes[def.to]
    const fromPort = getPort(fromNode, def.fromSide)
    const toPort = getPort(toNode, def.toSide)
    if (!fromPort || !toPort) {
      throw new Error(
        `Edge ${def.from} → ${def.to} references a missing port (${def.fromSide}/${def.toSide})`,
      )
    }
    // Tunnel edges connect an entrance to its exit — rendered invisibly
    // so items appear to dive under and pop back up. Item alpha handles
    // the fade-out / fade-in during the (unrendered) traversal.
    const isTunnel = fromNode.kind === 'tunnel_entrance' && toNode.kind === 'tunnel_exit'
    return {
      from: def.from,
      to: def.to,
      belt: buildBelt(scene, fromPort, toPort, {
        visible: !isTunnel,
        tunnel: isTunnel,
      }),
      isTunnel,
    }
  })
  // Chevrons only on visible belts — tunnel segments have no surface.
  const chevronLayers = edges
    .filter((e) => !e.isTunnel)
    .map((e) => buildBeltChevrons(scene, e.belt))

  // Event-type → station node index map. Items park at whichever station's
  // event_type matches their entity's current_event_type; entities whose
  // current_event_type doesn't correspond to a station on the board are
  // skipped entirely (system events, non-visualized PR events).
  const eventTypeToNode = new Map<string, number>()
  NODE_DEFS.forEach((def, idx) => {
    if (def.kind === 'station') eventTypeToNode.set(def.eventType, idx)
  })

  const items = buildItemSpawner(scene, nodes, edges, eventTypeToNode)

  const tick = (t: Ticker) => {
    const dt = t.deltaMS / 1000
    const scale = scene.scaled
    for (const n of nodes) {
      if (n.kind === 'station') n.update(dt, scale)
      else n.update(dt)
    }
    for (const ch of chevronLayers) ch.update(dt)
    // Items use scale for LOD and the visible world rect for culling the
    // expanded-detail path. Recompute both once per tick instead of per
    // item — both are cheap but still worth sharing.
    const vb = scene.getVisibleBounds()
    items.update(dt, {
      scale,
      visibleBounds: { x: vb.x, y: vb.y, width: vb.width, height: vb.height },
    })
  }
  app.ticker.add(tick)

  // ── View subscription ─────────────────────────────────────────────────────
  // HTML overlay components need to know where each station lands on screen
  // and whether we're at near zoom. Recomputing on every frame would re-
  // render the React tree needlessly — instead we publish snapshots only
  // when the viewport moves or zooms (user interaction) plus on resize.
  const stationHandles: Array<{ index: number; station: StationHandle }> = []
  nodes.forEach((n, index) => {
    if (n.kind === 'station') stationHandles.push({ index, station: n })
  })

  const computeSnapshot = (): ViewSnapshot => {
    const scale = scene.scaled
    const stations = stationHandles.map(({ index, station }) => {
      const { w, h } = station.worldSize
      const tl = scene.toScreen(station.center.x - w / 2, station.center.y - h / 2)
      return {
        id: index,
        eventType: station.eventType,
        screenX: tl.x,
        screenY: tl.y,
        screenW: w * scale,
        screenH: h * scale,
      }
    })
    return { scale, nearZoom: scale >= NEAR_ZOOM_THRESHOLD, stations }
  }

  const viewListeners = new Set<(s: ViewSnapshot) => void>()
  const publish = () => {
    if (viewListeners.size === 0) return
    const snapshot = computeSnapshot()
    for (const cb of viewListeners) cb(snapshot)
  }
  // pixi-viewport emits `moved` during drag and `zoomed` during wheel/pinch.
  // Both fire once per frame of interaction — plenty for overlay tracking
  // while avoiding per-frame work when the user isn't interacting.
  scene.on('moved', publish)
  scene.on('zoomed', publish)
  // Resize via ResizeObserver already calls scene.resize above; re-publish
  // so overlays re-anchor after the canvas changes size.
  const resizeRo = new ResizeObserver(() => publish())
  resizeRo.observe(container)

  return {
    destroy: () => {
      app.ticker.remove(tick)
      ro.disconnect()
      resizeRo.disconnect()
      viewListeners.clear()
      app.destroy(true, { children: true, texture: true })
    },
    onView(cb) {
      viewListeners.add(cb)
      // Fire once immediately so the subscriber starts with a fresh snapshot
      // rather than waiting for the next interaction.
      cb(computeSnapshot())
      return () => {
        viewListeners.delete(cb)
      }
    },
    setEntityPool(entities) {
      items.setEntityPool(entities)
    },
  }
}

// ─── Belt ────────────────────────────────────────────────────────────────────

interface FlatBelt extends Belt {
  tangentAt: (t: number) => { x: number; y: number }
  length: number
}

function buildBelt(
  parent: Container,
  from: Port,
  to: Port,
  opts: { visible?: boolean; tunnel?: boolean } = {},
): FlatBelt {
  const { visible = true, tunnel = false } = opts
  // S-curve conveyor — cubic bezier that exits `from` tangent to the port's
  // outward direction and arrives at `to` tangent to -to.dir (i.e. moving
  // INTO the port). This makes belts emerge smoothly from horizontal port
  // stubs no matter where the target station sits, and gives shared-port
  // situations clear directional read: two belts at the same port have
  // opposite tangents and their chevrons/items flow in opposite directions.
  //
  // Polygon rendering samples the curve N times, computes a CCW-perpendicular
  // edge offset at each sample, and stitches near/far edge strips into a
  // filled body + separate highlight/shadow strokes. Perpendicular is
  // recomputed per-sample so the belt width stays uniform under curvature;
  // lighting consistency (highlight always on one specific geometric side)
  // is a tradeoff we accept against width distortion.
  const c = new Container()
  parent.addChild(c)

  const dist = Math.hypot(to.x - from.x, to.y - from.y)
  // Offset magnitude for the control points. For opposite-facing collinear
  // ports (the common station-to-station horizontal case), the curve
  // overshoots and S-loops when `offset > dist/2` because P1 ends up past
  // P3 along the flow axis. Keep offset strictly below half the distance
  // to guarantee no overshoot, and cap at 160 so long arcs don't balloon.
  // Short belts naturally become near-straight, which is exactly what we
  // want for close primitives on the same row or column.
  const offset = Math.min(160, dist * 0.4)

  // Regular belts have OUTWARD control points (item leaves source going
  // in port.dir, enters destination going in -port.dir). Tunnel edges
  // flip this: items ENTER the tunnel through the source's external port
  // (heading inward, opposite to port.dir) and EXIT through the
  // destination's external port (also inward from the tunnel's
  // perspective). Inverting the offset sign gives tangents that track
  // the actual flow direction instead of swooping back out through the
  // external ports — otherwise the bezier loops the item around before
  // fading in/out.
  const offsetSign = tunnel ? -1 : 1
  const P0 = { x: from.x, y: from.y }
  const P1 = {
    x: from.x + from.dir.x * offset * offsetSign,
    y: from.y + from.dir.y * offset * offsetSign,
  }
  const P2 = {
    x: to.x + to.dir.x * offset * offsetSign,
    y: to.y + to.dir.y * offset * offsetSign,
  }
  const P3 = { x: to.x, y: to.y }

  const pointAt = (t: number) => {
    const u = 1 - t
    return {
      x: u * u * u * P0.x + 3 * u * u * t * P1.x + 3 * u * t * t * P2.x + t * t * t * P3.x,
      y: u * u * u * P0.y + 3 * u * u * t * P1.y + 3 * u * t * t * P2.y + t * t * t * P3.y,
    }
  }

  const tangentAt = (t: number) => {
    const u = 1 - t
    return {
      x: 3 * u * u * (P1.x - P0.x) + 6 * u * t * (P2.x - P1.x) + 3 * t * t * (P3.x - P2.x),
      y: 3 * u * u * (P1.y - P0.y) + 6 * u * t * (P2.y - P1.y) + 3 * t * t * (P3.y - P2.y),
    }
  }

  // Sample the curve and compute perpendicular edge points.
  const N = 28
  const hw = BELT_WIDTH / 2
  const nearPts: Array<{ x: number; y: number }> = []
  const farPts: Array<{ x: number; y: number }> = []
  let length = 0
  let prev: { x: number; y: number } | null = null

  for (let i = 0; i <= N; i++) {
    const t = i / N
    const p = pointAt(t)
    const tan = tangentAt(t)
    const tlen = Math.hypot(tan.x, tan.y) || 1
    // CCW-perpendicular to tangent. Consistent throughout the curve so the
    // polygon doesn't self-intersect; may not always point toward smaller y.
    const nx = tan.y / tlen
    const ny = -tan.x / tlen
    nearPts.push({ x: p.x + nx * hw, y: p.y + ny * hw })
    farPts.push({ x: p.x - nx * hw, y: p.y - ny * hw })

    if (prev) length += Math.hypot(p.x - prev.x, p.y - prev.y)
    prev = p
  }

  // Only render visible belt geometry when requested. Tunnel edges pass
  // visible=false — they still need pointAt/tangentAt for item movement,
  // but no surface is drawn (items fade to invisible during traversal).
  if (visible) {
    // Body polygon: near edge forward, far edge reverse, close.
    const body = new Graphics()
    body.moveTo(nearPts[0].x, nearPts[0].y)
    for (let i = 1; i <= N; i++) body.lineTo(nearPts[i].x, nearPts[i].y)
    for (let i = N; i >= 0; i--) body.lineTo(farPts[i].x, farPts[i].y)
    body.closePath()
    body.fill({ color: 0xffffff, alpha: 0.82 })
    c.addChild(body)

    // Warm tint (same shape).
    const tint = new Graphics()
    tint.moveTo(nearPts[0].x, nearPts[0].y)
    for (let i = 1; i <= N; i++) tint.lineTo(nearPts[i].x, nearPts[i].y)
    for (let i = N; i >= 0; i--) tint.lineTo(farPts[i].x, farPts[i].y)
    tint.closePath()
    tint.fill({ color: COLOR_ACCENT, alpha: 0.1 })
    c.addChild(tint)

    // Near-edge highlight stroke along the curve.
    const topEdge = new Graphics()
    topEdge.moveTo(nearPts[0].x, nearPts[0].y)
    for (let i = 1; i <= N; i++) topEdge.lineTo(nearPts[i].x, nearPts[i].y)
    topEdge.stroke({ width: 1.25, color: 0xffffff, alpha: 0.95 })
    c.addChild(topEdge)

    // Far-edge shadow stroke along the curve.
    const botEdge = new Graphics()
    botEdge.moveTo(farPts[0].x, farPts[0].y)
    for (let i = 1; i <= N; i++) botEdge.lineTo(farPts[i].x, farPts[i].y)
    botEdge.stroke({ width: 1.25, color: 0x000000, alpha: 0.18 })
    c.addChild(botEdge)
  }

  return { pointAt, tangentAt, length }
}

function buildBeltChevrons(parent: Container, belt: FlatBelt) {
  // Small ">" marks along the belt that slide in the flow direction. Each
  // chevron re-rotates every frame to match the local belt tangent at its
  // current t, so on curved belts the chevrons lean along the curve
  // naturally. Spacing is length-based so long and short belts look
  // consistent rather than scaling mark density with distance.
  const container = new Container()
  parent.addChild(container)

  const CHEVRON_SPACING = 36
  const count = Math.max(2, Math.round(belt.length / CHEVRON_SPACING))

  const chevrons = Array.from({ length: count }, () => {
    const g = new Graphics()
    g.moveTo(-4, -3)
    g.lineTo(1, 0)
    g.lineTo(-4, 3)
    g.stroke({ width: 1.25, color: COLOR_ACCENT, alpha: 0.55 })
    container.addChild(g)
    return g
  })

  let t = 0
  const speed = 0.64

  return {
    update(dt: number) {
      t = (t + dt * speed) % 1
      for (let i = 0; i < chevrons.length; i++) {
        const u = (t + i / chevrons.length) % 1
        const pos = belt.pointAt(u)
        const tan = belt.tangentAt(u)
        chevrons[i].x = pos.x
        chevrons[i].y = pos.y
        chevrons[i].rotation = Math.atan2(tan.y, tan.x)
        const fade = Math.min(1, u / 0.08, (1 - u) / 0.08)
        chevrons[i].alpha = 0.25 + 0.4 * Math.max(0, fade)
      }
    },
  }
}

// ─── Items ───────────────────────────────────────────────────────────────────

interface Edge {
  from: number
  to: number
  belt: FlatBelt
  isTunnel: boolean
}

function buildItemSpawner(
  parent: Container,
  nodes: GraphNode[],
  edges: Edge[],
  eventTypeToNode: Map<string, number>,
) {
  // Items are entity-identified, not script-driven: each real entity gets
  // exactly one item, parked at the station matching its
  // `current_event_type`. When a snapshot update shifts an entity's current
  // station, the item animates along the belt network to the new station.
  // Entities whose current_event_type doesn't correspond to any station on
  // the board are skipped (system events, non-visualized PR events).
  //
  // `setEntityPool(entities)` is the reconciler — it diffs the new pool
  // against existing items: new entity → spawn parked, changed station →
  // retarget, missing entity → despawn. Static data ⇒ static floor.
  const layer = new Container()
  parent.addChild(layer)

  const LEG_DURATION = 1.4
  const MAX_HOPS = 40

  const outgoingByNode = new Map<number, Edge[]>()
  for (const e of edges) {
    const list = outgoingByNode.get(e.from) ?? []
    list.push(e)
    outgoingByNode.set(e.from, list)
  }

  // Precompute next-hop: for every (from, to) pair, the first edge to take
  // when traveling from `from` to reach `to` along a shortest path.
  // Cycles in the graph (the ci_failed → new_commits loopback) are handled
  // correctly by BFS — visited-set prevents re-expansion.
  const nextHop = new Map<string, Edge>()
  for (let src = 0; src < nodes.length; src++) {
    const firstEdge = new Map<number, Edge>()
    const visited = new Set<number>([src])
    const queue: number[] = [src]
    while (queue.length > 0) {
      const curr = queue.shift()!
      const outs = outgoingByNode.get(curr) ?? []
      for (const e of outs) {
        if (visited.has(e.to)) continue
        visited.add(e.to)
        const rootEdge = curr === src ? e : firstEdge.get(curr)!
        firstEdge.set(e.to, rootEdge)
        queue.push(e.to)
      }
    }
    for (const [tgt, e] of firstEdge) {
      nextHop.set(`${src}:${tgt}`, e)
    }
  }

  const edgeToward = (fromNode: number, target: number): Edge | null => {
    if (fromNode === target) return null
    return nextHop.get(`${fromNode}:${target}`) ?? null
  }

  interface ItemMeta {
    /** Stable label for the compact pill. For GitHub entities: `PR #<number>`;
     * for Jira entities: the ticket key (e.g. `SKY-123`). */
    label: string
    mine: boolean
    title: string
    repo: string
    author: string
    diffAdd: number
    diffDel: number
  }

  interface Item {
    entityId: string
    /** Compact label used in the station's mid-zoom entity strip. Stored
     * alongside the Pixi text so restack can hand it back to the station
     * without re-deriving from entity metadata. */
    label: string
    mine: boolean
    gfx: Container
    /** Compact pill — label only. Shown at mid / far zoom or when the
     * item is off-screen at near zoom. */
    compactGroup: Container
    /** Expanded card — title, repo, author, diff. Shown only when near
     * zoom AND the item is inside the viewport's visible world rect. */
    detailGroup: Container
    /** Station node index where the item is currently parked. -1 while
     * traveling between stations. */
    parkedAt: number
    /** Station node index the item is heading toward. -1 when parked. */
    target: number
    /** Edge currently being traversed. Null while parked. */
    currentEdge: Edge | null
    t: number
    hopsRemaining: number
    /** FIFO queue of station node indices still to visit AFTER the
     * current parkedAt / target. Each snapshot update appends new
     * events (those with timestamp > lastSeenEventAt) so multi-event
     * poll cycles show the full progression rather than teleporting
     * to the latest. Repeated events (ci→new_commits→ci→new_commits)
     * stay distinct instead of collapsing against the current anchor. */
    chain: number[]
    /** ISO timestamp of the most recent event we've already incorporated
     * into this item's chain. Events with `at` strictly greater than
     * this value on the next reconcile are the NEW ones to append. */
    lastSeenEventAt: string
  }

  const items = new Map<string, Item>()

  const ITEM_LIFT = 12
  const ITEM_W = 60
  const ITEM_H = 22

  // Detail card — shown at near zoom instead of the compact pill. Sized
  // small enough to fit between adjacent stations on the main row without
  // overflowing into the next station (belt span there is ~60 world units,
  // so some visual overlap is inevitable; the chosen width is a compromise
  // between legibility and not engulfing neighbours).
  const DETAIL_W = 140
  const DETAIL_H = 56

  // Ownership tint — warm terracotta for entities where the session user
  // authored the PR, cooler muted blue for entities authored by others.
  // We track every PR we can see, so the floor should show both.
  const TINT_MINE = COLOR_ACCENT // 0xc47a5a terracotta
  const TINT_OTHER = 0x7a9aad // muted slate-blue

  const metaFromEntity = (e: FactoryEntity): ItemMeta => {
    const label =
      e.source === 'github' && e.number ? `PR #${e.number}` : e.source_id || e.title.slice(0, 18)
    return {
      label,
      mine: e.mine,
      title: e.title || e.source_id,
      repo: e.repo ?? e.source_id ?? '',
      author: e.source === 'github' ? (e.author ? `@${e.author}` : '') : e.assignee || '',
      diffAdd: e.additions ?? 0,
      diffDel: e.deletions ?? 0,
    }
  }

  // Pixi Text rasters at creation-time DPI. A 3× resolution keeps text
  // crisp up to the NEAR zoom ceiling without rebuilding textures when
  // the viewport zooms.
  const TEXT_RES = 3

  const createItem = (entityId: string, meta: ItemMeta, parkedAt: number): Item => {
    const g = new Container()
    layer.addChild(g)
    const tint = meta.mine ? TINT_MINE : TINT_OTHER

    // Shadow stays on the belt surface regardless of which LOD body is
    // showing — gives both the compact pill and the expanded card the
    // "floating above conveyor" lift.
    const shadow = new Graphics()
    shadow.ellipse(0, 2, ITEM_W / 2 - 2, 4)
    shadow.fill({ color: 0x000000, alpha: 0.22 })
    g.addChild(shadow)

    // ── Compact pill (mid / far LOD) ──────────────────────────────────────
    const compactGroup = new Container()
    compactGroup.y = -ITEM_LIFT
    g.addChild(compactGroup)

    const bg = new Graphics()
    bg.roundRect(-ITEM_W / 2, -ITEM_H / 2, ITEM_W, ITEM_H, ITEM_H / 2)
    bg.fill({ color: 0xffffff, alpha: 0.97 })
    bg.stroke({ width: 1, color: tint, alpha: 0.55 })
    compactGroup.addChild(bg)

    const inner = new Graphics()
    inner.roundRect(-ITEM_W / 2 + 2, -ITEM_H / 2 + 2, ITEM_W - 4, ITEM_H - 4, (ITEM_H - 4) / 2)
    inner.fill({ color: tint, alpha: 0.1 })
    compactGroup.addChild(inner)

    const topHighlight = new Graphics()
    topHighlight.moveTo(-ITEM_W / 2 + 6, -ITEM_H / 2 + 1.5)
    topHighlight.lineTo(ITEM_W / 2 - 6, -ITEM_H / 2 + 1.5)
    topHighlight.stroke({ width: 1, color: 0xffffff, alpha: 0.9 })
    compactGroup.addChild(topHighlight)

    const compactText = new Text({
      text: meta.label,
      resolution: TEXT_RES,
      style: {
        fontFamily: 'Inter, system-ui, sans-serif',
        fontSize: 11,
        fontWeight: '600',
        fill: tint,
        letterSpacing: 0.2,
      },
    })
    compactText.anchor.set(0.5, 0.5)
    compactGroup.addChild(compactText)

    // ── Expanded detail card (near LOD) ───────────────────────────────────
    // Anchored above the belt the same way as the compact pill, just a
    // bigger vertical offset so its bottom edge aligns with the belt top
    // rather than the belt center.
    const detailGroup = new Container()
    detailGroup.y = -ITEM_LIFT - (DETAIL_H - ITEM_H) / 2
    detailGroup.visible = false // toggled by update() per LOD + culling
    g.addChild(detailGroup)

    const dHW = DETAIL_W / 2
    const dHH = DETAIL_H / 2

    const detailBg = new Graphics()
    detailBg.roundRect(-dHW, -dHH, DETAIL_W, DETAIL_H, 8)
    detailBg.fill({ color: 0xffffff, alpha: 0.97 })
    detailBg.stroke({ width: 1, color: tint, alpha: 0.55 })
    detailGroup.addChild(detailBg)

    const detailInner = new Graphics()
    detailInner.roundRect(-dHW + 2, -dHH + 2, DETAIL_W - 4, DETAIL_H - 4, 7)
    detailInner.fill({ color: tint, alpha: 0.06 })
    detailGroup.addChild(detailInner)

    const detailTopHighlight = new Graphics()
    detailTopHighlight.moveTo(-dHW + 8, -dHH + 1.5)
    detailTopHighlight.lineTo(dHW - 8, -dHH + 1.5)
    detailTopHighlight.stroke({ width: 1, color: 0xffffff, alpha: 0.9 })
    detailGroup.addChild(detailTopHighlight)

    const padX = 8
    const titleY = -14
    const repoY = 0
    const footerY = 14

    // Fit a Pixi Text inside a max width by iteratively trimming characters
    // and re-measuring. Character-count truncation is unreliable with
    // proportional fonts (w is wider than i), so we rely on Pixi's actual
    // rasterized width instead. Cheap — only runs at item creation.
    const fitText = (t: Text, original: string, maxWidth: number) => {
      if (t.width <= maxWidth) return
      let s = original
      while (s.length > 2 && t.width > maxWidth) {
        s = s.slice(0, -1)
        t.text = s.trimEnd() + '…'
      }
    }
    const innerW = DETAIL_W - padX * 2

    const titleText = new Text({
      text: meta.title,
      resolution: TEXT_RES,
      style: {
        fontFamily: 'Inter, system-ui, sans-serif',
        fontSize: 11,
        fontWeight: '700',
        fill: TEXT_PRIMARY,
        letterSpacing: 0.1,
      },
    })
    titleText.anchor.set(0, 0.5)
    titleText.x = -dHW + padX
    titleText.y = titleY
    fitText(titleText, meta.title, innerW)
    detailGroup.addChild(titleText)

    const repoText = new Text({
      text: meta.repo,
      resolution: TEXT_RES,
      style: {
        fontFamily: 'Inter, system-ui, sans-serif',
        fontSize: 8,
        fontWeight: '500',
        fill: TEXT_TERTIARY,
        letterSpacing: 0.2,
      },
    })
    repoText.anchor.set(0, 0.5)
    repoText.x = -dHW + padX
    repoText.y = repoY
    fitText(repoText, meta.repo, innerW)
    detailGroup.addChild(repoText)

    const authorText = new Text({
      text: meta.author,
      resolution: TEXT_RES,
      style: {
        fontFamily: 'Inter, system-ui, sans-serif',
        fontSize: 8,
        fontWeight: '600',
        fill: tint,
        letterSpacing: 0.3,
      },
    })
    authorText.anchor.set(0, 0.5)
    authorText.x = -dHW + padX
    authorText.y = footerY
    detailGroup.addChild(authorText)

    const diffText = new Text({
      text: `+${meta.diffAdd} −${meta.diffDel}`,
      resolution: TEXT_RES,
      style: {
        fontFamily: 'SF Mono, Fira Code, monospace',
        fontSize: 8,
        fontWeight: '500',
        fill: TEXT_SECONDARY,
      },
    })
    diffText.anchor.set(1, 0.5)
    diffText.x = dHW - padX
    diffText.y = footerY
    detailGroup.addChild(diffText)

    // Seed the item parked at its station. Position is applied by the
    // update loop; we just record the state here.
    return {
      entityId,
      label: meta.label,
      mine: meta.mine,
      gfx: g,
      compactGroup,
      detailGroup,
      parkedAt,
      target: -1,
      currentEdge: null,
      t: 0,
      hopsRemaining: MAX_HOPS,
      chain: [],
      lastSeenEventAt: '',
    }
  }

  const despawn = (item: Item) => {
    layer.removeChild(item.gfx)
    item.gfx.destroy({ children: true })
    items.delete(item.entityId)
  }

  // Precomputed station rects (AABBs) in world coords. Items near a
  // station fall back to the compact pill at near zoom so the expanded
  // detail card never slips half-behind the station's HTML overlay —
  // that overlay renders on a DOM layer above the canvas, so there's no
  // z-index trick to push items in front of it. Hiding detail inside
  // the station zone turns "item disappears behind overlay" into "item
  // dives into the station" visually.
  const STATION_MARGIN = 12
  const DETAIL_HALF_W = DETAIL_W / 2
  const DETAIL_HALF_H = DETAIL_H / 2
  const DETAIL_CENTER_Y_OFFSET = -ITEM_LIFT - (DETAIL_H - ITEM_H) / 2
  const stationRects: Array<{ cx: number; cy: number; hw: number; hh: number }> = []
  for (const n of nodes) {
    if (n.kind === 'station') {
      stationRects.push({
        cx: n.center.x,
        cy: n.center.y,
        hw: n.worldSize.w / 2,
        hh: n.worldSize.h / 2,
      })
    }
  }
  const detailOverlapsAnyStation = (itemX: number, itemY: number): boolean => {
    const cx = itemX
    const cy = itemY + DETAIL_CENTER_Y_OFFSET
    for (const r of stationRects) {
      if (
        Math.abs(cx - r.cx) < DETAIL_HALF_W + r.hw + STATION_MARGIN &&
        Math.abs(cy - r.cy) < DETAIL_HALF_H + r.hh + STATION_MARGIN
      ) {
        return true
      }
    }
    return false
  }

  interface ViewContext {
    scale: number
    /** Viewport's visible world rect. Used to cull expanded-detail
     * updates — off-screen items stay in compact form so Pixi can skip
     * drawing their detail text even at near zoom. */
    visibleBounds: { x: number; y: number; width: number; height: number }
  }

  // Recompute parked-item stacking ordinals. Multiple entities can share a
  // station (e.g., 5 open PRs all at github:pr:opened); giving each a
  // distinct stackIdx offsets them vertically so the pile reads as N items
  // rather than 1.
  const restack = () => {
    const byStation = new Map<number, Item[]>()
    for (const it of items.values()) {
      if (it.parkedAt < 0) continue
      const list = byStation.get(it.parkedAt) ?? []
      list.push(it)
      byStation.set(it.parkedAt, list)
    }
    for (const list of byStation.values()) {
      // Deterministic order so the same set of items at a station doesn't
      // reshuffle on every reconcile — sort by entityId. Feeds the
      // station's entity-pill strip (and, indirectly, the near-zoom
      // overlay's waiting list) in stable order.
      list.sort((a, b) => (a.entityId < b.entityId ? -1 : 1))
    }
    publishStationCounts(byStation)
    publishStationEntities(byStation)
  }

  // Push the current list of parked entities per station into its handle,
  // driving the mid-zoom entity-pills strip that replaces predicate chips
  // when items are waiting. Order follows the restack sort so the pill
  // order matches the vertical stacking used at zoomed-in views.
  const publishStationEntities = (byStation: Map<number, Item[]>) => {
    for (let i = 0; i < nodes.length; i++) {
      const n = nodes[i]
      if (n.kind !== 'station') continue
      const list = byStation.get(i) ?? []
      n.setEntities(list.map((it) => ({ label: it.label, mine: it.mine })))
    }
  }

  // Push per-station item counts into the station handles. Drives the
  // far-view count badge (setItemCount on each station). Called from
  // restack so every change to the parked set — reconcile, arrivals,
  // despawns — refreshes the counts.
  const publishStationCounts = (byStation?: Map<number, Item[]>) => {
    const counts = new Map<number, number>()
    if (byStation) {
      for (const [idx, list] of byStation) counts.set(idx, list.length)
    } else {
      for (const it of items.values()) {
        if (it.parkedAt < 0) continue
        counts.set(it.parkedAt, (counts.get(it.parkedAt) ?? 0) + 1)
      }
    }
    for (let i = 0; i < nodes.length; i++) {
      const n = nodes[i]
      if (n.kind !== 'station') continue
      n.setItemCount(counts.get(i) ?? 0)
    }
  }

  // Map one of the entity's events to its station node index, returning
  // undefined if the event type isn't visualized on the board.
  const eventToNode = (eventType: string): number | undefined => eventTypeToNode.get(eventType)

  return {
    setEntityPool(next: FactoryEntity[]) {
      const seen = new Set<string>()
      for (const e of next) {
        const recent = e.recent_events ?? []
        // Pick the station this entity should CURRENTLY be at — the last
        // visualized station in its event history. Entities whose entire
        // history is non-visualized (system events only) are skipped.
        let latestStation = -1
        let latestAt = ''
        if (recent.length > 0) {
          for (let i = recent.length - 1; i >= 0; i--) {
            const n = eventToNode(recent[i].event_type)
            if (n != null) {
              latestStation = n
              latestAt = recent[i].at
              break
            }
          }
        } else if (e.current_event_type) {
          // Backward compat: snapshots without recent_events still park
          // the item at its current event type.
          const n = eventToNode(e.current_event_type)
          if (n != null) {
            latestStation = n
            latestAt = e.last_event_at ?? ''
          }
        }
        if (latestStation < 0) continue
        seen.add(e.id)

        const existing = items.get(e.id)
        if (existing) {
          // Append events that are NEW since we last reconciled. Identity
          // is timestamp-based, not event-type-based — a repeated event
          // type (ci_passed → new_commits → ci_passed) produces distinct
          // chain entries rather than collapsing on the anchor.
          const newEvents = recent.filter((ev) => ev.at > existing.lastSeenEventAt)
          for (const ev of newEvents) {
            const n = eventToNode(ev.event_type)
            if (n == null) continue
            // Dedupe against the immediate predecessor — the item's
            // current destination (chain tail if queued, else its
            // active target, else its parked station). Prevents a
            // ci_passed event firing while the item is already heading
            // to ci_passed from adding a redundant hop.
            const tail =
              existing.chain.length > 0
                ? existing.chain[existing.chain.length - 1]
                : existing.target >= 0
                  ? existing.target
                  : existing.parkedAt
            if (n === tail) continue
            existing.chain.push(n)
          }
          if (newEvents.length > 0) {
            existing.lastSeenEventAt = newEvents[newEvents.length - 1].at
          }
          // If we're parked with something new queued, kick off travel
          // now rather than waiting for a later tick. Travel logic in
          // update() handles further hops as each leg completes.
          if (existing.parkedAt >= 0 && !existing.currentEdge && existing.chain.length > 0) {
            const nextStation = existing.chain.shift()!
            const first = edgeToward(existing.parkedAt, nextStation)
            if (first) {
              existing.target = nextStation
              existing.currentEdge = first
              existing.t = 0
              existing.hopsRemaining = MAX_HOPS
              existing.parkedAt = -1
            }
          }
        } else {
          // New entity: park at the latest visualized station, do NOT
          // animate historical events. First mount of the factory
          // shouldn't replay a PR's entire event history — only events
          // that fire after the item exists produce movement.
          const item = createItem(e.id, metaFromEntity(e), latestStation)
          item.lastSeenEventAt = latestAt
          items.set(e.id, item)
        }
      }

      // Despawn items whose entity has left the pool (closed, merged,
      // dropped off the 100-entity window).
      for (const it of Array.from(items.values())) {
        if (!seen.has(it.entityId)) despawn(it)
      }

      restack()
      publishStationCounts()
    },
    update(dt: number, view: ViewContext) {
      const farZoom = view.scale < FAR_ZOOM_THRESHOLD
      const nearZoom = view.scale >= NEAR_ZOOM_THRESHOLD
      const vb = view.visibleBounds

      // At far zoom individual item pills are unreadable and clutter the
      // glyph — stations carry the count badge instead. Hide the whole
      // item layer rather than iterating per-item to flip visibility.
      if (farZoom) {
        for (const it of items.values()) it.gfx.visible = false
        return
      }

      for (const it of items.values()) {
        // Parked items sit at station center, offset by their stack ordinal.
        // No per-frame pathfinding, no alpha animation — static unless a
        // snapshot update retargets them.
        //
        // At mid zoom, parked items don't render on-canvas at all — the
        // station handles that itself via setEntities (entity-pill strip
        // replaces predicate chips). Keeping gfx hidden for parked items
        // avoids the duplicate "item pill at station center AND entity
        // pill in the bottom strip" redundancy and clears the core
        // chamber for the glyph.
        if (it.parkedAt >= 0 && !it.currentEdge) {
          // Parked items are represented by the station itself, not by
          // the on-canvas pill: mid-zoom → station's entity-pill strip;
          // near-zoom → HTML overlay's waiting strip; far-zoom → count
          // badge. The core/chamber stays clear for the glyph.
          it.gfx.visible = false
        } else if (it.currentEdge) {
          // Traveling. Advance along the edge; on arrival at an
          // intermediate station we just switch to the next edge — no
          // dwell, no parking. Intermediate stations read as pure
          // passthrough: nothing fired here, the item just flowed past.
          // Only the terminal (chain exhausted) parks.
          it.t += dt / LEG_DURATION
          if (it.t >= 1) {
            const atNode = it.currentEdge.to
            const reachedTarget = atNode === it.target
            const outOfHops = it.hopsRemaining <= 0

            // Terminal arrival: target with no more chain, or safety cap.
            if ((reachedTarget && it.chain.length === 0) || outOfHops) {
              it.parkedAt = atNode
              it.target = -1
              it.currentEdge = null
              it.t = 0
              restack()
              continue
            }

            // Intermediate: either we reached the current target and
            // there's more chain, or we're transiting a splitter/merger
            // between targets. Either way, pick the next target+edge.
            if (reachedTarget) {
              const nextTarget = it.chain.shift()!
              const next = edgeToward(atNode, nextTarget)
              if (!next) {
                // Can't reach next station — park here; future snapshot
                // may retarget via an available route.
                it.parkedAt = atNode
                it.target = -1
                it.currentEdge = null
                it.t = 0
                restack()
                continue
              }
              it.target = nextTarget
              it.currentEdge = next
              it.t = 0
              it.hopsRemaining -= 1
            } else {
              const next = edgeToward(atNode, it.target)
              if (!next) {
                it.parkedAt = atNode
                it.target = -1
                it.currentEdge = null
                it.t = 0
                restack()
                continue
              }
              it.currentEdge = next
              it.t = 0
              it.hopsRemaining -= 1
            }
          }

          const pos = it.currentEdge!.belt.pointAt(it.t)
          it.gfx.x = pos.x
          it.gfx.y = pos.y
          it.gfx.visible = true
          if (it.currentEdge!.isTunnel) {
            let alpha = 0
            if (it.t < 0.2) alpha = 1 - it.t / 0.2
            else if (it.t > 0.8) alpha = (it.t - 0.8) / 0.2
            it.gfx.alpha = Math.max(0, alpha)
          } else {
            it.gfx.alpha = 1
          }
        } else {
          it.gfx.visible = false
        }

        // LOD + culling. At near zoom the item switches to the expanded
        // detail card, but ONLY when:
        //   - currently inside the viewport's visible world rect, AND
        //   - not overlapping any station's footprint, since stations
        //     have HTML detail overlays at near zoom and we can't raise
        //     canvas-drawn items above DOM overlays.
        const px = it.gfx.x
        const py = it.gfx.y
        const inView = px >= vb.x && px <= vb.x + vb.width && py >= vb.y && py <= vb.y + vb.height
        const showDetail = nearZoom && inView && !detailOverlapsAnyStation(px, py)
        it.compactGroup.visible = !showDetail
        it.detailGroup.visible = showDetail
      }
    },
  }
}
