// timeAgo renders an RFC3339 / ISO timestamp as a compact "x ago" string
// ("just now", "5m ago", "3h ago", "2d ago", "3w ago", "4mo ago", "2y ago").
// Used by the pending-invite ghost rows for "invited {relative time}". Kept
// dependency-free (no date library in the project) and exported so tests can
// pin the buckets directly.
//
// `now` is injectable so callers/tests get deterministic output; it defaults
// to the wall clock. A future timestamp (clock skew) collapses to "just now"
// rather than rendering a nonsensical negative.
export function timeAgo(iso: string, now: number = Date.now()): string {
  const then = new Date(iso).getTime()
  if (Number.isNaN(then)) return ''
  const secs = Math.floor((now - then) / 1000)
  if (secs < 45) return 'just now'

  const mins = Math.floor(secs / 60)
  if (mins < 60) return `${mins}m ago`
  const hours = Math.floor(mins / 60)
  if (hours < 24) return `${hours}h ago`
  const days = Math.floor(hours / 24)
  if (days < 7) return `${days}d ago`
  const weeks = Math.floor(days / 7)
  if (weeks < 5) return `${weeks}w ago`
  const months = Math.floor(days / 30)
  if (months < 12) return `${months}mo ago`
  const years = Math.floor(days / 365)
  return `${years}y ago`
}
