// 3D rendering layer for the factory scene, built on Babylon.js.
//
// One Babylon `ArcRotateCamera` is the single source of truth for the
// view: alpha (yaw), beta (pitch), radius (zoom level), target (focal
// point). Babylon's default inputs handle gestures — left-mouse drag
// orbits, right-mouse (or ctrl+click) drags pan, wheel zooms.
//
// We're orthographic-only for now. `radius` doesn't affect projection
// in ortho mode, but Babylon still uses it as the wheel-input target —
// so we mirror it into `orthoLeft/Right/Top/Bottom` via a one-line
// observer on every camera-state change.
//
// What this file owns:
//   - The Babylon Engine, Scene, ArcRotateCamera, lights.
//   - The floor grid (LineSystem) and ground plane that catches
//     shadows.
//   - The ShadowGenerator on the sun directional light, and the
//     GlowLayer that blooms emissive trims/cores.
//   - The render loop.
//
// What this file does NOT own:
//   - Gestures. Babylon's default ArcRotateCamera inputs handle them.
//   - Camera-state mirroring or projection helpers. The camera *is*
//     the state. For projecting world points to screen (Stage 6 HTML
//     overlays), use Babylon's `Vector3.Project` directly with the
//     scene's view+projection matrices.

import {
  AbstractMesh,
  ArcRotateCamera,
  Camera,
  Color3,
  Color4,
  DirectionalLight,
  DynamicTexture,
  Engine,
  GlowLayer,
  HemisphericLight,
  ImageProcessingConfiguration,
  LinesMesh,
  Matrix,
  Mesh,
  MeshBuilder,
  PBRMaterial,
  PointerEventTypes,
  Scene,
  ShadowGenerator,
  StandardMaterial,
  Texture,
  Vector3,
} from '@babylonjs/core'

import { buildBelt, buildCurvedBelt, type BeltBuild } from './iso-belt'
import { ItemSimulator, type SpawnerOptions, type SpawnOptions } from './iso-items'
import { SnapshotChipController } from './snapshot-chips'
import type { PathSegment } from './iso-path'
import { buildPoleMesh, type Pole, type PoleBuild } from './iso-pole'
import { CONVEYOR_HEIGHT, CONVEYOR_WIDTH, type PortDirection, type PortHandle } from './iso-port'
import { buildRouterMesh, type Router, type RouterBuild } from './iso-router'
import {
  buildStationMesh,
  createStationMaterials,
  type Station,
  type StationHandle,
  type StationMaterials,
} from './iso-station'

const DEFAULT_FLOOR_SIZE = 4800
// Closest the camera can get before its near plane starts clipping
// objects in the scene. Expressed as a zoom multiplier over the
// floor's full-extent ortho bounds (radius_min = full_extent_radius
// / max_zoom), independent of where the camera starts. 9× lets the
// front-facing status panel and run-chip cluster on the console top
// read clearly at peak zoom.
const MAX_ZOOM = 9
// Initial zoom level relative to the floor's full-extent ortho
// bounds. >1 is zoomed in (smaller visible area).
const INITIAL_ZOOM = 1.5
// Camera target's y offset below the floor center, so the action
// fills more of the lower half of the screen by default. ~10 cells
// at the debug scene's 80-wu cell size.
const INITIAL_TARGET_Y_OFFSET = -800

/** Walk up the parent chain from a picked mesh looking for the
 *  `metadata.stationId` we tagged on the station's body and tray
 *  meshes. Picking a chip / scanner / port-stub on a station still
 *  routes to the same station id because the parent TransformNode
 *  also carries the metadata. */
function findStationId(mesh: AbstractMesh): string | undefined {
  let cur: AbstractMesh | { parent: unknown; metadata?: { stationId?: string } } | null = mesh
  while (cur) {
    const m = (cur as { metadata?: { stationId?: string } }).metadata
    if (m?.stationId) return m.stationId
    cur = (cur as { parent: AbstractMesh | null }).parent ?? null
  }
  return undefined
}

export class IsoScene {
  readonly engine: Engine
  readonly scene: Scene
  readonly camera: ArcRotateCamera
  private materials: StationMaterials | null = null
  private gridMesh: LinesMesh | null = null
  private ground: Mesh | null = null
  private groundMat: PBRMaterial | null = null
  private shadowGenerator: ShadowGenerator | null = null
  private glowLayer: GlowLayer | null = null
  private itemSim: ItemSimulator | null = null
  private snapshotChips: SnapshotChipController | null = null
  private detachShiftPan: (() => void) | null = null
  private detachWheelGuard: (() => void) | null = null

