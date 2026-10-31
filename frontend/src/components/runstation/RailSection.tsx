// RailSection — one labelled block of the Run page's telemetry rail: a small
// uppercase label ruled out to the rail's edge, then its readouts.
export function RailSection({
  label,
  children,
  last,
}: {
  label: string
  children: React.ReactNode
  last?: boolean
}) {
  return (
    <div className={last ? '' : 'mb-5'}>
      <div className="mb-2 flex items-center gap-2">
        <span className="font-mono text-label-sm font-semibold uppercase tracking-[0.18em] text-ink-3/70">
          {label}
        </span>
        <span className="h-px flex-1 bg-line-1" />
      </div>
      <div className="space-y-1.5">{children}</div>
    </div>
  )
}
