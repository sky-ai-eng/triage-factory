import { useCallback, useEffect, useState } from 'react'
import { useLocation, useNavigate } from 'react-router'
import { useAuth } from '../contexts/AuthContext'
import { apiErrors, apiJSON, httpErrorMessage, HttpError } from '../lib/apiClient'
import type { AcceptInviteResponse, InvitePreview } from '../types'

/**
 * InviteAccept is the public invite-redemption page (TFAC-418), mounted at a
 * TOP-LEVEL /invite/accept route OUTSIDE the org-context AuthGate — a brand-new
 * invitee has no org or membership yet. It still sits inside AuthProvider (a
 * sibling of /login and /onboarding), so it can read whether the visitor is
 * signed in.
 *
 * Flow, driven by ?token= :
 *   1. GET /api/invites/preview?token=… (unauthenticated) → headline or a
 *      terminal state (expired / revoked / accepted / not_found).
 *   2. Valid + logged out → "Sign in to accept" bounces to GitHub OAuth with
 *      the token riding return_to (safe through normalizeReturnTo).
 *   3. Valid + logged in → explicit Accept → POST /api/invites/accept → route
 *      into the org the backend just dropped them into.
 *   4. Wrong identity (409 INVITE_EMAIL_MISMATCH) → the actionable mismatch
 *      message, which names both the invited address and the signed-in one,
 *      plus "log out and sign in with the invited account".
 *
 * Multi-mode only: the route is registered in MultiRoutes; local mode never
 * mounts it (and the backend 404s every invite route there).
 */
