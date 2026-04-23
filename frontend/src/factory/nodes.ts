// Routing primitives placed along belts — splitters, mergers, and poles.
//
// Unlike stations (which own event semantics), these are pure routing:
// stations stay 1-in-1-out so every multi-target branch or multi-source
// convergence lives on one of these lightweight nodes. Items transit
// through without dwelling.
//
//   - splitter: 1 input side, up to 3 output sides. Diamond with outward
//     ticks. `orientation` names the input side (L/R/T/B); the other
//     three sides are outputs.
//   - merger: 3 input sides, 1 output side. Diamond with inward ticks.
//     `orientation` names the output side.
//   - pole: small round waypoint with 4 sides. Purely visual/routing,
//     no semantic. Used to bend belts at grid corners.

import { Container, Graphics } from 'pixi.js'
import { BELT_WIDTH, type Port } from './station'

const ACCENT = 0xc47a5a
const NODE_R = 24 // half-side of the rounded-square body
const POLE_R = 8
const TUNNEL_R = 10 // half-size of the tunnel endpoint square

export type NodeKind = 'splitter' | 'merger'
export type Side = 'left' | 'right' | 'top' | 'bottom'
export type TunnelRole = 'entrance' | 'exit'

export interface NodeOptions {
  kind: NodeKind
  center: { x: number; y: number }
  label?: string
  /** Input side for splitter, output side for merger. Data-only for now
   * — doesn't rotate the visual (diamond is symmetric and ticks are
   * drawn on all four sides). Used by the routing layer to identify
   * which port is input vs. output. */
  orientation?: Side
}

export interface NodeHandle {
  kind: NodeKind
  center: { x: number; y: number }
  leftPort: Port
  rightPort: Port
  topPort: Port
  bottomPort: Port
  update(dt: number): void
}

export interface PoleHandle {
  kind: 'pole'
  center: { x: number; y: number }
  leftPort: Port
  rightPort: Port
  topPort: Port
  bottomPort: Port
  update(dt: number): void
}

export interface TunnelOptions {
  role: TunnelRole
  /** Which side of the endpoint has the external-belt port. The opposite
   * side is where the tunnel "goes" (invisible). */
  side: Side
  center: { x: number; y: number }
}

export interface TunnelHandle {
  kind: 'tunnel_entrance' | 'tunnel_exit'
  center: { x: number; y: number }
  /** Exactly one of these four will be defined — the side specified by
   * `side` in TunnelOptions. Others are undefined. */
  leftPort?: Port
  rightPort?: Port
  topPort?: Port
  bottomPort?: Port
  update(dt: number): void
}

export function buildNode(parent: Container, opts: NodeOptions): NodeHandle {
  const { kind, center, orientation } = opts

  const root = new Container()
  root.x = center.x
  root.y = center.y
  parent.addChild(root)

  // Body: rounded square flush with the belt width on every side, axis-
  // aligned rather than diamond so the conveyor visually slots INTO the
  // block instead of meeting a pointed corner. Ports land exactly on
  // the body edges at center.y / center.x ± NODE_R, so belts connect
  // without any visible gap.
  const B = NODE_R
  const CORNER_R = 6

  // Drop shadow.
  const shadow = new Graphics()
  shadow.roundRect(-B, -B + 3, B * 2, B * 2, CORNER_R)
  shadow.fill({ color: 0x000000, alpha: 0.08 })
  root.addChild(shadow)

  // Main glass body — same liquid-glass layering as stations, scaled down.
  const body = new Graphics()
  body.roundRect(-B, -B, B * 2, B * 2, CORNER_R)
  body.fill({ color: 0xffffff, alpha: 0.92 })
  root.addChild(body)

  const tint = new Graphics()
  tint.roundRect(-B, -B, B * 2, B * 2, CORNER_R)
  tint.fill({ color: ACCENT, alpha: 0.09 })
  root.addChild(tint)

  // Inner highlight — inset rounded-rect stroke for depth.
  const inner = new Graphics()
  inner.roundRect(-B + 3, -B + 3, B * 2 - 6, B * 2 - 6, CORNER_R - 1)
  inner.stroke({ width: 0.75, color: 0xffffff, alpha: 0.85 })
  root.addChild(inner)

  // Outer rim — accent, hairline.
  const rim = new Graphics()
  rim.roundRect(-B, -B, B * 2, B * 2, CORNER_R)
  rim.stroke({ width: 1, color: ACCENT, alpha: 0.55 })
  root.addChild(rim)

  // Directional chevrons — one per side, placed just inside the edge,
  // pointing in the flow direction at that port. For a splitter the
  // orientation side is the input (chevron points INward); the other
  // three sides are outputs (chevrons point OUTward). Mergers invert.
  //
  // Rendered as open-stroke chevrons at the same weight as the belt
  // chevrons so the routing primitives feel like members of the same
  // family — quiet direction-of-flow hints, not punchy arrowheads.
  const sides: Array<{ side: Side; dx: number; dy: number }> = [
    { side: 'left', dx: -1, dy: 0 },
    { side: 'right', dx: 1, dy: 0 },
    { side: 'top', dx: 0, dy: -1 },
    { side: 'bottom', dx: 0, dy: 1 },
  ]
  const arrows = new Graphics()
  for (const { side, dx, dy } of sides) {
    const isOrientation = side === orientation
    const isOutputSide = kind === 'splitter' ? !isOrientation : isOrientation
    const flowSign = isOutputSide ? 1 : -1 // +1 = points outward, -1 = points inward
    const ax = dx * B * 0.55
    const ay = dy * B * 0.55
    drawChevron(arrows, ax, ay, dx * flowSign, dy * flowSign)
  }
  arrows.stroke({ width: 1.25, color: ACCENT, alpha: 0.5 })
  root.addChild(arrows)

  return {
    kind,
    center,
    leftPort: { x: center.x - B, y: center.y, dir: { x: -1, y: 0 } },
    rightPort: { x: center.x + B, y: center.y, dir: { x: 1, y: 0 } },
    topPort: { x: center.x, y: center.y - B, dir: { x: 0, y: -1 } },
    bottomPort: { x: center.x, y: center.y + B, dir: { x: 0, y: 1 } },
    update() {
      // No ambient animation yet — nodes are quiet. Hook in if we want
      // splitter-activating pulses when items route through.
    },
  }
}