  constructor(canvas: HTMLCanvasElement) {
    this.engine = new Engine(canvas, true, {
      adaptToDeviceRatio: true,
      stencil: false,
    })

    this.scene = new Scene(this.engine)
    // Warm off-white background, tuned to match the floor's albedo
    // (#f0ece3) so the rendered ground blends seamlessly into the
    // canvas chrome. ACES tonemap (below) gives a filmic rolloff so
    // the whites stay warm rather than clipping toward pure white.
    this.scene.clearColor = new Color4(0.941, 0.925, 0.89, 1)
    // Right-handed coordinates so our world (+x right, +y forward,
    // +z up) is what Babylon expects for cross products and culling.
    this.scene.useRightHandedSystem = true

    // Image-processing pipeline. Without this, Babylon writes linear
    // light straight to sRGB and any bright surface clips toward pure
    // white — on a cream-on-cream scene that reads as blinding. ACES
    // gives a filmic highlight rolloff (the standard cinematic curve);
    // exposure < 1 stops the "camera" down so mid-tones sit lower;
    // contrast > 1 deepens the shadows that the lower exposure
    // creates so the image still has bite.
    const ip = this.scene.imageProcessingConfiguration
    ip.toneMappingEnabled = true
    ip.toneMappingType = ImageProcessingConfiguration.TONEMAPPING_ACES
    ip.exposure = 0.9
    ip.contrast = 1.1

    // ArcRotateCamera: an orbit camera. With `upVector = (0, 0, 1)`
    // it orbits around `target` on the xy plane, with `alpha` as
    // azimuth and `beta` as polar angle from +z (so beta=0 is
    // top-down looking down). `radius` would be camera distance in
    // perspective; in ortho we use it as the zoom control.
    const initialTarget = new Vector3(
      DEFAULT_FLOOR_SIZE / 2,
      DEFAULT_FLOOR_SIZE / 2 + INITIAL_TARGET_Y_OFFSET,
      0,
    )
    this.camera = new ArcRotateCamera(
      'iso-camera',
      Math.PI / 2, // alpha — initial azimuth (items flow left → right on screen)
      Math.PI / 6, // beta — 30° tilt from top-down for a hint of perspective
      DEFAULT_FLOOR_SIZE / 2 / INITIAL_ZOOM, // radius — half-height of visible ortho bounds
      initialTarget,
      this.scene,
    )
    this.camera.upVector = new Vector3(0, 0, 1)
    // Negative minZ: in ortho mode the near plane can sit *behind*
    // the camera (no perspective convergence at view-z=0). Without
    // this, at oblique angles the ground plane gets sliced where it
    // crosses behind the camera in view space, producing a visible
    // horizon seam at any tilt. Pushing the near plane back past
    // anything we'd render behind the camera eliminates the issue.
    this.camera.minZ = -DEFAULT_FLOOR_SIZE * 4
    this.camera.maxZ = 100000
    this.camera.mode = Camera.ORTHOGRAPHIC_CAMERA

    // Mirror radius → orthoBounds. Whenever the camera moves (rotate,
    // pan, wheel), this fires and keeps the ortho frustum in sync.
    const updateOrthoBounds = () => {
      const aspect = this.engine.getRenderWidth() / this.engine.getRenderHeight()
      const halfH = this.camera.radius
      this.camera.orthoTop = halfH
      this.camera.orthoBottom = -halfH
      this.camera.orthoLeft = -halfH * aspect
      this.camera.orthoRight = halfH * aspect
    }
    this.camera.onViewMatrixChangedObservable.add(updateOrthoBounds)
    this.engine.onResizeObservable.add(updateOrthoBounds)
    updateOrthoBounds()

    // Attach Babylon's default inputs. The ArcRotateCamera's pointer
    // input handles LMB-drag = orbit, RMB-drag (or ctrl+LMB) = pan,
    // wheel = zoom. attachControl(true) means "don't preventDefault
    // on canvas events" so the surrounding page still receives them
    // when appropriate (we suppress only what we explicitly need to).
    this.camera.attachControl(true)

    // Wheel is one of the things we explicitly need to suppress. Babylon
    // reads it for zoom but, told not to preventDefault, leaves the browser
    // free to hand the same event to the nearest scroller as well. Now that
    // the scene covers the window that scroller is the shell, and on macOS
    // the result is the whole frame rubber-banding under the cursor while
    // the camera zooms — one gesture, two responses.
    //
    // Suppressed here rather than by flipping attachControl to preventDefault
    // everything: that would also swallow pointerdown, and dragging an entity
    // out of the scene and onto a station is a dnd-kit sensor that needs it.
    const swallowWheel = (e: WheelEvent) => e.preventDefault()
    canvas.addEventListener('wheel', swallowWheel, { passive: false })
    this.detachWheelGuard = () => canvas.removeEventListener('wheel', swallowWheel)

    // Sensitivity tuning for our world's scale (~1200 units across):
    // smaller `*Sensibility` = more pan/rotation per pixel. Babylon's
    // defaults are tuned for tiny default scenes; ours is bigger.
    this.camera.panningSensibility = 100
    this.camera.angularSensibilityX = 1000
    this.camera.angularSensibilityY = 1000
    this.camera.wheelPrecision = 0.5

    // Constrain orbit so users can't flip below the floor or zoom
    // past usable extremes.
    this.camera.lowerBetaLimit = 0.001
    this.camera.upperBetaLimit = Math.PI / 2 - 0.01
    this.camera.lowerRadiusLimit = DEFAULT_FLOOR_SIZE / 2 / MAX_ZOOM
    this.camera.upperRadiusLimit = 5000

    // Shift+LMB pan, on top of Babylon's default RMB-only pan.
    // ArcRotateCamera ships no shift modifier hook, so we listen at
    // capture phase and feed the same `inertialPanning{X,Y}` state
    // Babylon's built-in pan input writes to — that gives us free
    // damping, sensitivity, and target-clamp behavior. When shift
    // isn't held, the listener is a no-op and Babylon's normal LMB
    // rotate handling runs unchanged.
    this.detachShiftPan = attachShiftPan(canvas, this.camera)

    // GlowLayer blooms emissive contributions (LED trim, chip cores)
    // without affecting opaque PBR responses. Created before lights
    // so the ShadowGenerator setup can rely on a fully-initialized
    // scene.
    this.glowLayer = new GlowLayer('glow', this.scene, { blurKernelSize: 32 })
    this.glowLayer.intensity = 0.55

    this.setupLighting()

    // Station-click picking. Distinguish a tap from a drag by
    // tracking pointer-down position and only firing the click when
    // the pointer hasn't moved past a small threshold by the time it
    // lifts. Without this, every camera-orbit gesture would also
    // trigger a station click on whatever mesh sat under the
    // initial press.
    let downX = 0
    let downY = 0
    let downTime = 0
    const DRAG_PX = 4
    const TAP_MAX_MS = 600
    this.scene.onPointerObservable.add((info) => {
      if (info.type === PointerEventTypes.POINTERDOWN) {
        downX = info.event.clientX
        downY = info.event.clientY
        downTime = performance.now()
      } else if (info.type === PointerEventTypes.POINTERUP) {
        const dx = info.event.clientX - downX
        const dy = info.event.clientY - downY
        const dt = performance.now() - downTime
        if (Math.hypot(dx, dy) > DRAG_PX || dt > TAP_MAX_MS) return
        const pick = this.scene.pick(this.scene.pointerX, this.scene.pointerY)
        // Both "no hit at all" and "hit something that isn't part of
        // a station" emit null so the React layer can dismiss any
        // open station drawer. The canvas only receives this event
        // when the click landed outside any HTML overlay (drawer
        // clicks don't reach Babylon at all), so this is the right
        // signal for "click off the station view to close."
        const stationId = pick?.hit && pick.pickedMesh ? findStationId(pick.pickedMesh) : undefined
        for (const cb of this.stationClickListeners) cb(stationId ?? null)
      }
    })

    this.engine.runRenderLoop(() => {
      this.scene.render()
    })
  }

