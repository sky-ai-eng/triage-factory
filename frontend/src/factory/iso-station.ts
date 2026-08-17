// Stage-1 station, built as a hierarchy of Babylon meshes.
//
// Aesthetic: liquid glass + Halo Reach + Transcendence — a warm
// ceramic chassis with two flat working surfaces on top: a small
// intake tray on the left where queued entities wait, and a larger
// work tray on the right where active runs sit. Both trays are
// shallow CSG-cut insets framed by glowing LED perimeters; chips
// sit flat on the tray floors. A laser scanner bar slowly
// translates across the work tray to sell "machine doing work".
// Body chassis (vents, heatsink, anchor discs, status panel) is
// unchanged.
//
// Form factor:
//
//   ┌──────────────────────────────────────────┐
//   │ ┌──────┐  ┌──[ STATION LABEL ]────────┐  │
//   │ │ ▢ ▢  │  │   ════════════════════    │  │  ← scanner bar
//   │ │ ▢ ▢  │  │   ▢ ▢ ▢ ▢ ▢ ▢ ▢ ▢ ▢      │  │  ← run chips
//   │ │ ▢ ▢  │  │   ▢ ▢ ▢ ▢ ▢ ▢ ▢ ▢ ▢      │  │
//   │ └──────┘  └───────────────────────────┘  │
//   └──────────────────────────────────────────┘
//      INTAKE        MAIN WORK TRAY           (heatsink/etc)
//      TRAY            (active runs)
//
// Both trays are carved from the body via CSG.subtract, so each
// lip is a real geometric step on a single mesh. Chip pools are
// pre-built up to capacity and toggled visible based on the
// station handle's setQueuedCount / setConversationCount calls.

import {
  Color3,
  CSG,
  DynamicTexture,
  Material,
  Mesh,
  MeshBuilder,
  PBRMaterial,
  Scene,
  Texture,
  TransformNode,
  Vector3,
} from '@babylonjs/core'

import { buildBelt, createBeltMaterial, type BeltBuild } from './iso-belt'
import {
  CONVEYOR_HEIGHT,
  CONVEYOR_WIDTH,
  PORT_RECESS_MARGIN_ABOVE,
  PORT_RECESS_MARGIN_ACROSS,
  type Port,
  type PortHandle,
} from './iso-port'

export interface Station {
  /** World position of the station's bottom-back-left corner. */
  x: number
  y: number
  z: number
  /** Footprint and height. */
  w: number
  d: number
  h: number
  /** Stable id used for click hit-testing — set on the station's
   *  TransformNode metadata so the scene picker can route events
   *  back to a known station. */
  id?: string
  /** Initial visible queued chips on the intake tray floor (capped
   *  at MAX_QUEUED). The handle's setQueuedCount can update this
   *  later. */
  queuedCount?: number
  /** Initial visible run chips on the main tray floor (capped at
   *  MAX_RUNS). One per active run; setConversationCount updates dynamically. */
  conversationCount?: number
  /** Etched identity label rendered along the back of the main tray
   *  floor. Empty/undefined → no label plate. */
  label?: string
  /** Conveyor attach points on the station's walls. Each produces a
   *  recess + LED frame + internal belt stub. */
  ports?: Port[]
}

/** Live handle on a station after its meshes are built. Holds
 *  setters for the dynamic chip counts plus the bounds we'd need
 *  for projecting world points (e.g. for HTML overlays anchored
 *  to the station). */
export interface StationHandle {
  root: TransformNode
  ports: PortHandle[]
  /** Station's footprint in world coords (matches the spec). */
  bounds: { x: number; y: number; w: number; d: number; h: number }
  /** Station's stable id, mirrored from the spec; undefined if the
   *  spec didn't set one. */
  id: string | undefined
  /** Show n queued chips. n is clamped to MAX_QUEUED; chips above
   *  the cap are not rendered (caller surfaces overflow elsewhere). */
  setQueuedCount(n: number): void
  /** Show n run chips. n clamped to MAX_RUNS. */
  setConversationCount(n: number): void
  /** Update the lifetime counter rendered on the station's front-face
   *  status screen. Diff-gated internally — calling with the same
   *  value as last call is a no-op, so per-frame reconcilers can call
   *  it cheaply. */
  setLifetimeCount(n: number): void
}

// ─── Tray fractions ─────────────────────────────────────────────────────────

/** Main work tray — right portion of the top, holds run chips. */
const MAIN_TRAY_X_FRAC: readonly [number, number] = [0.3, 0.95]
const MAIN_TRAY_Y_FRAC: readonly [number, number] = [0.15, 0.85]

/** Intake tray — left portion, holds queued chips. */
const INTAKE_TRAY_X_FRAC: readonly [number, number] = [0.04, 0.24]
const INTAKE_TRAY_Y_FRAC: readonly [number, number] = [0.15, 0.85]

/** Both trays share depth and floor lift. */
const TRAY_DEPTH = 6
const TRAY_FLOOR_LIFT = 0.4

// ─── LED perimeter ────────────────────────────────────────────────────────
// Four thin emissive bars sitting on the body's top surface,
// tracing the inside edge of each tray opening.

const LED_PERIM_WIDTH = 1.4
const LED_PERIM_HEIGHT = 1.4
const LED_PERIM_INSET = 0.6
const LED_PERIM_END_INSET = 1.4

// ─── Side details ──────────────────────────────────────────────────────────

const VENT_COUNT = 5
const VENT_WIDTH_FRAC = 0.55
const VENT_HEIGHT = 2.4
const VENT_DEPTH = 6
const VENT_Z_FRAC_RANGE: readonly [number, number] = [0.3, 0.85]
const VENT_GLOW_INSET = 1.4
const VENT_GLOW_THICKNESS = 0.5

const STATUS_PANEL_X_FRAC = 0.7
const STATUS_PANEL_W = 64
const STATUS_PANEL_H = 28
const STATUS_PANEL_DEPTH = 3
const STATUS_PANEL_Z_FRAC = 0.5
const STATUS_SCREEN_INSET = 1.6
const STATUS_SCREEN_THICKNESS = 0.5

const HEATSINK_FIN_COUNT = 7
const HEATSINK_FIN_EXTENT = 8
const HEATSINK_FIN_THICKNESS = 2.6
const HEATSINK_FIN_HEIGHT_FRAC = 0.6
const HEATSINK_FIN_SPACING = 7
const HEATSINK_FIN_Z_FRAC = 0.5
const HEATSINK_FIN_Y_FRAC = 0.88
const HEATSINK_HOUSING_PADDING_Y = 5
const HEATSINK_HOUSING_PADDING_Z = 5
const HEATSINK_HOUSING_DEPTH = 3
const HEATSINK_BACKPLATE_THICKNESS = 1.5

