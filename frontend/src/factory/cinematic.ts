// Cinematic mode — an ambient shot-director for the factory view.
//
// Drives the existing ArcRotateCamera through a slow, eased,
// auto-advancing tour of the factory. The subject is the *architecture*
// — belts, bridges, the loop — which is well-composed whether the floor
// is busy or empty. Live chip motion only *weights* which scenic shot
// comes next (see pickNextShot); it never picks the shot and is never
// chased. That's the whole answer to "keep the camera on something
// without fixating on the same three busy spots."
//
// This module is the scene-agnostic machinery: easing, the shot/pose
// types, the selection policy, and the per-frame director. The concrete
// deck (which bridges, which belt runs, their framing) is authored in
// iso-scene.ts where the floor's grid constants live and handed in.
//
// smootherstep / pickNextShot are pure and exported for unit testing if
// a frontend harness ever lands (assert no back-to-back repeats; assert
// quiet regions keep a nonzero pick probability).

import { type ArcRotateCamera, type Observer, type Scene, Vector3 } from '@babylonjs/core'

// ─── Easing ────────────────────────────────────────────────────────

/** Perlin's smootherstep: zero first AND second derivative at both
 *  ends, so every camera move starts and stops with no perceptible
 *  jerk. The difference between "a pan" and "a glide." */
export function smootherstep(t: number): number {
  const x = t < 0 ? 0 : t > 1 ? 1 : t
  return x * x * x * (x * (x * 6 - 15) + 10)
}

// ─── Pose + shot model ───────────────────────────────────────────────

/** A camera pose on the ArcRotateCamera's four drivable channels. */
export interface Pose {
  alpha: number
  beta: number
  radius: number
  target: Vector3
}

/** The concrete move a shot performs, resolved fresh each time the shot
 *  is chosen so content-aware shots (belt truck, station detail) can
 *  re-pick their subject against the current floor. */
export interface ShotMotion {
  /** Pose the camera eases TO at the start of the hold. */
  alpha: number
  beta: number
  radius: number
  target: Vector3
  /** Seconds to hold after the ease-in before advancing. Infinity for
   *  the reduced-motion single-drift mode. */
  hold: number
  /** Continuous motion during the hold, per second, added on top of the
   *  held pose. driftAlpha = slow orbit, craneBeta = pitch rises,
   *  driftTargetX = lateral target slide. */
  driftAlpha?: number
  craneBeta?: number
  driftTargetX?: number
  /** Belt-truck travel: when set, the target glides start→targetTo over
   *  the hold (and the drift* fields are ignored). */
  targetTo?: Vector3
}

/** One entry in the director's deck. */
export interface Shot {
  id: string
  /** Scenic weight floor — what the shot is worth on composition alone,
   *  before activity or cooldown. High for the hero bridges so they
   *  carry their own weight even on an empty floor. */
  base: number
  /** World x/y box used to count chips "in frame" for activity
   *  weighting. null = a wide shot that frames the whole floor; those
   *  aren't activity-weighted or they'd always win (they contain every
   *  chip). */
  region: { cx: number; cy: number; halfX: number; halfY: number } | null
  /** Resolve the concrete move. Returns null to decline (e.g. the
   *  station shot when nothing has an active run); the director re-picks. */
  resolve: () => ShotMotion | null
}

// ─── Selection policy ────────────────────────────────────────────────

/** Each chip inside a shot's framing box is worth this many scenic
 *  points. Tuned so a single chip crossing a hero bridge tilts the dice
 *  toward it without letting a busy corner monopolise the tour. */
const ACTIVITY_GAIN = 3

/** Pick the next shot by weighted-random sample — never argmax (argmax
 *  is exactly what produces "the same three spots forever"). Cooldown
 *  zeroes anything shown in the last K picks; the base scenic floor
 *  keeps quiet-but-pretty regions in rotation; activity tops up by how
 *  many chips currently sit inside the framing box. */