// drawChevron draws an open (unfilled) "V" chevron at (cx, cy) pointing
// in direction (px, py). Caller is responsible for calling .stroke() on
// the Graphics after all chevrons have been added so they share a single
// draw call. Direction uses unit vectors — exactly one of (px, py) is ±1.
function drawChevron(g: Graphics, cx: number, cy: number, px: number, py: number) {
  const s = 4 // half-width along the base; tip sits one unit ahead
  if (px === 1) {
    g.moveTo(cx - s, cy - s)
    g.lineTo(cx + s, cy)
    g.lineTo(cx - s, cy + s)
  } else if (px === -1) {
    g.moveTo(cx + s, cy - s)
    g.lineTo(cx - s, cy)
    g.lineTo(cx + s, cy + s)
  } else if (py === 1) {
    g.moveTo(cx - s, cy - s)
    g.lineTo(cx, cy + s)
    g.lineTo(cx + s, cy - s)
  } else if (py === -1) {
    g.moveTo(cx - s, cy + s)
    g.lineTo(cx, cy - s)
    g.lineTo(cx + s, cy + s)
  }
}

export function buildTunnel(parent: Container, opts: TunnelOptions): TunnelHandle {
  // Tunnel endpoints — small rounded squares marking where a belt enters
  // or exits an "underground" routing segment. Entrance and exit are
  // drawn identically; the directional meaning is implicit from the
  // edges connected to them. Three stacked dashes inside suggest "layers
  // of depth" (the tunnel).
  //
  // Unlike poles (which have all four ports), a tunnel endpoint exposes
  // ONLY the side facing the external belt. The other three sides would
  // conflict with the invisible tunnel connection they're paired to.
  const { role, side, center } = opts

  const root = new Container()
  root.x = center.x
  root.y = center.y
  parent.addChild(root)

  const shadow = new Graphics()
  shadow.roundRect(-TUNNEL_R, -TUNNEL_R + 2, TUNNEL_R * 2, TUNNEL_R * 2, 3)
  shadow.fill({ color: 0x000000, alpha: 0.1 })
  root.addChild(shadow)

  const body = new Graphics()
  body.roundRect(-TUNNEL_R, -TUNNEL_R, TUNNEL_R * 2, TUNNEL_R * 2, 3)
  body.fill({ color: 0xffffff, alpha: 0.93 })
  body.stroke({ width: 0.75, color: ACCENT, alpha: 0.6 })
  root.addChild(body)

  // Tunnel-depth hint — three small dashes stacked, suggesting layered
  // darkness beneath. Orientation matches the tunnel axis: if the port
  // is on left/right the tunnel runs horizontally (dashes vertical), if
  // top/bottom the tunnel runs vertically (dashes horizontal).
  const dashes = new Graphics()
  const horizontal = side === 'left' || side === 'right'
  for (let i = -1; i <= 1; i++) {
    if (horizontal) {
      dashes.rect(-0.6, i * 3 - 0.6, 1.2, 1.2)
    } else {
      dashes.rect(i * 3 - 0.6, -0.6, 1.2, 1.2)
    }
  }
  dashes.fill({ color: ACCENT, alpha: 0.55 })
  root.addChild(dashes)

  // Small opening indicator on the port side — a thin gap in the border
  // suggesting "the belt comes in/out here."
  const opening = new Graphics()
  const OP_LEN = 7
  switch (side) {
    case 'left':
      opening.moveTo(-TUNNEL_R, -OP_LEN / 2)
      opening.lineTo(-TUNNEL_R, OP_LEN / 2)
      break
    case 'right':
      opening.moveTo(TUNNEL_R, -OP_LEN / 2)
      opening.lineTo(TUNNEL_R, OP_LEN / 2)
      break
    case 'top':
      opening.moveTo(-OP_LEN / 2, -TUNNEL_R)
      opening.lineTo(OP_LEN / 2, -TUNNEL_R)
      break
    case 'bottom':
      opening.moveTo(-OP_LEN / 2, TUNNEL_R)
      opening.lineTo(OP_LEN / 2, TUNNEL_R)
      break
  }
  opening.stroke({ width: 1.5, color: 0xffffff, alpha: 1 })
  root.addChild(opening)

  // Port position on the specified side.
  const port: Port = (() => {
    switch (side) {
      case 'left':
        return { x: center.x - TUNNEL_R, y: center.y, dir: { x: -1, y: 0 } }
      case 'right':
        return { x: center.x + TUNNEL_R, y: center.y, dir: { x: 1, y: 0 } }
      case 'top':
        return { x: center.x, y: center.y - TUNNEL_R, dir: { x: 0, y: -1 } }
      case 'bottom':
        return { x: center.x, y: center.y + TUNNEL_R, dir: { x: 0, y: 1 } }
    }
  })()

  return {
    kind: role === 'entrance' ? 'tunnel_entrance' : 'tunnel_exit',
    center,
    leftPort: side === 'left' ? port : undefined,
    rightPort: side === 'right' ? port : undefined,
    topPort: side === 'top' ? port : undefined,
    bottomPort: side === 'bottom' ? port : undefined,
    update() {},
  }
}

