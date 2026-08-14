import { useCallback, useEffect, useRef, useState } from 'react'
import { Check, Copy, X } from 'lucide-react'
import { httpErrorMessage } from '../lib/apiClient'
import { useFocusTrap } from '../hooks/useFocusTrap'
import { useOrgRole } from '../hooks/useOrgRole'
import { useTeams } from '../hooks/useTeams'
import type { CreateInviteResponse } from '../types'

interface Props {
  // create comes from the parent's useInvites so the pending-rows list and the
  // modal share one data lifecycle (a successful create reloads the list).
  create: (email: string, role: string, targetTeamId: string) => Promise<CreateInviteResponse>
  onClose: () => void
}

// Ascending org-role privilege ladder. 'owner' is the founder sentinel
// (orgs.owner_user_id) and is never grantable by invite — the backend rejects
// anything but admin|member too.
const ROLE_LADDER = ['member', 'admin', 'owner'] as const

// assignableRoles returns the roles an inviter may grant: never 'owner', and
// never above the caller's own org role. An owner and an admin both land on
// [member, admin]; a member never opens this modal (the People tab gates the
// "+ Invite" affordance on canManage).
function assignableRoles(callerRole: string | null): string[] {
  const callerRank = ROLE_LADDER.indexOf((callerRole ?? 'member') as (typeof ROLE_LADDER)[number])
  const rank = callerRank < 0 ? 0 : callerRank
  return ROLE_LADDER.filter((r, i) => r !== 'owner' && i <= rank)
}

// InviteModal is the org People roster's "+ Invite" affordance: email + role
// (capped to the caller's role) + an optional team, POSTed to /api/invites.
// On success it reveals the one-time accept link with a Copy button — no email
// is sent in v1, so copy/paste IS the delivery.
export default function InviteModal({ create, onClose }: Props) {
  const { role: callerRole } = useOrgRole()
  // The team picker offers the admin's own teams (GET /api/teams is
  // member-scoped); "No team" is the default. An admin inviting to a team they
  // don't belong to isn't reachable here in v1 — the common case is the
  // founder's Default team, which they're in.
  const { teams } = useTeams()
  const roles = assignableRoles(callerRole)

  const [email, setEmail] = useState('')
  const [role, setRole] = useState('member')
  const [teamId, setTeamId] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)
  // Set once the invite is created — flips the modal from the form to the
  // copy-the-link success state.
  const [created, setCreated] = useState<CreateInviteResponse | null>(null)

  // Trap keyboard focus inside the dialog while it's open and restore it to the
  // trigger on close (WCAG 2.1.2). Initial focus lands on the email field; the
  // container carries tabIndex={-1} for the empty-container fallback.
  const dialogRef = useRef<HTMLDivElement>(null)
  const emailRef = useRef<HTMLInputElement>(null)
  useFocusTrap(dialogRef, { initialFocus: emailRef })

  // Keep the selected role valid if the available set is narrower than the
  // default (defensive — both owner and admin currently see 'member').
  useEffect(() => {
    if (!roles.includes(role)) setRole(roles[roles.length - 1] ?? 'member')
  }, [roles, role])

  // Escape closes unless a create is in flight.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !submitting) onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose, submitting])

  const handleSubmit = useCallback(
    async (e: React.FormEvent) => {
      e.preventDefault()
      const trimmed = email.trim()
      if (trimmed === '' || submitting) return
      setSubmitting(true)
      setError(null)
      try {
        const res = await create(trimmed, role, teamId)
        setCreated(res)
      } catch (err) {
        // The 409 ("an invite for that email is already pending") is surfaced
        // verbatim inline, per the ticket; other failures get the server line
        // when present, else a generic message.
        setError(httpErrorMessage(err, 'Could not create the invite. Please try again.'))
      } finally {
        setSubmitting(false)
      }
    },
    [email, role, teamId, submitting, create],
  )

  const handleBackdrop = () => {
    if (!submitting) onClose()
  }

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-scrim backdrop-blur-sm"
      onClick={handleBackdrop}
    >
      <div
        ref={dialogRef}
        tabIndex={-1}
        className="relative w-full max-w-md overflow-y-auto rounded-2xl border border-line-1 bg-raised p-6 shadow-float shadow-black/[0.08] backdrop-blur-xl"
        role="dialog"
        aria-modal="true"
        aria-labelledby="invite-modal-title"
        onClick={(e) => e.stopPropagation()}
      >
        <header className="mb-5 flex items-start justify-between">
          <div>
            <h2
              id="invite-modal-title"
              className="text-lg font-semibold tracking-tight text-ink-1"
            >
              {created ? 'Invite created' : 'Invite to organization'}
            </h2>
            <p className="mt-0.5 text-ui text-ink-3">
              {created
                ? 'Copy this link and send it to them — it works once.'
                : 'They join after signing in and accepting the link.'}
            </p>
          </div>
          <button
            type="button"
            onClick={() => {
              if (!submitting) onClose()
            }}
            disabled={submitting}
            className="rounded-full p-1 text-ink-3 hover:bg-tint-2 hover:text-ink-2 disabled:cursor-not-allowed disabled:opacity-50"
            aria-label="Close"
          >
            <X size={16} />
          </button>
        </header>

        {created ? (
          <div className="space-y-4">
            <CopyLink url={created.accept_url} />
            <div className="flex justify-end">
              <button
                type="button"
                onClick={onClose}
                className="rounded-full bg-warm px-4 py-2 text-body font-medium text-warm-ink transition-all hover:opacity-90"
              >
                Done
              </button>
            </div>
          </div>
        ) : (
          <form onSubmit={handleSubmit} className="space-y-4">
            <Field label="Email" htmlFor="invite-email" required>
              <input
                ref={emailRef}
                id="invite-email"
                type="email"
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                required
                placeholder="person@example.com"
                className="w-full rounded-lg border border-line-1 bg-raised px-3 py-2 text-body text-ink-1 placeholder:text-ink-3 focus:border-warm focus:bg-raised focus:outline-none"
              />
            </Field>

            <Field label="Role" htmlFor="invite-role">
              <select
                id="invite-role"
                value={role}
                onChange={(e) => setRole(e.target.value)}
                className="w-full rounded-lg border border-line-1 bg-raised px-3 py-2 text-body text-ink-1 focus:border-warm focus:bg-raised focus:outline-none"
              >
                {roles.map((r) => (
                  <option key={r} value={r}>
                    {r}
                  </option>
                ))}
              </select>
            </Field>

            <Field label="Team" htmlFor="invite-team">
              <select
                id="invite-team"
                value={teamId}
                onChange={(e) => setTeamId(e.target.value)}
                className="w-full rounded-lg border border-line-1 bg-raised px-3 py-2 text-body text-ink-1 focus:border-warm focus:bg-raised focus:outline-none"
              >
                <option value="">No team</option>
                {teams.map((t) => (
                  <option key={t.id} value={t.id}>
                    {t.name}
                  </option>
                ))}
              </select>
            </Field>

            {error && (
              <p
                role="alert"
                className="rounded-lg border border-alarm/30 bg-alarm/[0.06] px-3 py-2 text-ui text-alarm"
              >
                {error}
              </p>
            )}

            <div className="flex justify-end gap-2 pt-1">
              <button
                type="button"
                onClick={onClose}
                disabled={submitting}
                className="rounded-full px-4 py-2 text-body text-ink-2 transition-all hover:bg-tint-2 hover:text-ink-1 disabled:opacity-50"
              >
                Cancel
              </button>
              <button
                type="submit"
                disabled={submitting || email.trim() === ''}
                className="rounded-full bg-warm px-4 py-2 text-body font-medium text-warm-ink transition-all hover:opacity-90 disabled:opacity-50"
              >
                {submitting ? 'Inviting…' : 'Send invite'}
              </button>
            </div>
          </form>
        )}
      </div>
    </div>
  )
}