export function pickNextShot(
  deck: Shot[],
  chips: { x: number; y: number }[],
  recent: string[],
  rng: () => number,
): Shot | null {
  let total = 0
  const weights = deck.map((s) => {
    if (recent.includes(s.id)) return 0
    let activity = 0
    if (s.region) {
      for (const c of chips) {
        if (
          Math.abs(c.x - s.region.cx) <= s.region.halfX &&
          Math.abs(c.y - s.region.cy) <= s.region.halfY
        ) {
          activity++
        }
      }
    }
    const w = s.base + ACTIVITY_GAIN * activity
    total += w
    return w
  })
  if (total <= 0) {
    // Everything is on cooldown (deck smaller than K, or a degenerate
    // round) — uniform pick ignoring cooldown so the tour never stalls.
    return deck.length ? deck[Math.floor(rng() * deck.length)] : null
  }
  let r = rng() * total
  for (let i = 0; i < deck.length; i++) {
    r -= weights[i]
    if (r < 0) return deck[i]
  }
  return deck[deck.length - 1]
}

// ─── Director ────────────────────────────────────────────────────────

const TRANSITION_SEC = 1.4 // ease between consecutive shots
const RESTORE_SEC = 1.1 // ease back to the user's pose on exit
const COOLDOWN_K = 3 // a shot can't replay until K others have run
const DT_CAP = 0.05 // clamp per-frame advance so a tab-wake hitch can't jump the camera

// Aesthetic clamps — kept inside the camera's own limits (beta
// 0.001..~π/2, radius 266..5000) so we never fight Babylon's bounds.
const BETA_MIN = 0.05
const BETA_MAX = 1.5
const RADIUS_MIN = 270 // ~ the camera's lowerRadiusLimit — the tightest zoom it allows
const RADIUS_MAX = 2800

type Phase = 'idle' | 'transition' | 'hold' | 'restore'

export class CinematicDirector {
  private observer: Observer<Scene> | null = null
  private endListeners = new Set<() => void>()

  private phase: Phase = 'idle'
  private elapsed = 0
  private startPose: Pose
  private holdPose: Pose
  private motion: ShotMotion | null = null
  private enterPose: Pose | null = null
  private recent: string[] = []
  private reduced = false
  // Reused for every per-frame camera-target write so a hold — especially
  // the reduced-motion infinite hold — doesn't allocate a Vector3 each
  // frame. Safe because the camera's target setter copies the value and
  // snapshot() clones before storing, so nothing aliases this instance.
  private readonly targetScratch = new Vector3()

  // Explicit field declarations + assignment rather than constructor
  // parameter properties — the project builds with `erasableSyntaxOnly`,
  // which forbids the latter (they emit code, not just types).
  private readonly camera: ArcRotateCamera
  private readonly scene: Scene
  private readonly deck: Shot[]
  private readonly getChips: () => { x: number; y: number }[]
  private readonly rng: () => number

  constructor(
    camera: ArcRotateCamera,
    scene: Scene,
    deck: Shot[],
    getChips: () => { x: number; y: number }[],
    rng: () => number = Math.random,
  ) {
    this.camera = camera
    this.scene = scene
    this.deck = deck
    this.getChips = getChips
    this.rng = rng
    this.startPose = this.snapshot()
    this.holdPose = this.snapshot()
  }

  get active(): boolean {
    return this.phase !== 'idle'
  }

  /** Subscribe to self/explicit exit completion (after the camera has
   *  fully eased back). Returns an unsubscribe. */
  onEnd(cb: () => void): () => void {
    this.endListeners.add(cb)
    return () => this.endListeners.delete(cb)
  }

  /** Begin the tour from wherever the camera currently sits. `reduced`
   *  collapses the whole thing to a single slow establishing drift with
   *  no cuts (prefers-reduced-motion). */
  enter(reduced: boolean): void {
    if (this.active) return
    this.reduced = reduced
    this.enterPose = this.snapshot()
    this.recent = []
    this.startNextFrom(this.enterPose)
    if (!this.observer) {
      this.observer = this.scene.onBeforeRenderObservable.add(() => this.tick())
    }
  }