  resize(): void {
    this.engine.resize()
  }

  resetView(): void {
    this.camera.alpha = Math.PI / 2
    this.camera.beta = Math.PI / 6
    this.camera.radius = DEFAULT_FLOOR_SIZE / 2 / INITIAL_ZOOM
    this.camera.target = new Vector3(
      DEFAULT_FLOOR_SIZE / 2,
      DEFAULT_FLOOR_SIZE / 2 + INITIAL_TARGET_Y_OFFSET,
      0,
    )
  }

  buildFloor(size: number, cell: number, showGrid: boolean = true): void {
    if (this.gridMesh) {
      this.gridMesh.dispose()
      this.gridMesh = null
    }
    if (this.ground) {
      this.ground.dispose()
      this.ground = null
    }

    // Grid lines, drawn just above the ground plane so they're
    // visible against it. Tinted with the project accent terracotta
    // (--color-warm #a85a3a) at very low alpha so the grid carries
    // the same warm hue as the rest of the app and station footprint
    // pads — and stays barely-there at normal zoom, only resolving
    // into faint blueprint structure when the camera leans in.
    if (showGrid) {
      const lines: Vector3[][] = []
      for (let i = 0; i <= size; i += cell) {
        lines.push([new Vector3(0, i, 0), new Vector3(size, i, 0)])
        lines.push([new Vector3(i, 0, 0), new Vector3(i, size, 0)])
      }
      const grid = MeshBuilder.CreateLineSystem('iso-grid', { lines }, this.scene)
      grid.color = Color3.FromHexString('#a85a3a')
      grid.alpha = 0.06
      grid.alwaysSelectAsActiveMesh = true
      this.gridMesh = grid
    }

    // Ground plane — receives shadows from stations and chips so the
    // scene gets soft contact-shadow grounding. Sized larger than
    // the grid so shadows don't clip at the edge of the visible
    // floor when the camera pans. CreatePlane is xy with normal +z,
    // matching our z-up world without needing rotation.
    //
    // Albedo lifted toward the app's --color-ground (#f7f5f2) for
    // cohesion with the rest of the UI. Roughness dropped to 0.72 so
    // the floor picks up a faint sheen from the warm key + cool rim
    // lights — reads as polished concrete / warm stone, not matte
    // cardboard. Stays 0% metallic so the reflection is a soft
    // diffuse bounce, not chrome.
    if (!this.groundMat) {
      this.groundMat = new PBRMaterial('ground-mat', this.scene)
      this.groundMat.albedoColor = Color3.FromHexString('#f0ece3')
      this.groundMat.metallic = 0
      this.groundMat.roughness = 0.72
    }
    const ground = MeshBuilder.CreatePlane(
      'ground',
      { width: size * 4, height: size * 4, sideOrientation: Mesh.DOUBLESIDE },
      this.scene,
    )
    ground.position = new Vector3(size / 2, size / 2, -0.5)
    ground.material = this.groundMat
    ground.receiveShadows = true
    this.ground = ground
  }

