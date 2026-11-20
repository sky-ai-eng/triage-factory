// Routing table for chip animations over the factory's PathSegment
// graph. Built once at scene construction; queried when a WS event
// announces an entity transition (event_type A → B) so the spawn
// pipeline can hand the simulator a precomputed itinerary instead of
// blindly following segment.next[0] at every fork.
//
// BFS from each station's exit segment finds the shortest segment
// chain to every other station's entry segment. Mid-walk stations
// are passed through (their entry is recorded as a destination, but
// BFS continues through their exit). Cycles are handled by a per-
// source visited set.

import type { PathSegment } from './iso-path'

export interface StationEndpoints {
  /** Stable station id — the event_type for the GitHub PR factory
   *  (e.g., "github:pr:opened"). */
  id: string
  /** Segment chips ride FROM when this station is the source — the
   *  east-output port's segment in the current factory. */
  exit: PathSegment
  /** Segment chips arrive AT when this station is the destination —
   *  the west-input port's segment. The chip rides to the end of
   *  this segment and is then disposed by the simulator (visually:
   *  it disappears into the station's recess). */
  entry: PathSegment
}

export interface RoutingTable {
  /** Itinerary of PathSegments for chips traveling from fromId to
   *  toId, or null if no path exists. The first element is the
   *  source station's exit segment; the last is the destination
   *  station's entry segment. */
  getItinerary(fromId: string, toId: string): PathSegment[] | null
  /** Animation duration in ms for a chip traveling from fromId to
   *  toId at the configured belt speed. Falls back to the supplied
   *  default when no path exists — the floor reconciler still
   *  needs a number so unknown pairs eventually snap to parked. */
  getDuration(fromId: string, toId: string): number
}

/** Build a routing table from the station endpoint set.
 *
 *  `worldSpeed` is in world-units per second; chip animation duration
 *  per pair is derived as (sum of segment lengths) / worldSpeed so
 *  chip travel time matches the visual length of the bridges. */
export function buildRoutingTable(
  stations: StationEndpoints[],
  worldSpeed: number,
  /** Returned by `getDuration` for pairs with no path; placeEntity
   *  uses this so chips spawned on those (rare) transitions still
   *  fall through to "parked" within a reasonable window. */
  fallbackDurationMs: number = 2000,
): RoutingTable {
  const entryToId = new Map<PathSegment, string>()
  for (const s of stations) entryToId.set(s.entry, s.id)

  // Key: `${fromId}\0${toId}` — null byte avoids any collision with
  // colon-bearing event_type ids like "github:pr:opened".
  const table = new Map<string, PathSegment[]>()

  for (const from of stations) {
    const visited = new Set<PathSegment>()
    visited.add(from.exit)
    const queue: { seg: PathSegment; path: PathSegment[] }[] = [
      { seg: from.exit, path: [from.exit] },
    ]
    while (queue.length > 0) {
      const { seg, path } = queue.shift()!
      const toId = entryToId.get(seg)
      if (toId && toId !== from.id) {
        const key = `${from.id}\0${toId}`
        if (!table.has(key)) {
          table.set(key, path.slice())
        }
      }
      for (const next of seg.next) {
        if (!visited.has(next)) {
          visited.add(next)
          queue.push({ seg: next, path: [...path, next] })
        }
      }
    }
  }

  // One-shot warning surface for unreachable pairs. The pipeline
  // falls back to teleport (drawer count update only) on these, so
  // it's not fatal — just useful during topology iteration.
  const unreachable: string[] = []
  for (const a of stations) {
    for (const b of stations) {
      if (a.id === b.id) continue
      if (!table.has(`${a.id}\0${b.id}`)) {
        unreachable.push(`${a.id} → ${b.id}`)
      }
    }
  }
  if (unreachable.length > 0) {
    console.info(
      `[iso-routing] ${unreachable.length} unreachable station pair(s) — chips will teleport for these transitions:\n  ${unreachable.join('\n  ')}`,
    )
  }

  // Precompute per-pair durations from the segment lengths summed
  // along each itinerary. Constant work done once at scene boot.
  const durations = new Map<string, number>()
  for (const [key, path] of table) {
    const totalLen = path.reduce((acc, seg) => acc + seg.length, 0)
    durations.set(key, (totalLen / worldSpeed) * 1000)
  }

  return {
    getItinerary(fromId, toId) {
      if (fromId === toId) return null
      return table.get(`${fromId}\0${toId}`) ?? null
    },
    getDuration(fromId, toId) {
      if (fromId === toId) return fallbackDurationMs
      return durations.get(`${fromId}\0${toId}`) ?? fallbackDurationMs
    },
  }
}
