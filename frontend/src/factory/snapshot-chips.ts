// Snapshot-driven chip mesh manager.
//
// One chip mesh per entity in transit; reconcile() is called every
// frame with the live set of transit placements, which pose the
// existing meshes, build new ones for newly in-flight entities, and
// dispose meshes for entities that are no longer in transit.
//
// Unlike ItemSimulator (which owns its own time-driven progress
// state), this controller has no internal animation loop — chip
// pose comes purely from the `progress` value in each placement.
// That value is derived snapshot-side from `(now - last_event_at) /
// duration`, so chip motion stays locked to backend authority and
// can never disagree with station tray counts.

import {
  Color3,
  DynamicTexture,
  type Mesh,
  MeshBuilder,
  PBRMaterial,
  type Scene,
  StandardMaterial,
  Texture,
  TransformNode,
} from '@babylonjs/core'

import type { PathSegment } from './iso-path'

const ITEM_DIAM = 32
const ITEM_HEIGHT = 14
const ITEM_LIFT = 0.5
const ITEM_CORE_DIAM_FRAC = 0.5
const ITEM_CORE_HEIGHT_FRAC = 0.6
const CORE_HUE_SATURATION = 0.78
const CORE_HUE_VALUE = 1.0
const CORE_EMISSIVE_INTENSITY = 1.4
const LABEL_PLATE_FRAC = 0.78
const LABEL_PLATE_LIFT = 0.4
const LABEL_TEX_PX = 128

export interface ChipDecor {
  /** [0, 360) hue applied to the chip's emissive core. Stable per
   *  entity (hashed from repo / project key). */
  hue?: number
  /** Short string drawn on top of the chip — PR number, Jira key. */
  label?: string
}

export interface TransitPlacement {
  from: string
  to: string
  /** [0, 1] along the precomputed itinerary. */
  progress: number
  decor: ChipDecor
}

interface ChipState {
  fromTo: string // `${from}\0${to}` — recreates the mesh on retarget
  itinerary: PathSegment[]
  totalLength: number
  root: TransformNode
  shell: Mesh
  core: Mesh
  labelMesh: Mesh | null
  labelKey: string | null
  hueKey: number | null
}

export class SnapshotChipController {
  private scene: Scene
  private shellMat: PBRMaterial
  private fallbackCoreMat: PBRMaterial
  private coreMaterials = new Map<number, PBRMaterial>()
  private labelMaterials = new Map<string, StandardMaterial>()
  private chips = new Map<string, ChipState>()
  private nextSeq = 0

  constructor(scene: Scene, shellMat: PBRMaterial, fallbackCoreMat: PBRMaterial) {
    this.scene = scene
    this.shellMat = shellMat
    this.fallbackCoreMat = fallbackCoreMat
  }

  /** Reconcile the live mesh set against the supplied transit map.
   *
   *  - New entities: build a chip mesh, pose at progress.
   *  - Existing entities with same from→to: pose at progress.
   *  - Existing entities with different from→to (retarget mid-flight):
   *    dispose old mesh, build new at progress 0 (snap to new transit
   *    rather than smooth-handoff — keeps backend the sole source of
   *    truth, see design doc).
   *  - Entities no longer in transit: dispose mesh.
   *
   *  `getItinerary(from, to)` returns null on no-path; chips for those
   *  pairs are skipped (the entity will appear parked at its
   *  destination once the transit window expires). */
  reconcile(
    transits: Map<string, TransitPlacement>,
    getItinerary: (from: string, to: string) => PathSegment[] | null,
  ): void {
    for (const [entityId, t] of transits) {
      const fromTo = `${t.from}\0${t.to}`
      let chip = this.chips.get(entityId)
      if (chip && chip.fromTo !== fromTo) {
        this.disposeChip(chip)
        this.chips.delete(entityId)
        chip = undefined
      }
      if (!chip) {
        const itinerary = getItinerary(t.from, t.to)
        if (!itinerary || itinerary.length === 0) continue
        chip = this.buildChip(entityId, fromTo, itinerary, t.decor)
        this.chips.set(entityId, chip)
      } else {
        this.applyDecor(chip, t.decor)
      }
      this.poseChipAt(chip, t.progress)
    }
    for (const [entityId, chip] of this.chips) {
      if (!transits.has(entityId)) {
        this.disposeChip(chip)
        this.chips.delete(entityId)
      }
    }
  }

  /** World x/y of every chip currently in transit. Cinematic mode
   *  reads this to score how much live motion sits inside a candidate
   *  shot's framing box — the "watch where the work is" weighting. */
  collectChipXY(): { x: number; y: number }[] {
    const out: { x: number; y: number }[] = []
    for (const chip of this.chips.values()) {
      out.push({ x: chip.root.position.x, y: chip.root.position.y })
    }
    return out
  }

  destroy(): void {
    for (const chip of this.chips.values()) this.disposeChip(chip)
    this.chips.clear()
    for (const m of this.coreMaterials.values()) m.dispose()
    this.coreMaterials.clear()
    for (const m of this.labelMaterials.values()) m.dispose()
    this.labelMaterials.clear()
  }