  addStation(spec: Station): StationHandle {
    const materials = this.getMaterials()
    const built = buildStationMesh(this.scene, spec, materials)

    // Register sub-meshes with the shadow generator. Opaque body
    // pieces both cast and receive; transparent shells cast only
    // (so chips drop a soft shadow onto the tray); port stubs cast
    // (dark surfaces inside the recess); emissive trims/frames are
    // skipped — their shadows would be either invisible or noisy.
    if (this.shadowGenerator) {
      for (const m of built.root.getChildMeshes()) {
        if (
          m.name === 'station-body' ||
          m.name === 'main-tray-floor' ||
          m.name === 'intake-tray-floor'
        ) {
          m.receiveShadows = true
          this.shadowGenerator.addShadowCaster(m)
        } else if (
          m.name.startsWith('queued-shell-') ||
          m.name.startsWith('run-shell-') ||
          m.name.startsWith('heatsink-')
        ) {
          this.shadowGenerator.addShadowCaster(m)
        }
      }
    }

    if (spec.id) {
      this.stationHandles.set(spec.id, built)
    }
    return built
  }

  private floorGlowMat: StandardMaterial | null = null
  /** Lazily-built shared material for floor underglow planes (used by
   *  the bridge underglow). Procedural radial-gradient texture: warm
   *  terracotta at the center, fading to fully transparent at the
   *  edge. Stretching the plane elongates the radial into an ellipse,
   *  which reads as warm spotlight on the floor under the bridge. */
  private getFloorGlowMaterial(): StandardMaterial {
    if (this.floorGlowMat) return this.floorGlowMat
    const SIZE = 256
    const tex = new DynamicTexture(
      'floor-glow-radial',
      { width: SIZE, height: SIZE },
      this.scene,
      false,
    )
    tex.hasAlpha = true
    const ctx = tex.getContext() as CanvasRenderingContext2D
    ctx.clearRect(0, 0, SIZE, SIZE)
    const cx = SIZE / 2
    const cy = SIZE / 2
    const grad = ctx.createRadialGradient(cx, cy, 0, cx, cy, SIZE / 2)
    grad.addColorStop(0, 'rgba(196, 122, 90, 0.34)')
    grad.addColorStop(0.55, 'rgba(196, 122, 90, 0.14)')
    grad.addColorStop(1, 'rgba(196, 122, 90, 0)')
    ctx.fillStyle = grad
    ctx.fillRect(0, 0, SIZE, SIZE)
    tex.update()
    tex.wrapU = Texture.CLAMP_ADDRESSMODE
    tex.wrapV = Texture.CLAMP_ADDRESSMODE

    const m = new StandardMaterial('floor-glow-mat', this.scene)
    m.diffuseTexture = tex
    m.diffuseTexture.hasAlpha = true
    m.useAlphaFromDiffuseTexture = true
    m.emissiveTexture = tex
    m.emissiveColor = new Color3(0.6, 0.6, 0.6)
    m.disableLighting = true
    m.backFaceCulling = false
    this.floorGlowMat = m
    return m
  }

  /** Subscribe to station-click events. Returns an unsubscribe
   *  function. Click hit-testing uses Babylon's pointer pick + a
   *  walk up the parent chain looking for a `metadata.stationId`. */
  /** Subscribe to station picks. Receives the picked station's id,
   *  or `null` when the click landed on empty floor / a non-station
   *  mesh — the React layer uses null as the "close the drawer"
   *  signal. Clicks that land on HTML overlays never reach Babylon,
   *  so this only fires for canvas-level interactions. */
  onStationClick(cb: (stationId: string | null) => void): () => void {
    this.stationClickListeners.add(cb)
    return () => {
      this.stationClickListeners.delete(cb)
    }
  }

  private stationHandles = new Map<string, StationHandle>()
  private stationClickListeners = new Set<(stationId: string | null) => void>()