  /** Glide back to the pose held before entering, then release. The
   *  onEnd listeners fire only once that restore completes, so the HUD
   *  doesn't pop back while the camera is still moving. */
  exit(): void {
    // No-op when idle or already restoring. In V1 no shot self-exits, so
    // the director is idle only while camera controls are attached (enter
    // detaches; the onEnd after a restore re-attaches) — a stray exit()
    // therefore can't strand controls detached. If a self-exiting shot is
    // ever added, route its completion through the same restore→onEnd path.
    if (this.phase === 'idle' || this.phase === 'restore') return
    this.startPose = this.snapshot()
    this.elapsed = 0
    this.phase = 'restore'
  }

  dispose(): void {
    if (this.observer) {
      this.scene.onBeforeRenderObservable.remove(this.observer)
      this.observer = null
    }
    this.endListeners.clear()
    this.phase = 'idle'
  }

  // ── internals ──

  private snapshot(): Pose {
    return {
      alpha: this.camera.alpha,
      beta: this.camera.beta,
      radius: this.camera.radius,
      target: this.camera.target.clone(),
    }
  }

  private tick(): void {
    if (this.phase === 'idle') return
    const dt = Math.min(this.scene.getEngine().getDeltaTime() / 1000, DT_CAP)
    if (dt <= 0) return

    if (this.phase === 'transition') {
      this.elapsed += dt
      this.applyLerp(this.startPose, this.holdPose, smootherstep(this.elapsed / TRANSITION_SEC))
      if (this.elapsed >= TRANSITION_SEC) {
        this.phase = 'hold'
        this.elapsed = 0
      }
    } else if (this.phase === 'hold') {
      this.elapsed += dt
      this.applyHoldMotion(this.elapsed)
      if (this.motion && this.elapsed >= this.motion.hold) {
        // Advance — the next shot eases from the current (drifted) pose.
        this.startNextFrom(this.snapshot())
      }
    } else if (this.phase === 'restore') {
      this.elapsed += dt
      this.applyLerp(this.startPose, this.enterPose!, smootherstep(this.elapsed / RESTORE_SEC))
      if (this.elapsed >= RESTORE_SEC) this.finishExit()
    }
  }

  private startNextFrom(from: Pose): void {
    const { motion, id } = this.reduced ? this.reducedHold() : this.nextHold()
    this.recent.push(id)
    while (this.recent.length > COOLDOWN_K) this.recent.shift()
    this.motion = motion
    this.startPose = from
    this.holdPose = {
      alpha: motion.alpha,
      beta: motion.beta,
      radius: motion.radius,
      target: motion.target.clone(),
    }
    this.elapsed = 0
    this.phase = 'transition'
  }

  private nextHold(): { motion: ShotMotion; id: string } {
    const chips = this.getChips()
    const excluded: string[] = []
    // Up to deck.length tries to satisfy resolve() (a shot may decline,
    // e.g. station-detail when nothing is running). Wide shots never
    // decline, so this terminates.
    for (let i = 0; i <= this.deck.length; i++) {
      const shot = pickNextShot(this.deck, chips, [...this.recent, ...excluded], this.rng)
      if (!shot) break
      const motion = shot.resolve()
      if (motion) return { motion, id: shot.id }
      excluded.push(shot.id)
    }
    return { motion: this.fallbackMotion(), id: 'fallback' }
  }

  private reducedHold(): { motion: ShotMotion; id: string } {
    const est = this.deck.find((s) => s.id === 'establishing') ?? this.deck[0]
    const m = est?.resolve() ?? this.fallbackMotion()
    return {
      id: 'establishing',
      motion: {
        alpha: m.alpha,
        beta: m.beta,
        radius: m.radius,
        target: m.target.clone(),
        hold: Infinity, // never cut
        driftAlpha: (m.driftAlpha ?? 0.02) * 0.5, // half speed
      },
    }
  }

