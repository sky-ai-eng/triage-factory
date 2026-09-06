import type { ApiToken } from '../../types'

// The arithmetic behind the token table, the sheet and the create dialog —
// kept pure so the day-math the ticket specifies is pinned by unit tests
// rather than read off a screenshot.

export const DAY = 86_400_000

/** Whole days from `now` to `iso`, rounded up: a token expiring in 25 hours
 *  reads "in 2d", one expiring in 30 minutes "in 1d", one past "expired". */
export function daysUntil(iso: string, now: number): number {
  // + 0 folds Math.ceil's -0 (a past instant within the day) into 0.
  return Math.ceil((new Date(iso).getTime() - now) / DAY) + 0
}

export type Expiry = {
  /** 'in 24d' | 'never' | 'expired' */
  text: string
  /** Infinity for never, ≤ 0 for expired. */
  days: number
  /** True when the org's cap, not the minter's date, is what decides. */
  orgLimit: boolean
}

/** The table's EXPIRES cell and the head's "next expiry", from the effective
 *  date the server already folded the cap into. */
export function expiry(
  t: Pick<ApiToken, 'expires_at' | 'effective_expires_at'>,
  now: number,
): Expiry {
  const eff = t.effective_expires_at
  if (eff == null) return { text: 'never', days: Infinity, orgLimit: false }
  const days = daysUntil(eff, now)
  const stored = t.expires_at
  const orgLimit = stored == null || new Date(eff).getTime() < new Date(stored).getTime()
  return { text: days <= 0 ? 'expired' : 'in ' + days + 'd', days, orgLimit }
}

/** `d MMM`, with the year when it is not this year. */
export function shortDate(iso: string | number, now: number): string {
  const d = new Date(iso)
  const o: Intl.DateTimeFormatOptions = { day: 'numeric', month: 'short' }
  if (d.getFullYear() !== new Date(now).getFullYear()) o.year = 'numeric'
  return d.toLocaleDateString('en-GB', o)
}

/** Minutes since `iso`, floored at 0, for the table's `ago` cell. */
export function minutesSince(iso: string, now: number): number {
  return Math.max(0, Math.floor((now - new Date(iso).getTime()) / 60_000))
}

/** A v4 or v6 CIDR range, as the middleware accepts one — fast feedback only;
 *  the server's canonicalizer is the enforcement. */
export function validCidr(s: string): boolean {
  const m4 = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})\/(\d{1,2})$/.exec(s)
  if (m4) return m4.slice(1, 5).every((n) => +n <= 255) && +m4[5] <= 32
  const m6 = /^([0-9a-f]{0,4}:){2,7}[0-9a-f]{0,4}\/(\d{1,3})$/i.exec(s)
  return !!m6 && +m6[2] <= 128
}

export const MAX_CIDRS = 20

export type Preset = { id: string; label: string; days: number | null }

/** The create dialog's expiry chips. `days` is Infinity for never, null for
 *  the custom date. Presets are display math: the request carries an
 *  absolute date or omits it. */
export const PRESETS: Preset[] = [
  { id: '7', label: '7 days', days: 7 },
  { id: '30', label: '30 days', days: 30 },
  { id: '60', label: '60 days', days: 60 },
  { id: '90', label: '90 days', days: 90 },
  { id: 'custom', label: 'a date', days: null },
  { id: 'never', label: 'never', days: Infinity },
]

/** Whether a preset is past the org's cap — struck, not hidden. */
export function presetOff(p: Preset, cap: number | null): boolean {
  return !!cap && (p.days === Infinity || (p.days != null && p.days > cap))
}

/** The absolute expiry a pick resolves to: an RFC3339 instant, `null` for
 *  never, or `undefined` when a custom date has not been picked yet. A day
 *  preset is `now + N days`; a custom date is midnight local at its start. */
export function expiresAtFor(pick: string, custom: string, now: number): string | null | undefined {
  if (pick === 'never') return null
  if (pick === 'custom') {
    if (!custom) return undefined
    return new Date(custom + 'T00:00:00').toISOString()
  }
  return new Date(now + Number(pick) * DAY).toISOString()
}

/** The LOCAL calendar date (yyyy-mm-dd) `n` days out, for a date input's
 *  bounds — the same calendar `expiresAtFor` reads a picked date in, so the
 *  bound and the pick never disagree by a day across the UTC line. */
export function isoDate(now: number, n: number): string {
  const d = new Date(now + n * DAY)
  const mm = String(d.getMonth() + 1).padStart(2, '0')
  const dd = String(d.getDate()).padStart(2, '0')
  return d.getFullYear() + '-' + mm + '-' + dd
}