  addPole(spec: Pole, cellSize: number, pathOffset: number = 0): PoleBuild {
    const materials = this.getMaterials()
    return buildPoleMesh(this.scene, spec, cellSize, materials.beltSurface, pathOffset)
  }

  addRouter(
    spec: Router,
    cellSize: number,
    pathOffsets: Partial<Record<PortDirection, number>> = {},
  ): RouterBuild {
    const m = this.getMaterials()
    const built = buildRouterMesh(
      this.scene,
      spec,
      cellSize,
      {
        body: m.body,
        ledTrim: m.ledTrim,
        recessInterior: m.recessInterior,
        beltSurface: m.beltSurface,
      },
      pathOffsets,
    )

    // Body and dome cast + receive shadows so the router grounds on
    // the floor like the station does. Belts, frames, and recess
    // walls don't add useful shadow contribution.
    if (this.shadowGenerator) {
      for (const mesh of built.meshes) {
        if (mesh.name === 'router-body' || mesh.name === 'router-dome') {
          mesh.receiveShadows = true
          this.shadowGenerator.addShadowCaster(mesh)
        }
      }
    }

    return built
  }

  /** Build a bridge that arches between two floor-level points. The
   *  z-profile is piecewise: 1 cell smooth ramp up (sin²(πs/2),
   *  s ∈ [0,1]) → (cellCount−2) cells flat at peak → 1 cell smooth
   *  ramp down. sin² ramps have zero slope at both endpoints, so the
   *  path tangents into the floor and into the flat top without a
   *  visible kink. Stilt cylinders sit at the ramp/peak boundaries —
   *  two per boundary (left + right of the belt edges) so the
   *  underside of the flat span stays clear of clutter and items
   *  below remain visible.
   *
   *  Direction is implicit in `start` → `end`. The straight-line
   *  distance between them should equal cellCount × cellSize. No
   *  ports — the bridge is a freestanding chain; the caller wires
   *  its returned segment manually if items should ride it.
   *
   *  cellCount must be ≥ 3: 1 ramp up + (cellCount-2) peak cells +
   *  1 ramp down. */
  addBridge(spec: {
    /** Bridge entry point at floor-z (z=0). */
    start: Vector3
    /** Bridge exit point at floor-z (z=0). */
    end: Vector3
    /** Total cells the bridge spans, ≥ 3. */
    cellCount: number
    /** Apex floor-z; the rendered top surface sits CONVEYOR_HEIGHT
     *  above this. */
    peakHeight: number
    /** Chevron continuity offset at the start end. */
    pathOffset: number
  }): BeltBuild {
    if (spec.cellCount < 3) {
      throw new Error(`addBridge: cellCount must be ≥ 3, got ${spec.cellCount}`)
    }
    const materials = this.getMaterials()

    const dx = spec.end.x - spec.start.x
    const dy = spec.end.y - spec.start.y
    const totalLen = Math.sqrt(dx * dx + dy * dy)
    // Tangent unit vector in xy, and the 90° CCW perpendicular for
    // placing stilts at the belt's two edges. (tx, ty) → (-ty, tx).
    const tx = dx / totalLen
    const ty = dy / totalLen
    const px = -ty
    const py = tx

    // Piecewise z-profile: smooth sin² ramp up over the first cell,
    // flat at peakHeight across the middle cells, smooth ramp down
    // over the last cell. Tessellation = cellCount * 8 puts ~8 points
    // per cell — generous for the chevron tile and keeps the ramps
    // visually smooth at any peakHeight.
    const tess = spec.cellCount * 8
    const rampFrac = 1 / spec.cellCount
    const points: Vector3[] = []
    for (let i = 0; i <= tess; i++) {
      const t = i / tess
      const x = spec.start.x + dx * t
      const y = spec.start.y + dy * t
      let z: number
      if (t <= rampFrac) {
        const s = t / rampFrac // [0, 1] across the up-ramp
        z = spec.peakHeight * Math.sin((Math.PI * s) / 2) ** 2
      } else if (t >= 1 - rampFrac) {
        const s = (t - (1 - rampFrac)) / rampFrac // [0, 1] across the down-ramp
        z = spec.peakHeight * Math.cos((Math.PI * s) / 2) ** 2
      } else {
        z = spec.peakHeight
      }
      points.push(new Vector3(x, y, z))
    }

    const built = buildCurvedBelt(this.scene, points, spec.pathOffset, materials.beltSurface)

    // Stilts at the boundaries between ramp and peak cells, i.e.
    // t = 1/cellCount (south end of north ramp) and t = (cellCount−1)/cellCount
    // (north end of south ramp). User feedback: only support the top
    // sides of the ramp cells so the underside of the peak stays clear
    // of clutter and items below remain visible. Two stilts per
    // boundary — one on each side of the belt — slim and matte black
    // so they read as trellis pillars rather than structural columns.
    const STILT_DIAMETER = 4
    const STILT_OFFSET = CONVEYOR_WIDTH / 2
    const stiltMat = this.getBridgeStiltMaterial()
    // At the ramp/peak boundary the path z is exactly peakHeight (the
    // sin² ramp lands at full peak with zero slope), so stilt height
    // is the same on both ends regardless of cellCount.
    const stiltHeight = spec.peakHeight + CONVEYOR_HEIGHT
    for (const t of [1 / spec.cellCount, (spec.cellCount - 1) / spec.cellCount]) {
      const x = spec.start.x + dx * t
      const y = spec.start.y + dy * t
      for (const sign of [-1, 1]) {
        const ox = sign * STILT_OFFSET * px
        const oy = sign * STILT_OFFSET * py
        const stilt = MeshBuilder.CreateCylinder(
          'bridge-stilt',
          { diameter: STILT_DIAMETER, height: stiltHeight, tessellation: 12 },
          this.scene,
        )
        // Babylon cylinders are y-axis aligned by default; stand them
        // up in our z-up world by rotating around x. Position is the
        // cylinder's center: shifted along the path-perpendicular to
        // either belt edge, z at half-height so the base sits on the
        // floor.
        stilt.rotation.x = Math.PI / 2
        stilt.position.set(x + ox, y + oy, stiltHeight / 2)
        stilt.material = stiltMat
        stilt.parent = built.root
        if (this.shadowGenerator) {
          stilt.receiveShadows = true
          this.shadowGenerator.addShadowCaster(stilt)
        }
        built.meshes.push(stilt)
      }
    }

    // Soft terracotta underglow on the floor beneath the bridge.
    // Reads as warm spotlight bouncing off the floor under the
    // elevated span — a single, distinctive moment of accent color
    // that says "this is where traffic lifts off." Uses the radial-
    // gradient material; an elongated plane stretches the radial into
    // an ellipse along the bridge's direction.
    const GLOW_HALO = 40
    const glowLen = totalLen + GLOW_HALO * 2
    const glowWidth = CONVEYOR_WIDTH + GLOW_HALO * 2
    const glow = MeshBuilder.CreatePlane(
      'bridge-floor-glow',
      { width: glowLen, height: glowWidth, sideOrientation: Mesh.DOUBLESIDE },
      this.scene,
    )
    glow.position.set((spec.start.x + spec.end.x) / 2, (spec.start.y + spec.end.y) / 2, 0.3)
    glow.rotation.z = Math.atan2(dy, dx)
    glow.material = this.getFloorGlowMaterial()
    glow.isPickable = false
    glow.parent = built.root
    built.meshes.push(glow)

    return built
  }