  /** Defensive only — the deck always carries wide shots that never
   *  decline, so this is effectively unreachable. Holds the current
   *  pose with a gentle orbit. */
  private fallbackMotion(): ShotMotion {
    const p = this.snapshot()
    return {
      alpha: p.alpha,
      beta: p.beta,
      radius: p.radius,
      target: p.target,
      hold: 6,
      driftAlpha: 0.02,
    }
  }

  private applyLerp(a: Pose, b: Pose, t: number): void {
    this.camera.alpha = lerpAngle(a.alpha, b.alpha, t)
    this.camera.beta = clamp(lerp(a.beta, b.beta, t), BETA_MIN, BETA_MAX)
    this.camera.radius = clamp(lerp(a.radius, b.radius, t), RADIUS_MIN, RADIUS_MAX)
    Vector3.LerpToRef(a.target, b.target, t, this.targetScratch)
    this.camera.target = this.targetScratch
  }

  private applyHoldMotion(e: number): void {
    const m = this.motion
    if (!m) return
    const base = this.holdPose
    if (m.targetTo) {
      this.camera.alpha = base.alpha
      this.camera.beta = clamp(base.beta, BETA_MIN, BETA_MAX)
      this.camera.radius = clamp(base.radius, RADIUS_MIN, RADIUS_MAX)
      Vector3.LerpToRef(
        base.target,
        m.targetTo,
        smootherstep(Math.min(1, e / m.hold)),
        this.targetScratch,
      )
      this.camera.target = this.targetScratch
      return
    }
    // Wrap to [0, 2π). In reduced-motion mode this branch holds forever
    // (hold: Infinity), so an unwrapped alpha would grow without bound
    // over a long kiosk session; 2π ≡ 0 visually, so the wrap is seamless.
    this.camera.alpha = wrapTwoPi(base.alpha + (m.driftAlpha ?? 0) * e)
    this.camera.beta = clamp(base.beta + (m.craneBeta ?? 0) * e, BETA_MIN, BETA_MAX)
    this.camera.radius = clamp(base.radius, RADIUS_MIN, RADIUS_MAX)
    // driftTargetX is a world-space delta and is deliberately NOT wrapped
    // or clamped here (the director stays scene-agnostic — it has no floor
    // bounds). The only infinite hold is reducedHold, which carries just a
    // wrapped alpha drift, so x can't run away; any shot using driftTargetX
    // must keep a finite hold (the deck's `blueprint` does).
    this.targetScratch.copyFrom(base.target)
    this.targetScratch.x += (m.driftTargetX ?? 0) * e
    this.camera.target = this.targetScratch
  }

  private finishExit(): void {
    this.phase = 'idle'
    this.motion = null
    this.enterPose = null
    this.recent = []
    if (this.observer) {
      this.scene.onBeforeRenderObservable.remove(this.observer)
      this.observer = null
    }
    for (const cb of this.endListeners) cb()
  }
}

// ─── small math ──────────────────────────────────────────────────────

function lerp(a: number, b: number, t: number): number {
  return a + (b - a) * t
}

function clamp(x: number, lo: number, hi: number): number {
  return x < lo ? lo : x > hi ? hi : x
}

/** Lerp an angle along the *shortest* arc, so a shot that ends with a
 *  drifted alpha doesn't spin all the way around to reach the next
 *  shot's azimuth. */
function lerpAngle(a: number, b: number, t: number): number {
  const tau = Math.PI * 2
  let d = (b - a) % tau
  if (d > Math.PI) d -= tau
  if (d < -Math.PI) d += tau
  return a + d * t
}

/** Wrap an angle into [0, 2π) so a continuous drift can't grow it
 *  without bound (2π ≡ 0, so wrapping never moves the camera). */
function wrapTwoPi(a: number): number {
  const tau = Math.PI * 2
  return ((a % tau) + tau) % tau
}
