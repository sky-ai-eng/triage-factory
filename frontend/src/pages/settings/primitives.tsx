/* eslint-disable react-refresh/only-export-components --
   Stable shared plumbing: the Section/Field primitives sit next to the
   inputClass / POLL_INTERVAL_OPTIONS constants they're always used with.
   The HMR boundary trade-off is acceptable for this leaf module. */
// Shared presentation primitives for the org/team settings field groups.
// Extracted so the same field-group components render identically in the
// Settings page, the org-create configure step, and the local Setup
// wizard — one chrome, no per-surface drift.

export const inputClass =
  'w-full bg-white/50 border border-border-subtle rounded-xl px-4 py-2.5 text-[13px] text-text-primary placeholder-text-tertiary focus:outline-none focus:ring-2 focus:ring-accent/30 focus:border-accent/40 transition-colors disabled:opacity-60 disabled:cursor-not-allowed'

// glassInputClass is the flush, frosted input used by the setup wizard (the URL
// steps, and the field groups when composed in `bare` mode) — a translucent
// surface-overlay fill over a glass edge, with a soft accent focus ring. Lives
// here next to inputClass so both the settings groups and the setup flow read
// the same field material without a cross-package import.
export const glassInputClass =
  'w-full rounded-2xl border border-[var(--color-border-glass)] bg-[var(--color-surface-overlay)]/60 px-4 py-3 text-[14px] text-text-primary placeholder:text-text-tertiary backdrop-blur-md outline-none transition-[border-color,background-color,box-shadow] focus:border-accent/40 focus:bg-[var(--color-surface-overlay)] focus:shadow-[0_0_0_4px_var(--color-accent-soft)] disabled:opacity-60 disabled:cursor-not-allowed'

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
      className={`backdrop-blur-xl bg-surface-raised border rounded-2xl p-6 shadow-sm shadow-black/[0.03] ${
        danger ? 'border-dismiss/15' : 'border-border-glass'
      }`}
    >
      {children}
    </section>
  )
}

export function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <label className="block">
      <span className="text-[11px] text-text-tertiary mb-1.5 block">{label}</span>
      {children}
    </label>
  )
}