  private bridgeStiltMat: PBRMaterial | null = null
  private getBridgeStiltMaterial(): PBRMaterial {
    if (!this.bridgeStiltMat) {
      const m = new PBRMaterial('bridge-stilt', this.scene)
      m.albedoColor = Color3.Black()
      m.metallic = 0.2
      m.roughness = 0.4
      this.bridgeStiltMat = m
    }
    return this.bridgeStiltMat
  }

  /** Build a connecting belt between two ports. Drops the snap point's
   *  z to floor level (BeltSpec expects floor-z; the snap's z is the
   *  conveyor centerline). */
  addBelt(
    start: PortHandle,
    end: PortHandle,
    pathOffset: number,
    capStart = false,
    capEnd = false,
  ): BeltBuild {
    const materials = this.getMaterials()
    const startPos = new Vector3(start.worldPos.x, start.worldPos.y, 0)
    const endPos = new Vector3(end.worldPos.x, end.worldPos.y, 0)
    return buildBelt(
      this.scene,
      { start: startPos, end: endPos, pathOffset, capStart, capEnd },
      materials.beltSurface,
    )
  }

  /** Build a straight belt between two raw world-space points. Used
   *  when one or both endpoints aren't ports — e.g. connecting a
   *  port-based station to the freestanding bridge. Both positions
   *  must be at floor-z (z=0); the belt's top surface lifts to
   *  CONVEYOR_HEIGHT internally. */
  addBeltAt(
    start: Vector3,
    end: Vector3,
    pathOffset: number,
    capStart = false,
    capEnd = false,
  ): BeltBuild {
    const materials = this.getMaterials()
    return buildBelt(
      this.scene,
      { start, end, pathOffset, capStart, capEnd },
      materials.beltSurface,
    )
  }

  private getMaterials(): StationMaterials {
    if (!this.materials) {
      this.materials = createStationMaterials(this.scene)
    }
    return this.materials
  }

  private getItemSimulator(): ItemSimulator {
    if (!this.itemSim) {
      const m = this.getMaterials()
      // Reuse the queued-chip materials so on-belt items read as the
      // same kind of token that's stacked on the station's pad. We
      // can specialize materials later (per entity category, hotter
      // glow for in-flight, etc.) without changing the simulator.
      this.itemSim = new ItemSimulator(this.scene, m.queuedShell, m.queuedCore)
    }
    return this.itemSim
  }

