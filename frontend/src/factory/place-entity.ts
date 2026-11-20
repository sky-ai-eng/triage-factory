// Read-authoritative entity placement.
//
// Single projection function used by both station tray counts AND
// chip positions on the floor: given an entity from the floor read,
// the set of station-mapped event_types, a per-pair animation
// duration source, and the current wall clock, returns whether the
// entity is parked at a station or in transit between two stations
// (with progress in [0,1]).
//
// Tray counts and chip placement read this same function so the two
// can never drift. Reload-stable rendering falls out for free —
// `recent_events[]` (with both source-time `at` and insert-time
// `detected_at`) on a possibly-moving entity encodes exactly enough
// state to resume mid-animation after a refresh.
//
// ─── Chain handling ────────────────────────────────────────────────
//
// When a single poll cycle detects multiple events for the same
// entity (`new_commits` then `ci_check_passed` is the canonical
// pair), the chip should animate through the FULL chain rather than
// teleporting to the final hop. We detect the chain by clustering
// the tail of `recent_events` by `detected_at` gap — events from one
// poll insert within milliseconds; pollers run every 30s+, so any
// small threshold (we use 2s) cleanly separates bursts.
//
// Within the chain, hops play sequentially anchored at the chain's
// first detection time:
//
//   anchor    = chain[0].detected_at
//   hop K     = (chain[K-1] → chain[K])  for K in [1..chain.len-1]
//   hop 0     = (source → chain[0])      where source is the
//               station-mapped event immediately *before* the chain
//               in `recent_events` (the entity's prior position)
//   elapsed   = now - anchor
//   bucket K  = sum of durations(hop[0..K-1]) ≤ elapsed < bucket K+1
//
// Past the total chain duration → parked at chain[last]. Pre-anchor
// (clock skew, future timestamps) → parked at the source. Single-
// event chains reduce to today's behavior automatically.

import type { FactoryEntity } from '../types'

// Detection gap above which two adjacent events are considered to
// belong to *different* chains. Pollers separate runs by ~30s; events
// in one poll insert within ~ms. 2s is a comfortable threshold.
const CHAIN_DETECTION_GAP_MS = 2000

export type EntityPlacement =
  | { kind: 'parked'; station: string }
  | { kind: 'transit'; from: string; to: string; progress: number }

/** Project an entity to its current factory position.
 *
 *  Returns null when the entity has no station-mapped event to anchor
 *  on (e.g., a pull request whose only events are system ones) so the
 *  caller can filter it out cleanly. */
export function placeEntity(
  entity: FactoryEntity,
  knownStations: Set<string>,
  /** ms duration for the from→to chip ride. Backed by the routing
   *  table (sum of segment lengths / belt speed) so chip travel time
   *  matches the visual length of the bridges. */
  getDuration: (from: string, to: string) => number,
  now: number,
): EntityPlacement | null {
  // Filter to station-mapped events only, preserving order. Non-
  // station events (system pollers, ignored kinds) shouldn't appear
  // in the chain or as the source — they don't move the chip.
  const station = (entity.recent_events ?? []).filter((e) => knownStations.has(e.event_type))
  if (station.length === 0) {
    if (entity.latest_event_type && knownStations.has(entity.latest_event_type)) {
      // The entity's latest event has a station and no history was read
      // for it (not possibly mid-ride), or none came back. Park there
      // with no animation.
      return { kind: 'parked', station: entity.latest_event_type }
    }
    return null
  }

  // Cluster the tail by detected_at gap. Walk backwards from the
  // newest entry; include each step in the chain while its predecessor
  // is within the threshold. The first index that fails the test (or
  // index 0 if we walk all the way back) is `chainStart`.
  let chainStart = station.length - 1
  while (chainStart > 0) {
    const cur = parseTimestamp(station[chainStart].detected_at)
    const prev = parseTimestamp(station[chainStart - 1].detected_at)
    if (cur == null || prev == null) break
    if (cur - prev > CHAIN_DETECTION_GAP_MS) break
    chainStart--
  }

  const chain = station.slice(chainStart)
  // Anchor: when this chain was discovered. For a single-event chain,
  // that's the event itself. For a multi-event chain, the earliest
  // detection in the burst (typically all within a few ms of each
  // other anyway).
  const anchorMs = parseTimestamp(chain[0].detected_at)
  if (anchorMs == null) {
    // Missing detection timestamp — fall back to parked at the chain's
    // last station. Shouldn't happen with the new backend, but the
    // contract is "render something reasonable" not "blow up."
    return { kind: 'parked', station: chain[chain.length - 1].event_type }
  }

  // Source = station-mapped event immediately before the chain.
  // Determines the *first* hop (source → chain[0]). When absent
  // (chain begins at index 0 of station-mapped history), the chip
  // has no prior known location to fly in from, so the first hop is
  // dropped and animation starts at chain[0] → chain[1].
  const source = chainStart > 0 ? station[chainStart - 1].event_type : null

  // Build hop list. Self-loop hops (same from and to, e.g., a
  // duplicate event somehow squeezed past dedup) are skipped — the
  // chip would have nothing to traverse and `progress / dur` would
  // be undefined.
  const hops: { from: string; to: string; dur: number }[] = []
  if (source && source !== chain[0].event_type) {
    hops.push({
      from: source,
      to: chain[0].event_type,
      dur: getDuration(source, chain[0].event_type),
    })
  }
  for (let i = 1; i < chain.length; i++) {
    const from = chain[i - 1].event_type
    const to = chain[i].event_type
    if (from === to) continue
    hops.push({ from, to, dur: getDuration(from, to) })
  }

  // Degenerate: nothing to animate (single-event chain with no source,
  // or an all-self-loop chain). Park at the chain's last station.
  if (hops.length === 0) {
    return { kind: 'parked', station: chain[chain.length - 1].event_type }
  }

  const elapsed = now - anchorMs
  if (elapsed < 0) {
    // Future timestamp / clock skew. Park at the source if we have
    // one (chip "hasn't moved yet"), otherwise at chain[0].
    return { kind: 'parked', station: source ?? chain[0].event_type }
  }

  // Time-slice. Find which hop the chip is in by walking the hop
  // list and accumulating durations.
  let acc = 0
  for (const hop of hops) {
    if (hop.dur <= 0) continue // routing-table fallback shouldn't bite, but guard
    if (elapsed < acc + hop.dur) {
      return {
        kind: 'transit',
        from: hop.from,
        to: hop.to,
        progress: (elapsed - acc) / hop.dur,
      }
    }
    acc += hop.dur
  }

  // Past the end of the chain — parked at the destination.
  return { kind: 'parked', station: chain[chain.length - 1].event_type }
}

function parseTimestamp(s: string | undefined): number | null {
  if (!s) return null
  const t = Date.parse(s)
  return Number.isNaN(t) ? null : t
}
