import { Eye } from 'lucide-react'

// ViewOnlyBadge is the non-hostile "you're a viewer here" indicator (TFAC-447).
// It renders on team-scoped surfaces where mutation affordances have been hidden
// for a read-only (viewer) member, so the absence of buttons reads as a role
// boundary rather than a broken page. Quiet by design — a muted chip, not an
// error banner: a viewer's read-only view is a legitimate, expected state.
export default function ViewOnlyBadge({ className = '' }: { className?: string }) {
  return (
    <span
      title="You have view-only access to this team. Ask a team admin for write access to make changes."
      className={`inline-flex shrink-0 items-center gap-1.5 rounded-full border border-border-subtle bg-surface-raised px-2.5 py-1 text-[11px] font-medium text-text-tertiary ${className}`}
    >
      <Eye size={12} aria-hidden />
      View only
    </span>
  )
}