  /** Register a periodic spawner on a path segment. The first item
   *  appears one interval after this call. Pass `namespaces` to
   *  round-robin through repo/project hues for the demo scene. */
  startItemSpawner(
    segment: PathSegment,
    intervalSeconds: number,
    options: SpawnerOptions = {},
  ): void {
    this.getItemSimulator().startSpawner(segment, intervalSeconds, options)
  }

  /** Spawn one chip that rides the given itinerary from start to end.
   *  Production callers (the live-data spawn pipeline) pass an
   *  itinerary precomputed by the routing table and a hue derived
   *  from the entity's repo/project. The chip disposes when it
   *  reaches the end of the itinerary; if `onArrive` is set in
   *  options it fires just before disposal. */
  spawnItem(itinerary: PathSegment[], options: SpawnOptions = {}): void {
    this.getItemSimulator().spawnItem(itinerary, options)
  }

  /** Snapshot-driven chip controller. The scene reconciler in
   *  iso-scene calls reconcile() every frame with the live transit
   *  set; this controller owns those meshes and poses them from
   *  externally-supplied progress values. Lazily constructed so the
   *  PBR materials it shares with the queued-pad don't get built
   *  until the first chip needs to render. */
  getSnapshotChipController(): SnapshotChipController {
    if (!this.snapshotChips) {
      const m = this.getMaterials()
      this.snapshotChips = new SnapshotChipController(this.scene, m.queuedShell, m.queuedCore)
    }
    return this.snapshotChips
  }

  destroy(): void {
    this.detachShiftPan?.()
    this.detachShiftPan = null
    this.detachWheelGuard?.()
    this.detachWheelGuard = null
    this.itemSim?.destroy()
    this.snapshotChips?.destroy()
    this.gridMesh?.dispose()
    this.ground?.dispose()
    this.groundMat?.dispose()
    this.floorGlowMat?.dispose()
    this.glowLayer?.dispose()
    this.shadowGenerator?.dispose()
    this.bridgeStiltMat?.dispose()
    if (this.materials) {
      for (const m of Object.values(this.materials)) {
        m.dispose()
      }
      this.materials = null
    }
    this.scene.dispose()
    this.engine.dispose()
  }

  private setupLighting(): void {
    // Hemispheric — sky/ground ambient. Tuned warm so unlit faces
    // pick up a soft cream color, not the neutral grey Babylon's
    // default would give.
    const hemi = new HemisphericLight('hemi', new Vector3(0, 0, 1), this.scene)
    hemi.diffuse = Color3.FromHexString('#fff8ee')
    hemi.groundColor = Color3.FromHexString('#a89e8e')
    hemi.intensity = 0.55

    // Sun — warm key directional with shadows. Position is set above
    // and to the back-right of the scene so shadows fall forward and
    // to the left, away from the camera's default view angle.
    const sun = new DirectionalLight('sun', new Vector3(-0.45, -0.55, -1).normalize(), this.scene)
    sun.position = new Vector3(800, 600, 1500)
    sun.diffuse = Color3.FromHexString('#fff5e1')
    sun.intensity = 1.4

    // Soft contact-style shadows. Blur exponential shadow maps
    // tolerate transparent casters more gracefully than PCF and
    // give that "studio hotel lobby" softness for free.
    this.shadowGenerator = new ShadowGenerator(2048, sun)
    this.shadowGenerator.useBlurExponentialShadowMap = true
    this.shadowGenerator.blurKernel = 32
    this.shadowGenerator.darkness = 0.3

    // Cool blue rim — a subtle counter-light from the opposite side
    // that picks out edges and ties into the cyan/blue accent
    // palette. No shadows on this one; it's pure form definition.
    const rim = new DirectionalLight('rim', new Vector3(0.6, 0.5, -0.6).normalize(), this.scene)
    rim.diffuse = Color3.FromHexString('#9fc4ff')
    rim.intensity = 0.25
  }
}

/** Wire shift+LMB drag-to-pan on the canvas, layered on top of
 *  Babylon's default camera input.
 *
 *  Two halves to make this work cleanly:
 *
 *  1. **Suppress Babylon's LMB rotate while shift is held.** Listen
 *     for shift keydown/keyup at the window level and toggle button
 *     0 in/out of `pointersInput.buttons`. Babylon's rotate input
 *     consults this list per pointerdown to decide whether to act,
 *     so removing 0 cleanly disables LMB rotation *only* for the
 *     duration of the keypress — releasing shift restores the
 *     original buttons immediately.
 *
 *  2. **Pan via direct world-space unproject.** Shift the camera
 *     target by the world delta between two adjacent screen points.
 *     That gives a 1:1 drag feel regardless of zoom — the world
 *     point under the cursor stays pinned to the cursor — without
 *     having to tune Babylon's perspective-flavored panning
 *     sensibility for ortho mode.
 *
 *  Returns a teardown function that detaches all listeners and
 *  restores the original button list. */