export function buildPole(
  parent: Container,
  center: { x: number; y: number },
  connection?: { in: Side; out: Side },
): PoleHandle {
  // Poles are pass-through waypoints that let belts bend at grid corners
  // without loading additional semantics onto stations. Visually they're
  // a small disc sitting on a continuous belt — the incoming belt stops
  // at the pole's entry port, a connector belt crosses the interior, and
  // the outgoing belt starts at the exit port. Without the connector
  // there's a 2*POLE_R gap in the conveyor material; with it the belt
  // reads as unbroken through the pole, with the disc as a subtle accent.
  const root = new Container()
  root.x = center.x
  root.y = center.y
  parent.addChild(root)

  // Connector belt — drawn first so the pole disc lays on top. When both
  // sides are known the connector fully stitches the two adjacent belts
  // together, so the disc becomes redundant visual noise on top of the
  // seamless belt material. Drop it entirely in the connected case.
  if (connection) {
    drawPoleConnector(root, connection.in, connection.out)
  } else {
    const shadow = new Graphics()
    shadow.circle(0, 1.5, POLE_R)
    shadow.fill({ color: 0x000000, alpha: 0.08 })
    root.addChild(shadow)

    const outer = new Graphics()
    outer.circle(0, 0, POLE_R)
    outer.fill({ color: 0xffffff, alpha: 0.95 })
    outer.stroke({ width: 0.75, color: ACCENT, alpha: 0.55 })
    root.addChild(outer)

    const inner = new Graphics()
    inner.circle(0, 0, 2)
    inner.fill({ color: ACCENT, alpha: 0.65 })
    root.addChild(inner)
  }

  return {
    kind: 'pole',
    center,
    leftPort: { x: center.x - POLE_R, y: center.y, dir: { x: -1, y: 0 } },
    rightPort: { x: center.x + POLE_R, y: center.y, dir: { x: 1, y: 0 } },
    topPort: { x: center.x, y: center.y - POLE_R, dir: { x: 0, y: -1 } },
    bottomPort: { x: center.x, y: center.y + POLE_R, dir: { x: 0, y: 1 } },
    update() {},
  }
}