  private buildChip(
    entityId: string,
    fromTo: string,
    itinerary: PathSegment[],
    decor: ChipDecor,
  ): ChipState {
    const id = `chip-${this.nextSeq++}-${entityId}`
    const root = new TransformNode(`${id}-root`, this.scene)

    // Shell + core stand up via parent.rotation.x = π/2 — see the
    // long comment in iso-items.ts for why parent owns Z and child
    // owns X. Same trick here.
    const shell = MeshBuilder.CreateCylinder(
      `${id}-shell`,
      { diameter: ITEM_DIAM, height: ITEM_HEIGHT, tessellation: 28 },
      this.scene,
    )
    shell.rotation.x = Math.PI / 2
    shell.material = this.shellMat
    shell.parent = root

    const core = MeshBuilder.CreateCylinder(
      `${id}-core`,
      {
        diameter: ITEM_DIAM * ITEM_CORE_DIAM_FRAC,
        height: ITEM_HEIGHT * ITEM_CORE_HEIGHT_FRAC,
        tessellation: 20,
      },
      this.scene,
    )
    core.rotation.x = Math.PI / 2
    core.parent = root

    const totalLength = itinerary.reduce((acc, seg) => acc + seg.length, 0)
    const chip: ChipState = {
      fromTo,
      itinerary,
      totalLength,
      root,
      shell,
      core,
      labelMesh: null,
      labelKey: null,
      hueKey: null,
    }
    this.applyDecor(chip, decor)
    return chip
  }

  private applyDecor(chip: ChipState, decor: ChipDecor): void {
    const hueKey = decor.hue == null ? null : Math.round(((decor.hue % 360) + 360) % 360)
    if (hueKey !== chip.hueKey) {
      chip.core.material = hueKey == null ? this.fallbackCoreMat : this.coreMaterialFor(hueKey)
      chip.hueKey = hueKey
    }
    const labelKey = decor.label ?? null
    if (labelKey !== chip.labelKey) {
      if (chip.labelMesh) {
        chip.labelMesh.dispose()
        chip.labelMesh = null
      }
      if (labelKey) {
        chip.labelMesh = this.buildLabelMesh(chip.root.name, labelKey, chip.root)
      }
      chip.labelKey = labelKey
    }
  }

  private poseChipAt(chip: ChipState, progress: number): void {
    const clamped = Math.max(0, Math.min(progress, 1))
    const target = clamped * chip.totalLength
    let acc = 0
    for (const seg of chip.itinerary) {
      if (acc + seg.length >= target) {
        const local = target - acc
        const { position, tangent } = seg.sample(local)
        chip.root.position.set(position.x, position.y, position.z + ITEM_HEIGHT / 2 + ITEM_LIFT)
        chip.root.rotation.z = Math.atan2(tangent.y, tangent.x)
        return
      }
      acc += seg.length
    }
    const last = chip.itinerary[chip.itinerary.length - 1]
    const { position, tangent } = last.sample(last.length)
    chip.root.position.set(position.x, position.y, position.z + ITEM_HEIGHT / 2 + ITEM_LIFT)
    chip.root.rotation.z = Math.atan2(tangent.y, tangent.x)
  }

  private coreMaterialFor(key: number): PBRMaterial {
    const cached = this.coreMaterials.get(key)
    if (cached) return cached
    const m = new PBRMaterial(`chip-core-${key}`, this.scene)
    m.albedoColor = Color3.Black()
    m.emissiveColor = Color3.FromHSV(key, CORE_HUE_SATURATION, CORE_HUE_VALUE)
    m.emissiveIntensity = CORE_EMISSIVE_INTENSITY
    m.metallic = 0
    m.roughness = 1
    this.coreMaterials.set(key, m)
    return m
  }

  private buildLabelMesh(rootName: string, label: string, parent: TransformNode): Mesh {
    const plate = MeshBuilder.CreatePlane(
      `${rootName}-label`,
      { size: ITEM_DIAM * LABEL_PLATE_FRAC },
      this.scene,
    )
    plate.position.set(0, 0, ITEM_HEIGHT / 2 + LABEL_PLATE_LIFT)
    plate.material = this.labelMaterialFor(label)
    plate.parent = parent
    return plate
  }

  private labelMaterialFor(label: string): StandardMaterial {
    const cached = this.labelMaterials.get(label)
    if (cached) return cached
    const tex = new DynamicTexture(
      `chip-label-${label}`,
      { width: LABEL_TEX_PX, height: LABEL_TEX_PX },
      this.scene,
      false,
    )
    tex.hasAlpha = true
    const ctx = tex.getContext() as CanvasRenderingContext2D
    ctx.clearRect(0, 0, LABEL_TEX_PX, LABEL_TEX_PX)
    ctx.fillStyle = '#ffffff'
    const fontPx = label.length <= 4 ? 56 : label.length <= 6 ? 42 : 32
    ctx.font = `700 ${fontPx}px ui-sans-serif, system-ui, -apple-system, sans-serif`
    ctx.textAlign = 'center'
    ctx.textBaseline = 'middle'
    // V-flip — Babylon's right-handed UV layout flips V on +z faces.
    ctx.save()
    ctx.translate(0, LABEL_TEX_PX)
    ctx.scale(1, -1)
    ctx.fillText(label, LABEL_TEX_PX / 2, LABEL_TEX_PX / 2)
    ctx.restore()
    tex.update()

    const m = new StandardMaterial(`chip-label-mat-${label}`, this.scene)
    m.diffuseTexture = tex
    m.diffuseTexture.hasAlpha = true
    m.useAlphaFromDiffuseTexture = true
    m.emissiveTexture = tex
    m.emissiveColor = new Color3(0.95, 0.95, 0.95)
    m.disableLighting = true
    m.backFaceCulling = false
    ;(m.diffuseTexture as Texture).wrapU = Texture.CLAMP_ADDRESSMODE
    ;(m.diffuseTexture as Texture).wrapV = Texture.CLAMP_ADDRESSMODE
    this.labelMaterials.set(label, m)
    return m
  }

  private disposeChip(chip: ChipState): void {
    chip.labelMesh?.dispose()
    chip.shell.dispose()
    chip.core.dispose()
    chip.root.dispose()
  }
}