const ANCHOR_DIAM = 14
const ANCHOR_THICKNESS = 1.4
const ANCHOR_X_INSET = 22
const ANCHOR_Z_FRAC = 0.18

// ─── Chip dimensions ──────────────────────────────────────────────────────

const QUEUED_CHIP_DIAM = 22
const QUEUED_CHIP_H = 6
const QUEUED_CHIP_GAP = 6
const QUEUED_CORE_DIAM = 14
const QUEUED_CORE_H = 2

const RUN_CHIP_DIAM = 22
const RUN_CHIP_H = 7
const RUN_CHIP_GAP = 6
const RUN_CORE_DIAM = 12
const RUN_CORE_H = 3.5

/** Pool sizes — meshes are pre-built up to these limits and
 *  toggled visible based on counts. Sized to the grid that fits
 *  on each tray floor with the chosen chip dimensions. */
const MAX_QUEUED = 10 // intake tray: 2 cols × 5 rows
const MAX_RUNS = 36 // main tray: 9 cols × 4 rows

// ─── Etched label ─────────────────────────────────────────────────────────

const LABEL_TEX_W = 1024
const LABEL_TEX_H = 96
const LABEL_PLATE_DEPTH_FRAC = 0.22
const LABEL_PLATE_BACK_INSET = 5
const LABEL_PLATE_LIFT = 0.5
const LABEL_PLATE_SIDE_MARGIN = 8

// ─── Scanner laser ────────────────────────────────────────────────────────
// A thin emissive bar that slowly translates front-to-back across
// the main tray. Sells "this machine is actively scanning the
// work surface" — the most cost-effective way to make the tray
// feel alive without per-chip animation. One mesh per station,
// animated by a single scene-level observer.

const SCANNER_THICKNESS_Y = 2.2 // beam's y extent (along scan axis)
const SCANNER_THICKNESS_Z = 1.5 // beam's vertical thickness
const SCANNER_LIFT_ABOVE_CHIP = 4
/** Seconds for one full sweep. Linear loop — beam translates from
 *  front to back, snaps back to front, repeats. */
const SCANNER_PERIOD_SECONDS = 2.4

// ─── Chip pulse ───────────────────────────────────────────────────────────
// Each run chip's mesh scale oscillates with a slight phase offset
// per chip — reads as "each run is alive at its own rhythm" without
// per-chip materials.

const PULSE_AMPLITUDE = 0.04 // ±4% scale oscillation
const PULSE_PERIOD_SECONDS = 2.0

// ─── Materials ────────────────────────────────────────────────────────────

export interface StationMaterials {
  body: PBRMaterial
  trayFloor: PBRMaterial
  ledTrim: PBRMaterial
  ventGlow: PBRMaterial
  heatsinkFin: PBRMaterial
  screen: PBRMaterial
  beltSurface: PBRMaterial
  recessInterior: PBRMaterial
  queuedShell: PBRMaterial
  queuedCore: PBRMaterial
  runShell: PBRMaterial
  runCore: PBRMaterial
  scannerBeam: PBRMaterial
}

export function createStationMaterials(scene: Scene): StationMaterials {
  const body = new PBRMaterial('station-body', scene)
  body.albedoColor = Color3.FromHexString('#ece6d8')
  body.metallic = 0
  body.roughness = 0.45
  body.clearCoat.isEnabled = true
  body.clearCoat.intensity = 0.4
  body.clearCoat.roughness = 0.18

  const trayFloor = new PBRMaterial('tray-floor-mat', scene)
  trayFloor.albedoColor = Color3.FromHexString('#46423c')
  trayFloor.metallic = 0.65
  trayFloor.roughness = 0.5

  const ledTrim = new PBRMaterial('led-trim', scene)
  ledTrim.albedoColor = Color3.Black()
  ledTrim.emissiveColor = Color3.FromHexString('#7cf7ec')
  ledTrim.emissiveIntensity = 1.6
  ledTrim.metallic = 0
  ledTrim.roughness = 1

  const ventGlow = new PBRMaterial('vent-glow', scene)
  ventGlow.albedoColor = Color3.Black()
  ventGlow.emissiveColor = Color3.FromHexString('#ff8a3a')
  ventGlow.emissiveIntensity = 1.3
  ventGlow.metallic = 0
  ventGlow.roughness = 1

  const heatsinkFin = new PBRMaterial('heatsink-fin', scene)
  heatsinkFin.albedoColor = Color3.FromHexString('#605b52')
  heatsinkFin.metallic = 0.75
  heatsinkFin.roughness = 0.42

  const screen = new PBRMaterial('status-screen', scene)
  screen.albedoColor = Color3.FromHexString('#15171c')
  screen.metallic = 0.1
  screen.roughness = 0.35
  screen.emissiveColor = Color3.FromHexString('#2a3550')
  screen.emissiveIntensity = 0.25
  screen.clearCoat.isEnabled = true
  screen.clearCoat.intensity = 0.7
  screen.clearCoat.roughness = 0.05

  const beltSurface = createBeltMaterial(scene)

  const recessTex = new DynamicTexture('recess-gradient', { width: 64, height: 256 }, scene, false)
  const recessCtx = recessTex.getContext()
  const recessGrad = recessCtx.createLinearGradient(0, 0, 0, 256)
  recessGrad.addColorStop(0, '#322a22')
  recessGrad.addColorStop(1, '#0a0807')
  recessCtx.fillStyle = recessGrad
  recessCtx.fillRect(0, 0, 64, 256)
  recessTex.update()

  const recessInterior = new PBRMaterial('recess-interior', scene)
  recessInterior.albedoTexture = recessTex
  recessInterior.metallic = 0.25
  recessInterior.roughness = 0.7

  const queuedShell = new PBRMaterial('queued-shell', scene)
  queuedShell.albedoColor = Color3.FromHexString('#ffce7a')
  queuedShell.metallic = 0
  queuedShell.roughness = 0.15
  queuedShell.alpha = 0.55
  queuedShell.transparencyMode = Material.MATERIAL_ALPHABLEND
  queuedShell.indexOfRefraction = 1.45

  const queuedCore = new PBRMaterial('queued-core', scene)
  queuedCore.albedoColor = Color3.Black()
  queuedCore.emissiveColor = Color3.FromHexString('#ff9c3a')
  queuedCore.emissiveIntensity = 1.4
  queuedCore.metallic = 0
  queuedCore.roughness = 1

  const runShell = new PBRMaterial('run-shell', scene)
  runShell.albedoColor = Color3.FromHexString('#a8c4ff')
  runShell.metallic = 0
  runShell.roughness = 0.08
  runShell.alpha = 0.45
  runShell.transparencyMode = Material.MATERIAL_ALPHABLEND
  runShell.indexOfRefraction = 1.5

  const runCore = new PBRMaterial('run-core', scene)
  runCore.albedoColor = Color3.Black()
  runCore.emissiveColor = Color3.FromHexString('#7aa3ff')
  runCore.emissiveIntensity = 2.0
  runCore.metallic = 0
  runCore.roughness = 1

  // Scanner beam — pure emissive, slightly hotter than the LED
  // perimeter so it reads as a moving light source rather than
  // ambient trim. Cyan keeps it in family with the LED frames.
  const scannerBeam = new PBRMaterial('scanner-beam', scene)
  scannerBeam.albedoColor = Color3.Black()
  scannerBeam.emissiveColor = Color3.FromHexString('#a4f8ff')
  scannerBeam.emissiveIntensity = 2.4
  scannerBeam.metallic = 0
  scannerBeam.roughness = 1
  scannerBeam.alpha = 0.85
  scannerBeam.transparencyMode = Material.MATERIAL_ALPHABLEND

  return {
    body,
    trayFloor,
    ledTrim,
    ventGlow,
    heatsinkFin,
    screen,
    beltSurface,
    recessInterior,
    queuedShell,
    queuedCore,
    runShell,
    runCore,
    scannerBeam,
  }
}

