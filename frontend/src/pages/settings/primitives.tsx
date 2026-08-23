/* eslint-disable react-refresh/only-export-components --
   Stable shared plumbing: the Section/Field primitives sit next to the
   inputClass / POLL_INTERVAL_OPTIONS constants they're always used with.
   The HMR boundary trade-off is acceptable for this leaf module. */
// Shared presentation primitives for the org/team settings field groups.
// Extracted so the same field-group components render identically in the
// Settings page, the org-create configure step, and the local Setup
// wizard — one chrome, no per-surface drift.

export const inputClass =
  'w-full bg-raised border border-line-1 rounded-xl px-4 py-2.5 text-body text-ink-1 placeholder-ink-3 focus:outline-none focus:ring-2 focus:ring-warm/30 focus:border-warm/40 transition-colors disabled:opacity-60 disabled:cursor-not-allowed'

// glassInputClass is the flush, frosted input used by the setup wizard (the URL
// steps, and the field groups when composed in `bare` mode) — a translucent
// surface-overlay fill over a glass edge, with a soft accent focus ring. Lives
// here next to inputClass so both the settings groups and the setup flow read
// the same field material without a cross-package import.
export const glassInputClass =
  'w-full rounded-2xl border border-[var(--color-line-1)] bg-[var(--color-raised)]/60 px-4 py-3 text-body text-ink-1 placeholder:text-ink-3 backdrop-blur-md outline-none transition-[border-color,background-color,box-shadow] focus:border-warm/40 focus:bg-[var(--color-raised)] focus:shadow-[0_0_0_4px_var(--color-warm-2)] disabled:opacity-60 disabled:cursor-not-allowed'

// Segmented toggle styling (SSH/HTTPS, the App's Personal/Org switch), with a
// flush glass variant for the setup wizard's `bare` field groups: a glass track
// and an accent-tinted selected segment. The carded default keeps the white
// chip. Kept here so the same control reads identically wherever it's composed.
export const segmentedWrap = (bare = false) =>
  bare
    ? 'inline-flex rounded-xl border border-[var(--color-line-1)] bg-[var(--color-raised)]/50 p-0.5 backdrop-blur-md'
    : 'inline-flex rounded-lg border border-line-1 bg-tint-2 p-0.5'

export const segmentedBtn = (selected: boolean, bare = false) =>
  `rounded-md px-3 py-1 text-ui font-medium transition-colors ${
    selected
      ? bare
        ? 'bg-warm/[0.12] text-warm shadow-float'
        : 'bg-raised text-ink-1 shadow-float'
      : 'text-ink-3 hover:text-ink-2'
  }`

// Poll-interval options shared by the GitHub + Jira timing selects. The
// values are Go duration strings (the wire format org_settings round-trips).
export const POLL_INTERVAL_OPTIONS: { value: string; label: string }[] = [
  { value: '30s', label: '30 seconds' },
  { value: '1m0s', label: '1 minute' },
  { value: '2m0s', label: '2 minutes' },
  { value: '5m0s', label: '5 minutes' },
]

export function Section({ children, danger }: { children: React.ReactNode; danger?: boolean }) {
  return (
    <section
      className={`backdrop-blur-xl bg-raised border rounded-2xl p-6 shadow-float shadow-black/[0.03] ${
        danger ? 'border-alarm/15' : 'border-line-1'
      }`}
    >
      {children}
    </section>
  )
}

export function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <label className="block">
      <span className="text-reported text-ink-3 mb-1.5 block">{label}</span>
      {children}
    </label>
  )
}
