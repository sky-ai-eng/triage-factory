/** Snooze presets and their resolution to a wake instant.
 *
 *  `PATCH /api/tasks/{id}` takes `snooze_until` as an RFC3339 timestamp and
 *  nothing else, so the preset vocabulary lives here rather than on the
 *  server. That is where it belongs: "tomorrow" means the user's own next
 *  morning, and a server resolving the word would resolve it against its own
 *  clock and zone — which for a multi-tenant deployment is nobody's morning. */

export type SnoozePreset = '1h' | '2h' | '4h' | 'tomorrow'

/** Resolves a preset to the RFC3339 instant the task wakes.
 *
 *  The relative presets are offsets from `now`. `tomorrow` is 9am the
 *  following day in the user's own timezone — a wall-clock choice, not an
 *  offset — which `toISOString` then converts to the UTC instant the API
 *  takes. */
export function snoozeUntilFromPreset(preset: SnoozePreset, now: Date = new Date()): string {
  if (preset === 'tomorrow') {
    const wake = new Date(now)
    wake.setDate(wake.getDate() + 1)
    wake.setHours(9, 0, 0, 0)
    return wake.toISOString()
  }
  const hours = { '1h': 1, '2h': 2, '4h': 4 }[preset]
  return new Date(now.getTime() + hours * 60 * 60 * 1000).toISOString()
}