// ─── Port mesh builder ────────────────────────────────────────────────────

interface PortBuild {
  cutout: Mesh
  frameMeshes: Mesh[]
  recessWalls: Mesh[]
  belt: BeltBuild
  handle: PortHandle
}

const FRAME_THICKNESS = 2
const FRAME_PROTRUSION = 0.6
const STUB_BACK_GAP = 2

const RECESS_WALL_THICKNESS = 0.2
const RECESS_WALL_INSET = 0.1

function buildPortMeshes(
  scene: Scene,
  station: Station,
  port: Port,
  materials: StationMaterials,
  index: number,
): PortBuild {
  const isXAxis = port.direction === 'east' || port.direction === 'west'
  const outwardSign = port.direction === 'east' || port.direction === 'north' ? 1 : -1
  const outwardX = isXAxis ? outwardSign : 0
  const outwardY = isXAxis ? 0 : outwardSign
  const acrossX = isXAxis ? 0 : 1
  const acrossY = isXAxis ? 1 : 0
  const acrossLen = isXAxis ? station.d : station.w

  let faceX: number
  let faceY: number
  switch (port.direction) {
    case 'east':
      faceX = station.x + station.w
      faceY = station.y + station.d / 2
      break
    case 'west':
      faceX = station.x
      faceY = station.y + station.d / 2
      break
    case 'north':
      faceX = station.x + station.w / 2
      faceY = station.y + station.d
      break
    case 'south':
      faceX = station.x + station.w / 2
      faceY = station.y
      break
  }

  const acrossDelta = (port.offset - 0.5) * acrossLen
  const snapX = faceX + acrossX * acrossDelta
  const snapY = faceY + acrossY * acrossDelta
  const snapZ = station.z + CONVEYOR_HEIGHT / 2

  const recessOpenW = CONVEYOR_WIDTH + 2 * PORT_RECESS_MARGIN_ACROSS
  const recessOpenH = CONVEYOR_HEIGHT + PORT_RECESS_MARGIN_ABOVE

  const cutoutOvershoot = 1
  const cutoutAlong = port.recessDepth + cutoutOvershoot
  const cutoutAlongOffset = (cutoutOvershoot - port.recessDepth) / 2
  const cutoutVertical = recessOpenH + 1
  const cutoutVerticalCenter = station.z + (recessOpenH - 1) / 2
  const cutoutSize = isXAxis
    ? { width: cutoutAlong, height: recessOpenW, depth: cutoutVertical }
    : { width: recessOpenW, height: cutoutAlong, depth: cutoutVertical }

  const cutout = MeshBuilder.CreateBox(`port-cut-${index}`, cutoutSize, scene)
  cutout.position.set(
    faceX + outwardX * cutoutAlongOffset + acrossX * acrossDelta,
    faceY + outwardY * cutoutAlongOffset + acrossY * acrossDelta,
    cutoutVerticalCenter,
  )

  const recessTopZ = station.z + recessOpenH
  const frameOutwardCenter = FRAME_PROTRUSION / 2
  const frameMeshes: Mesh[] = []

  const topAcrossLen = recessOpenW + 2 * FRAME_THICKNESS
  const topZ = recessTopZ + FRAME_THICKNESS / 2
  const topSize = isXAxis
    ? { width: FRAME_PROTRUSION, height: topAcrossLen, depth: FRAME_THICKNESS }
    : { width: topAcrossLen, height: FRAME_PROTRUSION, depth: FRAME_THICKNESS }
  const topBar = MeshBuilder.CreateBox(`port-frame-${index}-top`, topSize, scene)
  topBar.position.set(
    faceX + outwardX * frameOutwardCenter + acrossX * acrossDelta,
    faceY + outwardY * frameOutwardCenter + acrossY * acrossDelta,
    topZ,
  )
  topBar.material = materials.ledTrim
  frameMeshes.push(topBar)

  const sideZ = station.z + recessOpenH / 2
  const sideAcrossOffset = recessOpenW / 2 + FRAME_THICKNESS / 2
  const sideSize = isXAxis
    ? { width: FRAME_PROTRUSION, height: FRAME_THICKNESS, depth: recessOpenH }
    : { width: FRAME_THICKNESS, height: FRAME_PROTRUSION, depth: recessOpenH }
  for (const sign of [-1, 1] as const) {
    const bar = MeshBuilder.CreateBox(
      `port-frame-${index}-${sign < 0 ? 'left' : 'right'}`,
      sideSize,
      scene,
    )
    bar.position.set(
      faceX + outwardX * frameOutwardCenter + acrossX * (acrossDelta + sign * sideAcrossOffset),
      faceY + outwardY * frameOutwardCenter + acrossY * (acrossDelta + sign * sideAcrossOffset),
      sideZ,
    )
    bar.material = materials.ledTrim
    frameMeshes.push(bar)
  }

  const inX = -outwardX
  const inY = -outwardY
  const wallT = RECESS_WALL_THICKNESS
  const wallInset = RECESS_WALL_INSET
  const recessWalls: Mesh[] = []

  const placeWall = (
    name: string,
    alongCenter: number,
    acrossOffset: number,
    zCenter: number,
    alongExtent: number,
    acrossExtent: number,
    zExtent: number,
  ): Mesh => {
    const w = isXAxis ? alongExtent : acrossExtent
    const h = isXAxis ? acrossExtent : alongExtent
    const slab = MeshBuilder.CreateBox(name, { width: w, height: h, depth: zExtent }, scene)
    slab.position.set(
      faceX + inX * alongCenter + acrossX * (acrossDelta + acrossOffset),
      faceY + inY * alongCenter + acrossY * (acrossDelta + acrossOffset),
      zCenter,
    )
    slab.material = materials.recessInterior
    return slab
  }

  recessWalls.push(
    placeWall(
      `port-recess-back-${index}`,
      port.recessDepth - wallInset - wallT / 2,
      0,
      station.z + recessOpenH / 2,
      wallT,
      recessOpenW,
      recessOpenH,
    ),
  )
  recessWalls.push(
    placeWall(
      `port-recess-top-${index}`,
      port.recessDepth / 2,
      0,
      station.z + recessOpenH - wallInset - wallT / 2,
      port.recessDepth - 2 * wallInset,
      recessOpenW,
      wallT,
    ),
  )
  for (const sign of [-1, 1] as const) {
    recessWalls.push(
      placeWall(
        `port-recess-side-${index}-${sign < 0 ? 'l' : 'r'}`,
        port.recessDepth / 2,
        sign * (recessOpenW / 2 - wallInset - wallT / 2),
        station.z + recessOpenH / 2,
        port.recessDepth - 2 * wallInset,
        wallT,
        recessOpenH,
      ),
    )
  }

  const stubLength = port.recessDepth - STUB_BACK_GAP
  const wallEnd = new Vector3(
    faceX + acrossX * acrossDelta,
    faceY + acrossY * acrossDelta,
    station.z,
  )
  const recessEnd = new Vector3(
    faceX + inX * stubLength + acrossX * acrossDelta,
    faceY + inY * stubLength + acrossY * acrossDelta,
    station.z,
  )
  const isOutput = port.kind === 'output'
  const belt = buildBelt(
    scene,
    {
      start: isOutput ? recessEnd : wallEnd,
      end: isOutput ? wallEnd : recessEnd,
      pathOffset: 0,
      capStart: false,
      capEnd: false,
    },
    materials.beltSurface,
  )

  const handle: PortHandle = {
    port,
    worldPos: new Vector3(snapX, snapY, snapZ),
    outward: new Vector3(outwardX, outwardY, 0),
    segment: belt.segment,
  }

  return { cutout, frameMeshes, recessWalls, belt, handle }
}