export default function InviteAccept() {
  const auth = useAuth()
  const navigate = useNavigate()
  const location = useLocation()
  const token = new URLSearchParams(location.search).get('token') ?? ''

  const [preview, setPreview] = useState<InvitePreview | null>(null)
  const [previewLoading, setPreviewLoading] = useState(true)
  const [previewError, setPreviewError] = useState<string | null>(null)

  const [accepting, setAccepting] = useState(false)
  const [acceptError, setAcceptError] = useState<string | null>(null)
  // Set on the 409 INVITE_EMAIL_MISMATCH case: the visitor is signed in as
  // someone other than the invited address. The server's message names both
  // addresses, so it is rendered verbatim above the switch-account button.
  const [mismatch, setMismatch] = useState<{ message: string } | null>(null)

  const loadPreview = useCallback(async () => {
    if (!token) {
      setPreviewLoading(false)
      return
    }
    setPreviewLoading(true)
    setPreviewError(null)
    try {
      setPreview(
        await apiJSON<InvitePreview>(`/api/invites/preview?token=${encodeURIComponent(token)}`),
      )
    } catch (e) {
      setPreviewError(httpErrorMessage(e, 'Could not load this invitation.'))
    } finally {
      setPreviewLoading(false)
    }
  }, [token])

  useEffect(() => {
    void loadPreview()
  }, [loadPreview])

  // Bounce to GitHub OAuth, carrying the token back to this page via return_to.
  // The token rides the relative path (confirmed safe through the server's
  // normalizeReturnTo open-redirect guard).
  const goSignIn = useCallback(() => {
    const returnTo = `/invite/accept?token=${token}`
    window.location.href = `/api/auth/oauth/github?return_to=${encodeURIComponent(returnTo)}`
  }, [token])

  const handleSwitchAccount = useCallback(async () => {
    // Drop the current TF session, then re-enter OAuth so the invitee can sign
    // in as the invited address. (We can't force GitHub to switch accounts, but
    // clearing our session + re-prompting is the actionable path.)
    await auth.logout()
    goSignIn()
  }, [auth, goSignIn])

  const handleAccept = useCallback(async () => {
    setAccepting(true)
    setAcceptError(null)
    setMismatch(null)
    try {
      const res = await apiJSON<AcceptInviteResponse>('/api/invites/accept', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ token }),
      })
      // Refresh /me BEFORE navigating: the membership + active org now exist
      // server-side, but AuthGate gates the org route on auth.orgs, which is
      // stale until we reload — navigating first would bounce us right back out.
      await auth.refresh()
      navigate('/orgs/' + res.org_id, { replace: true })
    } catch (e) {
      if (e instanceof HttpError && e.status === 409) {
        const item = apiErrors(e)[0]
        if (item?.reason === 'INVITE_EMAIL_MISMATCH') {
          // The message names both addresses — the invited one and the one
          // the caller is signed in as — so the card renders it verbatim.
          setMismatch({ message: item.message })
        } else {
          setAcceptError(httpErrorMessage(e, 'This invitation can no longer be accepted.'))
        }
      } else if (e instanceof HttpError && (e.status === 404 || e.status === 400)) {
        setAcceptError('This invitation could not be found.')
      } else if (e instanceof HttpError && e.status === 401) {
        // Session lapsed between preview and accept — the global 401 handler
        // flips auth to unauth and the render falls back to the sign-in state.
        // No scary error needed.
      } else {
        setAcceptError(httpErrorMessage(e, 'Could not accept the invitation. Please try again.'))
      }
      setAccepting(false)
    }
  }, [token, auth, navigate])

  // ── Render ────────────────────────────────────────────────────────────
  if (!token) {
    return (
      <Card title="Invalid invite link">
        <p className="text-[13px] leading-relaxed text-text-tertiary">
          This link is missing its invitation token. Ask your admin for a fresh invite link.
        </p>
      </Card>
    )
  }

  // Wait for both the preview and the session to resolve before deciding which
  // call-to-action to render (the headline depends on the preview; the CTA on
  // whether you're signed in).
  if (previewLoading || auth.status === 'loading') {
    return (
      <Card title="Checking your invitation…">
        <p className="text-[13px] text-text-tertiary">One moment.</p>
      </Card>
    )
  }

  if (previewError) {
    return (
      <Card title="Couldn’t load the invitation">
        <p className="mb-4 text-[13px] leading-relaxed text-text-tertiary">{previewError}</p>
        <PrimaryButton onClick={() => void loadPreview()}>Try again</PrimaryButton>
      </Card>
    )
  }

  if (!preview || preview.status !== 'valid') {
    return (
      <Card title="This invitation isn’t available">
        <p className="text-[13px] leading-relaxed text-text-tertiary">
          {terminalMessage(preview?.status)}
        </p>
      </Card>
    )
  }

  const orgName = preview.org_name || 'this organization'

  // Wrong-identity: the actionable mismatch from the accept 409.
  if (mismatch) {
    return (
      <Card title="Signed in as the wrong account">
        <p className="mb-4 text-[13px] leading-relaxed text-text-tertiary">{mismatch.message}</p>
        <PrimaryButton onClick={() => void handleSwitchAccount()}>
          Log out and sign in with the invited account
        </PrimaryButton>
      </Card>
    )
  }

  return (
    <Card title="You’ve been invited">
      <p className="text-[13px] leading-relaxed text-text-secondary">
        You’ve been invited to <span className="font-semibold text-text-primary">{orgName}</span> as{' '}
        <span className="font-semibold text-text-primary">{preview.role}</span>.
      </p>
      {preview.invited_by_name && (
        <p className="mt-1 text-[12px] text-text-tertiary">Invited by {preview.invited_by_name}.</p>
      )}

      <div className="mt-5">
        {auth.status === 'authed' ? (
          <>
            <PrimaryButton onClick={() => void handleAccept()} disabled={accepting}>
              {accepting ? 'Accepting…' : 'Accept invitation'}
            </PrimaryButton>
            {acceptError && (
              <p role="alert" className="mt-3 text-[12px] text-dismiss">
                {acceptError}
              </p>
            )}
          </>
        ) : (
          <>
            <PrimaryButton onClick={goSignIn}>Sign in to accept</PrimaryButton>
            <p className="mt-3 text-[11px] leading-relaxed text-text-tertiary">
              You’ll sign in with GitHub, then land back here to accept.
            </p>
          </>
        )}
      </div>
    </Card>
  )
}

// terminalMessage maps a non-valid preview status to the dead-end copy.
function terminalMessage(status: InvitePreview['status'] | undefined): string {
  switch (status) {
    case 'expired':
      return 'This invitation has expired. Ask your admin to send a fresh one.'
    case 'revoked':
      return 'This invitation has been revoked. Ask your admin for a new invite.'
    case 'accepted':
      return 'This invitation has already been accepted. Try signing in instead.'
    default:
      // not_found, or an unexpected/empty status.
      return 'We couldn’t find this invitation. The link may be incorrect or no longer valid.'
  }
}

function Card({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="flex min-h-screen items-center justify-center bg-surface p-4">
      <div className="w-full max-w-md space-y-4 rounded-2xl border border-border-glass bg-surface-raised p-8 shadow-lg shadow-black/[0.04] backdrop-blur-xl">
        <h1 className="text-[22px] font-semibold tracking-tight text-text-primary">{title}</h1>
        {children}
      </div>
    </div>
  )
}

function PrimaryButton({
  onClick,
  disabled,
  children,
}: {
  onClick: () => void
  disabled?: boolean
  children: React.ReactNode
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      disabled={disabled}
      className="w-full rounded-xl bg-accent px-4 py-2.5 text-[13px] font-medium text-white transition-colors hover:bg-accent/90 disabled:cursor-not-allowed disabled:opacity-40"
    >
      {children}
    </button>
  )
}