function attachShiftPan(canvas: HTMLCanvasElement, camera: ArcRotateCamera): () => void {
  // ─── Half 1: gate Babylon's rotate input on shift-held state ────
  // The `pointers` entry on `inputs.attached` is the
  // ArcRotateCameraPointersInput; it exposes a public `buttons` array
  // (default [0, 1, 2]) that the rotate handler consults per pointer
  // event. We splice 0 out while shift is held and put it back on
  // release, so rotation is only ever paused mid-keypress.
  const pointersInput = camera.inputs.attached.pointers as { buttons?: number[] } | undefined
  const originalButtons = pointersInput?.buttons ? [...pointersInput.buttons] : null

  let shiftHeld = false
  const setShift = (held: boolean) => {
    if (held === shiftHeld) return
    shiftHeld = held
    if (!pointersInput || !originalButtons) return
    pointersInput.buttons = held ? originalButtons.filter((b) => b !== 0) : [...originalButtons]
  }

  const onKeyDown = (e: KeyboardEvent) => {
    if (e.key === 'Shift') setShift(true)
  }
  const onKeyUp = (e: KeyboardEvent) => {
    if (e.key === 'Shift') setShift(false)
  }
  // Window blur clears any "stuck" shift state — if the user alt-tabs
  // away while holding shift, we'd otherwise leave LMB-rotate disabled
  // until they shift-tap to flush the state.
  const onBlur = () => setShift(false)

  // ─── Half 2: pan via unproject ──────────────────────────────────
  let activePointerId: number | null = null
  let lastX = 0
  let lastY = 0
  // Reused on every move to avoid allocating fresh Vector3s at
  // 120Hz trackpad rates.
  const tmpA = new Vector3()
  const tmpB = new Vector3()

  const onDown = (e: PointerEvent) => {
    if (e.button !== 0 || !e.shiftKey) return
    activePointerId = e.pointerId
    lastX = e.clientX
    lastY = e.clientY
    canvas.setPointerCapture(e.pointerId)
    e.preventDefault()
  }

  const onMove = (e: PointerEvent) => {
    if (e.pointerId !== activePointerId) return
    const dx = e.clientX - lastX
    const dy = e.clientY - lastY
    lastX = e.clientX
    lastY = e.clientY

    const rect = canvas.getBoundingClientRect()
    if (rect.width <= 0 || rect.height <= 0) return

    // Unproject (0,0) and (dx,dy) at the same screen-space depth and
    // take the world-space delta. The fixed z=0.5 is arbitrary in
    // ortho — the projection's z range collapses to a single xy plane
    // — so any value works, and the result is independent of zoom.
    const view = camera.getViewMatrix()
    const proj = camera.getProjectionMatrix()
    const id = Matrix.IdentityReadOnly
    tmpA.set(0, 0, 0.5)
    tmpB.set(dx, dy, 0.5)
    const a = Vector3.Unproject(tmpA, rect.width, rect.height, id, view, proj)
    const b = Vector3.Unproject(tmpB, rect.width, rect.height, id, view, proj)
    // Mutate the target Vector3 in place rather than going through the
    // setter. ArcRotateCamera's `target` setter calls
    // `rebuildAnglesAndRadius`, which re-derives alpha/beta/radius
    // from the camera's *current* position relative to the *new*
    // target — so position stays put and the view appears to swing
    // (rotate-flavored), instead of rigidly translating. Direct
    // mutation leaves alpha/beta/radius untouched; on the next
    // `_checkInputs` tick Babylon recomputes position as
    // `target + radius * (cosa*sinb, sina*sinb, cosb)`, giving the
    // rigid pan we actually want.
    camera.target.addInPlace(a.subtract(b))

    e.preventDefault()
  }

  const onUp = (e: PointerEvent) => {
    if (e.pointerId !== activePointerId) return
    activePointerId = null
    if (canvas.hasPointerCapture(e.pointerId)) {
      canvas.releasePointerCapture(e.pointerId)
    }
  }

  window.addEventListener('keydown', onKeyDown)
  window.addEventListener('keyup', onKeyUp)
  window.addEventListener('blur', onBlur)
  canvas.addEventListener('pointerdown', onDown)
  canvas.addEventListener('pointermove', onMove)
  canvas.addEventListener('pointerup', onUp)
  canvas.addEventListener('pointercancel', onUp)

  return () => {
    setShift(false)
    window.removeEventListener('keydown', onKeyDown)
    window.removeEventListener('keyup', onKeyUp)
    window.removeEventListener('blur', onBlur)
    canvas.removeEventListener('pointerdown', onDown)
    canvas.removeEventListener('pointermove', onMove)
    canvas.removeEventListener('pointerup', onUp)
    canvas.removeEventListener('pointercancel', onUp)
  }
}