// ─── Tray helpers ─────────────────────────────────────────────────────────

interface TrayBounds {
  /** Tray opening corners on the body's top surface. */
  x0: number
  x1: number
  y0: number
  y1: number
  /** Top of the tray-floor inset slab — chips sit on this z. */
  floorZ: number
}

/** Build the LED perimeter (4 strips) tracing the inside of a tray
 *  opening. Strips sit on the body top, just outside the lip. */
function buildLedPerimeter(
  scene: Scene,
  bounds: TrayBounds,
  bodyTopZ: number,
  ledTrim: PBRMaterial,
  namePrefix: string,
  parent: TransformNode,
): void {
  const perimZ = bodyTopZ + LED_PERIM_HEIGHT / 2
  const lenX = bounds.x1 - bounds.x0 - 2 * LED_PERIM_END_INSET
  const lenY = bounds.y1 - bounds.y0 - 2 * LED_PERIM_END_INSET
  const cfgs: Array<{ suffix: string; cx: number; cy: number; w: number; d: number }> = [
    {
      suffix: 's',
      cx: (bounds.x0 + bounds.x1) / 2,
      cy: bounds.y0 - LED_PERIM_INSET - LED_PERIM_WIDTH / 2,
      w: lenX,
      d: LED_PERIM_WIDTH,
    },
    {
      suffix: 'n',
      cx: (bounds.x0 + bounds.x1) / 2,
      cy: bounds.y1 + LED_PERIM_INSET + LED_PERIM_WIDTH / 2,
      w: lenX,
      d: LED_PERIM_WIDTH,
    },
    {
      suffix: 'w',
      cx: bounds.x0 - LED_PERIM_INSET - LED_PERIM_WIDTH / 2,
      cy: (bounds.y0 + bounds.y1) / 2,
      w: LED_PERIM_WIDTH,
      d: lenY,
    },
    {
      suffix: 'e',
      cx: bounds.x1 + LED_PERIM_INSET + LED_PERIM_WIDTH / 2,
      cy: (bounds.y0 + bounds.y1) / 2,
      w: LED_PERIM_WIDTH,
      d: lenY,
    },
  ]
  for (const c of cfgs) {
    const strip = MeshBuilder.CreateBox(
      `${namePrefix}-led-${c.suffix}`,
      { width: c.w, height: c.d, depth: LED_PERIM_HEIGHT },
      scene,
    )
    strip.position.set(c.cx, c.cy, perimZ)
    strip.material = ledTrim
    strip.parent = parent
  }
}

// ─── Builder ───────────────────────────────────────────────────────────────

/** Build one station as a hierarchy of Babylon meshes. Returns a
 *  StationHandle with a TransformNode parent (move/animate/dispose
 *  the whole station at once), port handles for connecting
 *  conveyors, and setters for the dynamic chip counts. */