// drawPoleConnector renders a short belt segment that crosses the pole's
// interior from its input port to its output port. Opposite sides produce
// a straight segment; adjacent sides produce a 90° arc via a quadratic
// bezier with control point at the pole center — the tangents at the
// endpoints are exactly the port axis directions, so the connector meets
// the incoming/outgoing belts seamlessly without a visible kink.
//
// The 4-layer belt recipe (white body + tint + top highlight + bottom
// shadow) mirrors buildBelt in scene.ts so the connector is visually
// indistinguishable from the belts it joins.
function drawPoleConnector(parent: Container, inSide: Side, outSide: Side) {
  const inPos = portOffset(inSide)
  const outPos = portOffset(outSide)
  const N = 16
  type Sample = { x: number; y: number; tx: number; ty: number }
  const samples: Sample[] = []

  if (isOppositeSide(inSide, outSide)) {
    // Linear interpolation — tangent is constant along the segment.
    const tx = outPos.x - inPos.x
    const ty = outPos.y - inPos.y
    for (let i = 0; i <= N; i++) {
      const t = i / N
      samples.push({ x: inPos.x + tx * t, y: inPos.y + ty * t, tx, ty })
    }
  } else {
    // Quadratic bezier P0 → P1 → P2 with control point at origin (pole
    // center). Tangent at P0 = 2*(P1 - P0) ∝ port-in axis; tangent at
    // P2 = 2*(P2 - P1) ∝ port-out axis. Tight 90° arc through the pole.
    const P0 = inPos
    const P1 = { x: 0, y: 0 }
    const P2 = outPos
    for (let i = 0; i <= N; i++) {
      const t = i / N
      const u = 1 - t
      samples.push({
        x: u * u * P0.x + 2 * u * t * P1.x + t * t * P2.x,
        y: u * u * P0.y + 2 * u * t * P1.y + t * t * P2.y,
        tx: 2 * u * (P1.x - P0.x) + 2 * t * (P2.x - P1.x),
        ty: 2 * u * (P1.y - P0.y) + 2 * t * (P2.y - P1.y),
      })
    }
  }

  // Perpendicular-offset edge points for belt width.
  const hw = BELT_WIDTH / 2
  const nearPts: Array<{ x: number; y: number }> = []
  const farPts: Array<{ x: number; y: number }> = []
  for (const s of samples) {
    const tlen = Math.hypot(s.tx, s.ty) || 1
    const nx = s.ty / tlen
    const ny = -s.tx / tlen
    nearPts.push({ x: s.x + nx * hw, y: s.y + ny * hw })
    farPts.push({ x: s.x - nx * hw, y: s.y - ny * hw })
  }

  const body = new Graphics()
  body.moveTo(nearPts[0].x, nearPts[0].y)
  for (let i = 1; i <= N; i++) body.lineTo(nearPts[i].x, nearPts[i].y)
  for (let i = N; i >= 0; i--) body.lineTo(farPts[i].x, farPts[i].y)
  body.closePath()
  body.fill({ color: 0xffffff, alpha: 0.82 })
  parent.addChild(body)

  const tint = new Graphics()
  tint.moveTo(nearPts[0].x, nearPts[0].y)
  for (let i = 1; i <= N; i++) tint.lineTo(nearPts[i].x, nearPts[i].y)
  for (let i = N; i >= 0; i--) tint.lineTo(farPts[i].x, farPts[i].y)
  tint.closePath()
  tint.fill({ color: ACCENT, alpha: 0.1 })
  parent.addChild(tint)

  // Edge strokes are trimmed at both ends — the adjacent belts already
  // draw their own edges up to each port, and exact-pixel overlap at the
  // port position produces a darker doubled line where the strokes stack
  // (visible especially on the outer side of a 90° turn). Starting a few
  // samples in avoids the overlap while the body/tint polygons still fill
  // the full length, so there's no visible gap in the belt material.
  const EDGE_TRIM = 2
  const edgeStart = EDGE_TRIM
  const edgeEnd = N - EDGE_TRIM

  const topEdge = new Graphics()
  topEdge.moveTo(nearPts[edgeStart].x, nearPts[edgeStart].y)
  for (let i = edgeStart + 1; i <= edgeEnd; i++) topEdge.lineTo(nearPts[i].x, nearPts[i].y)
  topEdge.stroke({ width: 1.25, color: 0xffffff, alpha: 0.95 })
  parent.addChild(topEdge)

  const botEdge = new Graphics()
  botEdge.moveTo(farPts[edgeStart].x, farPts[edgeStart].y)
  for (let i = edgeStart + 1; i <= edgeEnd; i++) botEdge.lineTo(farPts[i].x, farPts[i].y)
  botEdge.stroke({ width: 1.25, color: 0x000000, alpha: 0.18 })
  parent.addChild(botEdge)
}

function portOffset(side: Side): { x: number; y: number } {
  switch (side) {
    case 'left':
      return { x: -POLE_R, y: 0 }
    case 'right':
      return { x: POLE_R, y: 0 }
    case 'top':
      return { x: 0, y: -POLE_R }
    case 'bottom':
      return { x: 0, y: POLE_R }
  }
}

function isOppositeSide(a: Side, b: Side): boolean {
  return (
    (a === 'left' && b === 'right') ||
    (a === 'right' && b === 'left') ||
    (a === 'top' && b === 'bottom') ||
    (a === 'bottom' && b === 'top')
  )
}