// CopyLink shows a read-only link with a Copy button. Exported so the
// pending-row "resend" reveal reuses the same affordance. Clipboard failures
// (jsdom, blocked permissions) degrade gracefully — the field stays selectable.
export function CopyLink({ url }: { url: string }) {
  const [copied, setCopied] = useState(false)
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(() => () => clearTimeout(timer.current ?? undefined), [])

  const copy = useCallback(async () => {
    try {
      await navigator.clipboard.writeText(url)
      setCopied(true)
      clearTimeout(timer.current ?? undefined)
      timer.current = setTimeout(() => setCopied(false), 2000)
    } catch {
      // Clipboard unavailable — the input is selected for manual copy below.
    }
  }, [url])

  return (
    <div className="flex items-center gap-2">
      <input
        readOnly
        value={url}
        aria-label="Invite link"
        onFocus={(e) => e.currentTarget.select()}
        className="min-w-0 flex-1 rounded-lg border border-line-1 bg-raised px-3 py-2 font-mono text-ui text-ink-2 focus:border-warm focus:outline-none"
      />
      <button
        type="button"
        onClick={copy}
        className="inline-flex shrink-0 items-center gap-1.5 rounded-lg border border-line-1 bg-raised px-3 py-2 text-ui font-medium text-ink-2 transition-colors hover:bg-raised hover:text-ink-1"
      >
        {copied ? <Check size={13} className="text-ink-2" /> : <Copy size={13} />}
        {copied ? 'Copied' : 'Copy'}
      </button>
    </div>
  )
}

function Field({
  label,
  htmlFor,
  required,
  children,
}: {
  label: string
  htmlFor: string
  required?: boolean
  children: React.ReactNode
}) {
  return (
    <div>
      <label htmlFor={htmlFor} className="mb-1.5 block text-ui font-medium text-ink-2">
        {label}
        {required && <span className="ml-0.5 text-warm">*</span>}
      </label>
      {children}
    </div>
  )
}