export function buildStationMesh(
  scene: Scene,
  station: Station,
  materials: StationMaterials,
): StationHandle {
  const root = new TransformNode(`station_${station.x}_${station.y}`, scene)
  if (station.id) {
    root.metadata = { stationId: station.id }
  }

  const cx = station.x + station.w / 2
  const cy = station.y + station.d / 2
  const bodyTopZ = station.z + station.h
  const trayCutOvershoot = 2
  const trayCutH = TRAY_DEPTH + trayCutOvershoot
  const floorThickness = 4
  const trayFloorInset = 0.6

  // ─── Tray bounds ─────────────────────────────────────────────────────
  // Compute world-space extents for both trays up front. Used for
  // CSG cuts, floor insets, LED perimeters, and chip layout.

  const mainTray: TrayBounds = {
    x0: station.x + station.w * MAIN_TRAY_X_FRAC[0],
    x1: station.x + station.w * MAIN_TRAY_X_FRAC[1],
    y0: station.y + station.d * MAIN_TRAY_Y_FRAC[0],
    y1: station.y + station.d * MAIN_TRAY_Y_FRAC[1],
    floorZ: bodyTopZ - TRAY_DEPTH + TRAY_FLOOR_LIFT,
  }
  const intakeTray: TrayBounds = {
    x0: station.x + station.w * INTAKE_TRAY_X_FRAC[0],
    x1: station.x + station.w * INTAKE_TRAY_X_FRAC[1],
    y0: station.y + station.d * INTAKE_TRAY_Y_FRAC[0],
    y1: station.y + station.d * INTAKE_TRAY_Y_FRAC[1],
    floorZ: bodyTopZ - TRAY_DEPTH + TRAY_FLOOR_LIFT,
  }

  // ─── Body with carved trays ──────────────────────────────────────────

  const bodyTmp = MeshBuilder.CreateBox(
    'body-tmp',
    { width: station.w, height: station.d, depth: station.h },
    scene,
  )
  bodyTmp.position.set(cx, cy, station.z + station.h / 2)

  const buildTrayCutMesh = (name: string, t: TrayBounds): Mesh => {
    const m = MeshBuilder.CreateBox(
      name,
      { width: t.x1 - t.x0, height: t.y1 - t.y0, depth: trayCutH },
      scene,
    )
    m.position.set(
      (t.x0 + t.x1) / 2,
      (t.y0 + t.y1) / 2,
      bodyTopZ - TRAY_DEPTH / 2 + trayCutOvershoot / 2,
    )
    return m
  }
  const mainCutTmp = buildTrayCutMesh('main-tray-cut', mainTray)
  const intakeCutTmp = buildTrayCutMesh('intake-tray-cut', intakeTray)

  const ventCutouts: Mesh[] = []
  const ventZs: number[] = []
  const ventCutoutDepth = VENT_DEPTH + 0.5
  const ventW = station.w * VENT_WIDTH_FRAC
  const ventCy = station.y + station.d - ventCutoutDepth / 2 + 0.25
  for (let i = 0; i < VENT_COUNT; i++) {
    const t = (i + 0.5) / VENT_COUNT
    const zFrac = VENT_Z_FRAC_RANGE[0] + t * (VENT_Z_FRAC_RANGE[1] - VENT_Z_FRAC_RANGE[0])
    const ventZ = station.z + station.h * zFrac
    const v = MeshBuilder.CreateBox(
      `vent-cut-${i}`,
      { width: ventW, height: ventCutoutDepth, depth: VENT_HEIGHT },
      scene,
    )
    v.position.set(cx, ventCy, ventZ)
    ventCutouts.push(v)
    ventZs.push(ventZ)
  }

  const panelCx = station.x + station.w * STATUS_PANEL_X_FRAC
  const panelCutDepth = STATUS_PANEL_DEPTH + 0.5
  const panelCz = station.z + station.h * STATUS_PANEL_Z_FRAC
  const panelTmp = MeshBuilder.CreateBox(
    'panel-cut',
    { width: STATUS_PANEL_W, height: panelCutDepth, depth: STATUS_PANEL_H },
    scene,
  )
  panelTmp.position.set(panelCx, station.y + panelCutDepth / 2 - 0.25, panelCz)

  const finsCenterY = station.y + station.d * HEATSINK_FIN_Y_FRAC
  const finsCenterZ = station.z + station.h * HEATSINK_FIN_Z_FRAC
  const finHeight = station.h * HEATSINK_FIN_HEIGHT_FRAC
  const finsTotalY = (HEATSINK_FIN_COUNT - 1) * HEATSINK_FIN_SPACING + HEATSINK_FIN_THICKNESS
  const housingCutDepth = HEATSINK_HOUSING_DEPTH + 0.5
  const housingD = finsTotalY + HEATSINK_HOUSING_PADDING_Y * 2
  const housingH = finHeight + HEATSINK_HOUSING_PADDING_Z * 2
  const housingTmp = MeshBuilder.CreateBox(
    'housing-cut',
    { width: housingCutDepth, height: housingD, depth: housingH },
    scene,
  )
  housingTmp.position.set(
    station.x + station.w - housingCutDepth / 2 + 0.25,
    finsCenterY,
    finsCenterZ,
  )

  const portBuilds: PortBuild[] = (station.ports ?? []).map((p, i) =>
    buildPortMeshes(scene, station, p, materials, i),
  )

  let bodyCSG = CSG.FromMesh(bodyTmp)
    .subtract(CSG.FromMesh(mainCutTmp))
    .subtract(CSG.FromMesh(intakeCutTmp))
    .subtract(CSG.FromMesh(panelTmp))
    .subtract(CSG.FromMesh(housingTmp))
  for (const v of ventCutouts) {
    bodyCSG = bodyCSG.subtract(CSG.FromMesh(v))
  }
  for (const pb of portBuilds) {
    bodyCSG = bodyCSG.subtract(CSG.FromMesh(pb.cutout))
  }
  const body = bodyCSG.toMesh('station-body', materials.body, scene, true)
  body.parent = root
  if (station.id) {
    body.metadata = { stationId: station.id }
  }

  bodyTmp.dispose()
  mainCutTmp.dispose()
  intakeCutTmp.dispose()
  panelTmp.dispose()
  housingTmp.dispose()
  for (const v of ventCutouts) v.dispose()
  for (const pb of portBuilds) {
    pb.cutout.dispose()
    for (const f of pb.frameMeshes) f.parent = root
    for (const w of pb.recessWalls) w.parent = root
    pb.belt.root.parent = root
  }

  // ─── Status screen ───────────────────────────────────────────────────
  const statusScreen = MeshBuilder.CreateBox(
    'status-screen',
    {
      width: STATUS_PANEL_W - STATUS_SCREEN_INSET * 2,
      height: STATUS_SCREEN_THICKNESS,
      depth: STATUS_PANEL_H - STATUS_SCREEN_INSET * 2,
    },
    scene,
  )
  statusScreen.position.set(
    panelCx,
    station.y + STATUS_PANEL_DEPTH - STATUS_SCREEN_THICKNESS / 2 - 0.05,
    panelCz,
  )
  // Per-station screen material. Carries a DynamicTexture rendered
  // with the lifetime counter on its albedo + emissive channels.
  // Babylon's default Box UV mapping gives each face the full 0–1 UV
  // range, so whichever face of the thin screen plate is camera-facing
  // will display the text — no per-face UV gymnastics required.
  //
  // 4× resolution vs the screen face's effective pixel budget keeps
  // the numerals crisp under the camera's iso zoom. Mipmaps are off
  // (LINEAR_LINEAR sampling); anisotropic filtering at level 16
  // handles the oblique viewing angle without adding blur.
  const screenTexW = 1024
  const screenTexH = 512
  const screenTex = new DynamicTexture(
    `status-screen-tex-${station.x}_${station.y}`,
    { width: screenTexW, height: screenTexH },
    scene,
    false,
    Texture.BILINEAR_SAMPLINGMODE,
  )
  screenTex.anisotropicFilteringLevel = 16
  const screenCtx = screenTex.getContext() as CanvasRenderingContext2D
  screenCtx.imageSmoothingEnabled = true
  screenCtx.imageSmoothingQuality = 'high'

  // Approximate the shared `materials.screen` look (dark blue glass
  // with low emissive lift) so the text sits on the same backdrop the
  // station already shows. Filled into the canvas itself rather than
  // layered as a second material — keeps a single PBRMaterial per
  // station and avoids a transparent plane that drops out at certain
  // angles.
  const screenBgFill = '#15171c'
  const screenTextFill = '#a4f8ff'
  // Monospaced stack to give the lifetime counter an LCD/scoreboard
  // feel matching the rest of the factory's emissive readouts. SF Mono
  // ships on macOS, Cascadia/Consolas on Windows, plus JetBrains Mono
  // and Fira Code as common dev installs. 500 weight reads as a thin
  // refined digital display rather than a heavy print numeral.
  const screenFontFamily =
    "'SF Mono', 'JetBrains Mono', 'Fira Code', 'Cascadia Mono', Menlo, Consolas, monospace"

  const renderScreenText = (n: number) => {
    screenCtx.fillStyle = screenBgFill
    screenCtx.fillRect(0, 0, screenTexW, screenTexH)
    const text = formatLifetimeCount(n)
    // Babylon's default Box UV mapping puts the canvas U axis along
    // the visible face's *vertical* edge, not its horizontal one — so
    // text drawn upright on the canvas ends up rotated 90° CW on the
    // screen. We pre-rotate the drawing 90° CCW to compensate. After
    // rotation the text's natural width is laid out along canvas Y,
    // so the shrink-to-fit budget compares against canvas height.
    //
    // Sizing: a 4-char monospace string ("999k", "1.2M") at ~0.6 char
    // aspect needs roughly 2.4 × fontPx of width; with a 412 px budget
    // that caps fontPx at ~170. Starting at 320 leaves room for the
    // common shorter strings ("47", "1.2k") to render larger.
    const widthBudget = screenTexH - 100
    let fontPx = 320
    screenCtx.font = `500 ${fontPx}px ${screenFontFamily}`
    while (screenCtx.measureText(text).width > widthBudget && fontPx > 140) {
      fontPx -= 10
      screenCtx.font = `500 ${fontPx}px ${screenFontFamily}`
    }
    screenCtx.fillStyle = screenTextFill
    screenCtx.textAlign = 'center'
    screenCtx.textBaseline = 'middle'
    screenCtx.save()
    screenCtx.translate(screenTexW / 2, screenTexH / 2)
    screenCtx.rotate(Math.PI / 2)
    // The visible face of the screen Box is back-faced relative to
    // Babylon's default UV winding, so the rotation alone leaves
    // glyphs mirrored along the screen's horizontal axis. scale(-1, 1)
    // in the rotated frame flips them back.
    screenCtx.scale(-1, 1)
    screenCtx.fillText(text, 0, 0)
    screenCtx.restore()
    screenTex.update(true)
  }
  let lifetimeValue = 0
  renderScreenText(0)

  const screenMat = new PBRMaterial(`status-screen-mat-${station.x}_${station.y}`, scene)
  screenMat.albedoColor = Color3.White()
  screenMat.albedoTexture = screenTex
  screenMat.emissiveTexture = screenTex
  screenMat.emissiveColor = Color3.White()
  screenMat.emissiveIntensity = 1.1
  screenMat.metallic = 0.1
  screenMat.roughness = 0.35
  screenMat.clearCoat.isEnabled = true
  screenMat.clearCoat.intensity = 0.7
  screenMat.clearCoat.roughness = 0.05
  ;(screenMat.albedoTexture as Texture).wrapU = Texture.CLAMP_ADDRESSMODE
  ;(screenMat.albedoTexture as Texture).wrapV = Texture.CLAMP_ADDRESSMODE
  statusScreen.material = screenMat
  statusScreen.parent = root

  // ─── Vent glow slabs ─────────────────────────────────────────────────
  const ventGlowBackY = station.y + station.d - VENT_DEPTH + VENT_GLOW_THICKNESS / 2 + 0.05
  const ventGlowW = ventW - VENT_GLOW_INSET * 2
  const ventGlowH = VENT_HEIGHT - VENT_GLOW_INSET * 2
  for (let i = 0; i < ventZs.length; i++) {
    const glow = MeshBuilder.CreateBox(
      `vent-glow-${i}`,
      { width: ventGlowW, height: VENT_GLOW_THICKNESS, depth: ventGlowH },
      scene,
    )
    glow.position.set(cx, ventGlowBackY, ventZs[i])
    glow.material = materials.ventGlow
    glow.parent = root
  }

  // ─── Tray floor insets ───────────────────────────────────────────────

  const buildTrayFloor = (name: string, t: TrayBounds): Mesh => {
    const w = t.x1 - t.x0 - trayFloorInset * 2
    const h = t.y1 - t.y0 - trayFloorInset * 2
    const m = MeshBuilder.CreateBox(name, { width: w, height: h, depth: floorThickness }, scene)
    m.position.set((t.x0 + t.x1) / 2, (t.y0 + t.y1) / 2, t.floorZ - floorThickness / 2)
    m.material = materials.trayFloor
    m.parent = root
    if (station.id) {
      m.metadata = { stationId: station.id }
    }
    return m
  }
  buildTrayFloor('main-tray-floor', mainTray)
  buildTrayFloor('intake-tray-floor', intakeTray)

  // ─── LED perimeters ──────────────────────────────────────────────────
  buildLedPerimeter(scene, mainTray, bodyTopZ, materials.ledTrim, 'main', root)
  buildLedPerimeter(scene, intakeTray, bodyTopZ, materials.ledTrim, 'intake', root)

  // ─── Etched label on main tray ───────────────────────────────────────

  const trayD = mainTray.y1 - mainTray.y0
  const trayW = mainTray.x1 - mainTray.x0
  const labelDepth = trayD * LABEL_PLATE_DEPTH_FRAC
  const labelBackEdge = mainTray.y1 - LABEL_PLATE_BACK_INSET
  const labelFrontEdge = labelBackEdge - labelDepth
  const trayCx = (mainTray.x0 + mainTray.x1) / 2
  if (station.label && station.label.length > 0) {
    const plateW = trayW - 2 * (trayFloorInset + LABEL_PLATE_SIDE_MARGIN)
    const plateThickness = 0.6
    const plateCy = labelBackEdge - labelDepth / 2
    const plateZ = mainTray.floorZ + LABEL_PLATE_LIFT + plateThickness / 2
    const plate = MeshBuilder.CreateBox(
      'tray-label',
      { width: plateW, height: labelDepth, depth: plateThickness },
      scene,
    )
    plate.position.set(trayCx, plateCy, plateZ)

    const tex = new DynamicTexture(
      `tray-label-tex-${station.x}_${station.y}`,
      { width: LABEL_TEX_W, height: LABEL_TEX_H },
      scene,
      false,
    )
    const ctx = tex.getContext() as CanvasRenderingContext2D
    ctx.clearRect(0, 0, LABEL_TEX_W, LABEL_TEX_H)
    let fontPx = 56
    ctx.font = `600 ${fontPx}px Inter, system-ui, sans-serif`
    while (ctx.measureText(station.label).width > LABEL_TEX_W - 60 && fontPx > 24) {
      fontPx -= 2
      ctx.font = `600 ${fontPx}px Inter, system-ui, sans-serif`
    }
    ctx.fillStyle = '#ffffff'
    ctx.textAlign = 'center'
    ctx.textBaseline = 'middle'
    // Babylon's right-handed CreateBox flips V on the +z face's UVs,
    // so we pre-flip the canvas's Y axis to compensate.
    ctx.save()
    ctx.translate(0, LABEL_TEX_H)
    ctx.scale(1, -1)
    ctx.fillText(station.label, LABEL_TEX_W / 2, LABEL_TEX_H / 2 + 2)
    ctx.restore()
    tex.update(true)
    tex.hasAlpha = true
    tex.getAlphaFromRGB = false

    const labelMat = new PBRMaterial(`tray-label-mat-${station.x}_${station.y}`, scene)
    labelMat.albedoColor = Color3.Black()
    labelMat.albedoTexture = tex
    labelMat.useAlphaFromAlbedoTexture = true
    labelMat.transparencyMode = Material.MATERIAL_ALPHABLEND
    labelMat.emissiveTexture = tex
    labelMat.emissiveColor = Color3.White()
    labelMat.emissiveIntensity = 1.4
    labelMat.metallic = 0
    labelMat.roughness = 1
    plate.material = labelMat
    plate.parent = root
  }

  // ─── Heat sink module ────────────────────────────────────────────────

  const backplateOuterX =
    station.x + station.w - HEATSINK_HOUSING_DEPTH + HEATSINK_BACKPLATE_THICKNESS
  const backplate = MeshBuilder.CreateBox(
    'heatsink-backplate',
    {
      width: HEATSINK_BACKPLATE_THICKNESS,
      height: housingD - 1.5,
      depth: housingH - 1.5,
    },
    scene,
  )
  backplate.position.set(
    backplateOuterX - HEATSINK_BACKPLATE_THICKNESS / 2,
    finsCenterY,
    finsCenterZ,
  )
  backplate.material = materials.trayFloor
  backplate.parent = root

  const finStartY = finsCenterY - ((HEATSINK_FIN_COUNT - 1) * HEATSINK_FIN_SPACING) / 2
  const finInnerX = backplateOuterX + 0.05
  for (let i = 0; i < HEATSINK_FIN_COUNT; i++) {
    const fin = MeshBuilder.CreateBox(
      `heatsink-${i}`,
      { width: HEATSINK_FIN_EXTENT, height: HEATSINK_FIN_THICKNESS, depth: finHeight },
      scene,
    )
    fin.position.set(
      finInnerX + HEATSINK_FIN_EXTENT / 2,
      finStartY + i * HEATSINK_FIN_SPACING,
      finsCenterZ,
    )
    fin.material = materials.heatsinkFin
    fin.parent = root
  }

  // ─── Anchor discs ────────────────────────────────────────────────────

  const anchorZ = station.z + station.h * ANCHOR_Z_FRAC
  const anchorPositions: Array<{ x: number; y: number }> = [
    { x: station.x + ANCHOR_X_INSET, y: station.y - ANCHOR_THICKNESS / 2 },
    { x: station.x + station.w - ANCHOR_X_INSET, y: station.y - ANCHOR_THICKNESS / 2 },
    { x: station.x + ANCHOR_X_INSET, y: station.y + station.d + ANCHOR_THICKNESS / 2 },
    {
      x: station.x + station.w - ANCHOR_X_INSET,
      y: station.y + station.d + ANCHOR_THICKNESS / 2,
    },
  ]
  for (let i = 0; i < anchorPositions.length; i++) {
    const p = anchorPositions[i]
    const anchor = MeshBuilder.CreateCylinder(
      `anchor-${i}`,
      { height: ANCHOR_THICKNESS, diameter: ANCHOR_DIAM, tessellation: 24 },
      scene,
    )
    anchor.position.set(p.x, p.y, anchorZ)
    anchor.material = materials.trayFloor
    anchor.parent = root
  }

  // ─── Queued chip pool (intake tray) ──────────────────────────────────
  // Pre-build MAX_QUEUED chips arrayed in a 2-col grid on the
  // intake tray floor, all initially hidden. setQueuedCount toggles
  // visibility on the first n.

  const queuedLayout = layoutChipGrid({
    bounds: intakeTray,
    cols: 2,
    chipDiam: QUEUED_CHIP_DIAM,
    chipGap: QUEUED_CHIP_GAP,
    floorInset: trayFloorInset,
    rowMargin: 4,
    front: intakeTray.y0,
    back: intakeTray.y1,
  })
  // Both shell and core are parented to root (not shell→core) so
  // each gets the world-coord position we set; toggling enabled
  // state walks both arrays in lockstep.
  const queuedShells: Mesh[] = []
  const queuedCores: Mesh[] = []
  for (let i = 0; i < MAX_QUEUED; i++) {
    const pos = queuedLayout[i]
    if (!pos) break
    const chipZCenter = intakeTray.floorZ + QUEUED_CHIP_H / 2
    const shell = MeshBuilder.CreateCylinder(
      `queued-shell-${i}`,
      { height: QUEUED_CHIP_H, diameter: QUEUED_CHIP_DIAM, tessellation: 28 },
      scene,
    )
    shell.rotation.x = Math.PI / 2
    shell.position.set(pos.x, pos.y, chipZCenter)
    shell.material = materials.queuedShell
    shell.parent = root
    shell.setEnabled(false)
    queuedShells.push(shell)

    const core = MeshBuilder.CreateCylinder(
      `queued-core-${i}`,
      { height: QUEUED_CORE_H, diameter: QUEUED_CORE_DIAM, tessellation: 20 },
      scene,
    )
    core.rotation.x = Math.PI / 2
    core.position.set(pos.x, pos.y, chipZCenter)
    core.material = materials.queuedCore
    core.parent = root
    core.setEnabled(false)
    queuedCores.push(core)
  }

  // ─── Run chip pool (main tray) ───────────────────────────────────────
  // Pre-build MAX_RUNS chips arrayed in a grid in the front portion
  // of the main tray (label takes the back). setConversationCount toggles
  // visibility on the first n.

  const runLayout = layoutChipGrid({
    bounds: mainTray,
    cols: 9,
    chipDiam: RUN_CHIP_DIAM,
    chipGap: RUN_CHIP_GAP,
    floorInset: trayFloorInset,
    rowMargin: 4,
    front: mainTray.y0,
    back: labelFrontEdge - 4,
  })
  const runShells: Mesh[] = []
  const runCores: Mesh[] = []
  for (let i = 0; i < MAX_RUNS; i++) {
    const pos = runLayout[i]
    if (!pos) break
    const chipZCenter = mainTray.floorZ + RUN_CHIP_H / 2
    const shell = MeshBuilder.CreateCylinder(
      `run-shell-${i}`,
      { height: RUN_CHIP_H, diameter: RUN_CHIP_DIAM, tessellation: 28 },
      scene,
    )
    shell.rotation.x = Math.PI / 2
    shell.position.set(pos.x, pos.y, chipZCenter)
    shell.material = materials.runShell
    shell.parent = root
    shell.setEnabled(false)
    // Bake a phase offset onto the mesh so the per-chip pulse looks
    // like independent rhythms rather than a synchronized wave.
    ;(shell.metadata ??= {}).pulsePhase = (i * 0.37) % (2 * Math.PI)
    runShells.push(shell)

    const core = MeshBuilder.CreateCylinder(
      `run-core-${i}`,
      { height: RUN_CORE_H, diameter: RUN_CORE_DIAM, tessellation: 20 },
      scene,
    )
    core.rotation.x = Math.PI / 2
    core.position.set(pos.x, pos.y, chipZCenter)
    core.material = materials.runCore
    core.parent = root
    core.setEnabled(false)
    runCores.push(core)
  }

  // ─── Scanner laser ───────────────────────────────────────────────────
  // Spans the main tray width along x; animates y-position front to
  // back, snaps back, repeats. Hidden when no runs are active so
  // empty stations look idle.

  const scannerW = mainTray.x1 - mainTray.x0 - 2 * (trayFloorInset + 4)
  const scannerZ = mainTray.floorZ + RUN_CHIP_H + SCANNER_LIFT_ABOVE_CHIP
  const scannerYStart = mainTray.y0 + 6
  const scannerYEnd = labelFrontEdge - 4
  const scanner = MeshBuilder.CreateBox(
    'scanner-beam',
    { width: scannerW, height: SCANNER_THICKNESS_Y, depth: SCANNER_THICKNESS_Z },
    scene,
  )
  scanner.position.set(trayCx, scannerYStart, scannerZ)
  scanner.material = materials.scannerBeam
  scanner.parent = root
  scanner.setEnabled(false)

  // ─── Live state + animation ──────────────────────────────────────────

  let queuedVisible = 0
  let runVisible = 0

  const setQueuedCount = (n: number) => {
    const target = Math.max(0, Math.min(n, queuedShells.length))
    if (target === queuedVisible) return
    for (let i = 0; i < queuedShells.length; i++) {
      const on = i < target
      queuedShells[i].setEnabled(on)
      queuedCores[i].setEnabled(on)
    }
    queuedVisible = target
  }
  const setConversationCount = (n: number) => {
    const target = Math.max(0, Math.min(n, runShells.length))
    if (target === runVisible) return
    for (let i = 0; i < runShells.length; i++) {
      const on = i < target
      runShells[i].setEnabled(on)
      runCores[i].setEnabled(on)
    }
    runVisible = target
    scanner.setEnabled(target > 0)
  }

  const setLifetimeCount = (n: number) => {
    if (lifetimeValue === n) return
    lifetimeValue = n
    renderScreenText(n)
  }

  setQueuedCount(station.queuedCount ?? 0)
  setConversationCount(station.conversationCount ?? 0)

  // Single per-station observer drives both scanner translation and
  // per-chip pulse. Cheap: only updates visible meshes.
  scene.onBeforeRenderObservable.add(() => {
    const now = performance.now() / 1000
    if (runVisible > 0) {
      // Linear loop along y.
      const frac = (now / SCANNER_PERIOD_SECONDS) % 1
      scanner.position.y = scannerYStart + frac * (scannerYEnd - scannerYStart)
      // Per-chip pulse: scale.x = scale.y = 1 + amplitude * sin(...).
      // Apply to both shell and core so they breathe together.
      const omega = (2 * Math.PI) / PULSE_PERIOD_SECONDS
      for (let i = 0; i < runVisible; i++) {
        const phase = (runShells[i].metadata?.pulsePhase as number) ?? 0
        const s = 1 + PULSE_AMPLITUDE * Math.sin(omega * now + phase)
        runShells[i].scaling.set(s, s, 1)
        runCores[i].scaling.set(s, s, 1)
      }
    }
  })

  return {
    root,
    ports: portBuilds.map((pb) => pb.handle),
    bounds: { x: station.x, y: station.y, w: station.w, d: station.d, h: station.h },
    id: station.id,
    setQueuedCount,
    setConversationCount,
    setLifetimeCount,
  }
}

