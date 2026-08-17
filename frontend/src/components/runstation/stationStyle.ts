import type { Conversation } from '../../types'
import { chainPosition, completionKind, isActiveConversation } from '../../lib/conversationStatus'
import { hasUnresolvedArtifacts } from '../../lib/approval'

// stationStyle — the design system for the run-station HMI. It maps a run's
// status to the single "light" the whole machine wears, plus the motion knobs
// (scanner / vent heat / belt speed) that make a live run feel like a station on
// the factory line. Pure module (no JSX) so the presentational components in
// runstation/* import it freely and Fast Refresh stays happy.
//
// The machine is lit by state, not chrome — the board's grammar pulled onto a
// full screen. A baseline cyan "powered" trim is always on; a dominant
// state-colored emissive frame rides over it: violet working, cyan idle/open,
// amber your-move, green done, red failed. Everything is flat. Depth is never
// implied (that's the whole reason this isn't a 3D scene).

export type StationKey =
  | 'queued'
  | 'working'
  | 'open'
  | 'attention'
  | 'done'
  | 'handoff'
  | 'stopped'
  | 'failed'

export interface StationState {
  key: StationKey
  /** CSS color for the emissive state frame/glow (a theme var or HMI var). */
  light: string
  /** Uppercase status word shown in the label plate. */
  label: string
  /** A live run breathes, scans, runs hot, and moves the belt. */
  live: boolean
  /** The scanner sweep runs only while a turn is actively executing. */
  scanner: boolean
  /** 0..1 vent-heat intensity (the machine running hot). */
  heat: number
  /** 0..1 conveyor belt speed (ambient transit; 0 = parked). */
  belt: number
}

// The baseline machine trim — a powered cyan, the Her/Transcendence accent
// light from the factory's LED perimeter, toned for the warm field.
export const HMI_CYAN = 'var(--hmi-cyan)'

// stationState collapses the run lifecycle into the machine's lit state. Every
// active status — each setup phase and running alike — reads as one thing: the
// machine is hot and scanning.
export function stationState(conversation: Conversation): StationState {
  if (isActiveConversation(conversation)) {
    return {
      key: 'working',
      light: 'var(--color-delegate)',
      label: 'WORKING',
      live: true,
      scanner: true,
      heat: 0.9,
      belt: 1,
    }
  }
  // Approval is derived, not a stored status (TFAC-382/TFAC-492): a settled run
  // (idle / terminal) that still holds unresolved draft PRs or ready reviews
  // wears the amber "your move" light, regardless of its run status. A live run
  // keeps WORKING (handled above) — the dock surfaces the approval affordance
  // without recoloring the whole machine mid-turn.
  if (hasUnresolvedArtifacts(conversation)) {
    return {
      key: 'attention',
      light: 'var(--color-snooze)',
      label: 'YOUR MOVE',
      live: false,
      scanner: false,
      heat: 0.36,
      belt: 0.16,
    }
  }
  switch (conversation.Status) {
    case 'open':
      // Honest idle: not executing, not concluded. Powered, waiting — the
      // machine glows its baseline cyan, belt idling, vents barely warm.
      return {
        key: 'open',
        light: HMI_CYAN,
        label: 'IDLE',
        live: false,
        scanner: false,
        heat: 0.22,
        belt: 0.34,
      }
    case 'queued':
      return {
        key: 'queued',
        light: 'var(--color-snooze)',
        label: 'QUEUED',
        live: false,
        scanner: false,
        heat: 0.3,
        belt: 0.5,
      }
    case 'completed': {
      // One stored terminal, three endings — see completionKind. The machine
      // wears a different light for each, because "this step is done" and "the
      // task is done" are not the same news, and the green DONE plate was
      // telling a viewer mid-chain that the whole thing had stopped.
      switch (completionKind(conversation)) {
        case 'handoff': {
          // Cyan, not green: the powered-and-waiting trim. This step concluded
          // and the line moved on, so the belt keeps idling — green here reads
          // as "nothing follows", which is exactly the wrong conclusion. The
          // position rides the label so the plate itself says where you are.
          const pos = chainPosition(conversation)
          return {
            key: 'handoff',
            light: HMI_CYAN,
            label: pos ? `STEP ${pos.step}/${pos.total} DONE` : 'STEP DONE',
            live: false,
            scanner: false,
            heat: 0.12,
            belt: 0.34,
          }
        }
        case 'stopped':
          // An abort is a completed row, but the agent gave up and the task is
          // still open — that is a your-move amber, never a success green.
          return {
            key: 'stopped',
            light: 'var(--color-snooze)',
            label: 'STOPPED',
            live: false,
            scanner: false,
            heat: 0.12,
            belt: 0,
          }
        default:
          return {
            key: 'done',
            light: 'var(--color-claim)',
            label: 'DONE',
            live: false,
            scanner: false,
            heat: 0.12,
            belt: 0,
          }
      }
    }
    case 'failed':
      return {
        key: 'failed',
        light: 'var(--color-dismiss)',
        label: 'FAILED',
        live: false,
        scanner: false,
        heat: 0,
        belt: 0,
      }
    default:
      return {
        key: 'open',
        light: HMI_CYAN,
        label: conversation.Status.toUpperCase(),
        live: false,
        scanner: false,
        heat: 0.2,
        belt: 0.2,
      }
  }
}

// tint(color, pct) — a translucent wash of an emissive color, matching the
// board's color-mix idiom so the state light reads as glow, not fill.
export function tint(color: string, pct: number): string {
  return `color-mix(in srgb, ${color} ${pct}%, transparent)`
}

// liveHeat — vent-heat 0..1 for a live run: idles at the state's base heat and
// flares toward 1 when the agent just emitted (a tool call / message in the last
// few seconds), decaying over ~6s. Reads as the machine pulsing hot as work
// flows through it.
export function liveHeat(base: number, lastMessageAt: number | null, now: number): number {
  if (!lastMessageAt) return base
  const since = (now - lastMessageAt) / 1000
  if (since < 0) return base
  const flare = Math.max(0, 1 - since / 6) * (1 - base)
  return Math.min(1, base + flare)
}

// compactNum — 1.2k / 3.4M for the gauge readouts (M-aware; cardStyle's is k-only).
export function compactNum(n: number): string {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1).replace(/\.0$/, '') + 'M'
  if (n >= 1000) return (n / 1000).toFixed(1).replace(/\.0$/, '') + 'k'
  return String(n)
}