// formatLifetimeCount renders integers compactly so the inline label
// stays legible past four digits. 0–999 verbatim, ≥1k formatted as
// "1.2k" / "12k", ≥1m as "1.2M". Drops trailing ".0" for round numbers.
function formatLifetimeCount(n: number): string {
  if (n < 1000) return String(n)
  if (n < 10_000) return (n / 1000).toFixed(1).replace(/\.0$/, '') + 'k'
  if (n < 1_000_000) {
    const thousands = Math.floor(n / 1000)
    return thousands + 'k'
  }
  return (n / 1_000_000).toFixed(1).replace(/\.0$/, '') + 'M'
}

// ─── Chip grid layout ─────────────────────────────────────────────────────

interface ChipGridLayoutOpts {
  bounds: TrayBounds
  cols: number
  chipDiam: number
  chipGap: number
  floorInset: number
  rowMargin: number
  /** Front edge of the chip area (low y) and back edge (high y).
   *  Chips fill front-to-back, then wrap. */
  front: number
  back: number
}

/** Compute (x, y) world positions for chips in a row-major grid
 *  inside the tray's chip area. Front-most row is closest to the
 *  camera. Returns up to `cols × rows` positions; caller stops
 *  consuming when its pool is exhausted. */
function layoutChipGrid(opts: ChipGridLayoutOpts): Array<{ x: number; y: number }> {
  const { bounds, cols, chipDiam, chipGap, floorInset, rowMargin, front, back } = opts
  const stride = chipDiam + chipGap
  const innerW = bounds.x1 - bounds.x0 - 2 * (floorInset + 2)
  const totalRowW = cols * chipDiam + (cols - 1) * chipGap
  const startX =
    totalRowW > innerW
      ? bounds.x0 + floorInset + 2 + chipDiam / 2
      : (bounds.x0 + bounds.x1) / 2 - totalRowW / 2 + chipDiam / 2
  const innerD = back - front - 2 * rowMargin
  const rows = Math.max(1, Math.floor((innerD + chipGap) / stride))
  const startY = front + rowMargin + chipDiam / 2
  const out: Array<{ x: number; y: number }> = []
  for (let r = 0; r < rows; r++) {
    for (let c = 0; c < cols; c++) {
      out.push({ x: startX + c * stride, y: startY + r * stride })
    }
  }
  return out
}
