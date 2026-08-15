// SSOSettings — the per-org "Configure SSO" admin surface (TFAC-429), rendered
// as the body of the "Single sign-on" section on the /org Settings tab. Multi-
// mode only and admin-gated by its surface: the Settings tab only renders for
// org admins, and OrgSettings gates this section on !isLocal (local mode is N=1
// with no IdP, and every SSO endpoint 404s there). The backend additionally
// 404s a non-admin, so this is never the trust boundary — just the UI.
//
// One screen, three parts, mirroring how an operator wires up Entra:
//   1. SP details — the Entity ID + ACS URL (and, once connected, the Sign-on
//      URL the My Apps tile points at) to paste into the IdP. Always shown:
//      they're a pure function of the deployment URL, so the operator can set up
//      the Entra side before registering here.
//   2. Connection — paste the Entra App Federation Metadata URL to register
//      (POST), then enable/disable (PATCH). GoTrue fetches the metadata
//      server-side; TF stores only the org↔provider binding.
//   3. Domains — claim an email domain (POST), publish the shown DNS-TXT record,
//      Verify (POST {id}/verify) to flip Pending → Verified, or remove (DELETE).
//      Only a verified domain routes sign-in. Gated on having a connection — the
//      backend requires one before a domain can attach.
//
// Every endpoint resolves the org from the session's active org (no /api/orgs/
// prefix), so the apiClient calls pass no `org` option.

import { useEffect, useMemo, useRef, useState } from 'react'
import { Check, Copy, Trash2 } from 'lucide-react'
import * as Switch from '@radix-ui/react-switch'
import { apiFetch, apiJSON, httpErrorMessage } from '../../../lib/apiClient'
import { toast } from '../../../components/Toast/toastStore'
import { glassInputClass } from '../primitives'
import type { BreakGlassPrincipal, SSOConnectionResponse, SSODomain } from '../../../types'

export default function SSOSettings() {
  const [loading, setLoading] = useState(true)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [resp, setResp] = useState<SSOConnectionResponse | null>(null)
  const [domains, setDomains] = useState<SSODomain[]>([])
  // Bumping reloadKey re-runs the load effect; its cleanup cancels any in-flight
  // load first, so a rapid Retry (or an unmount mid-load) can't let a stale
  // completion win or setState on an unmounted component — mirrors the
  // cancelled-flag guard in the sibling OrgSettings.load.
  const [reloadKey, setReloadKey] = useState(0)
  const reload = () => setReloadKey((k) => k + 1)

  useEffect(() => {
    let cancelled = false
    setLoading(true)
    setLoadError(null)
    void (async () => {
      try {
        const conn = await apiJSON<SSOConnectionResponse>('/api/sso/connection')
        // Domains attach to a connection, so skip the (always-empty) list fetch
        // when none is registered yet.
        const doms = conn.connection ? await apiJSON<SSODomain[]>('/api/sso/domains') : []
        if (cancelled) return
        setResp(conn)
        setDomains(doms)
      } catch (e) {
        if (cancelled) return
        setLoadError(httpErrorMessage(e, 'Could not load SSO settings.'))
      } finally {
        if (!cancelled) setLoading(false)
      }
    })()
    return () => {
      cancelled = true
    }
  }, [reloadKey])

  if (loading) {
    return <p className="text-body text-ink-3">Loading SSO settings…</p>
  }
  if (loadError || !resp) {
    return (
      <div className="text-body text-ink-2">
        {loadError ?? 'Could not load SSO settings.'}{' '}
        <button type="button" onClick={reload} className="text-warm underline">
          Retry
        </button>
      </div>
    )
  }

  return (
    <div className="space-y-7">
      <SPDetails resp={resp} />
      <ConnectionBlock resp={resp} setResp={setResp} />
      {resp.connection && <DomainsBlock domains={domains} setDomains={setDomains} />}
      {resp.connection && <EnforcementBlock resp={resp} setResp={setResp} domains={domains} />}
    </div>
  )
}

// ── Part 1: SP details to paste into the IdP ────────────────────────────────

// SP_METADATA_SUFFIX is the fixed GoTrue SP metadata path the backend appends
// to the deployment's full public URL to build the Entity ID (see spValues in
// internal/server/sso_connection_handler.go). Stripping it from entity_id
// recovers the deployment base — INCLUDING any sub-path — which is the right
// base for TF's SP-initiated start endpoint too (TF's API and GoTrue's SP
// endpoints sit under the same public base).
const SP_METADATA_SUFFIX = '/auth/v1/sso/saml/metadata'

// ssoStartURLFrom builds the SP-initiated start URL (the My Apps bookmark /
// Entra "Sign-on URL") from the backend-provided Entity ID + the connection's
// provider_id. It derives the base by stripping the fixed metadata suffix —
// NOT via URL().origin, which would drop a sub-path and produce a wrong Sign-on
// URL for a deployment hosted under one (e.g. https://example.com/tf). The
// provider_id-before-return_to order + %2F encoding mirror the backend's
// ssoStartURL so the displayed value matches what TF actually serves. Returns
// '' when entity_id doesn't match the expected shape (a guessed URL is worse
// than none) — the backend contract guarantees the suffix in practice.
function ssoStartURLFrom(entityID: string, providerID: string): string {
  if (!entityID.endsWith(SP_METADATA_SUFFIX)) return ''
  const base = entityID.slice(0, -SP_METADATA_SUFFIX.length)
  const q = new URLSearchParams()
  q.set('provider_id', providerID)
  q.set('return_to', '/')
  return `${base}/api/auth/oauth/saml?${q.toString()}`
}

function SPDetails({ resp }: { resp: SSOConnectionResponse }) {
  // The Sign-on URL is the TF SP-initiated start endpoint carrying the
  // connection's provider_id — the same URL the identifier-first login path
  // (TFAC-427) and the tile both target. Only buildable once a connection
  // exists (it needs the provider_id).
  const bookmarkURL = useMemo(
    () => (resp.connection ? ssoStartURLFrom(resp.entity_id, resp.connection.provider_id) : ''),
    [resp.connection, resp.entity_id],
  )

  return (
    <div className="space-y-3">
      <BlockHeader
        title="Service provider details"
        hint="Paste these into your identity provider (e.g. Microsoft Entra) when you create the SAML enterprise app."
      />
      <CopyField label="Identifier (Entity ID)" value={resp.entity_id} />
      <CopyField label="Reply URL (ACS)" value={resp.acs_url} />
      {bookmarkURL && (
        <CopyField
          label="Sign-on URL"
          value={bookmarkURL}
          hint="In Entra, also set the Sign-on URL to this so the My Apps tile launches sign-in here (SP-initiated)."
        />
      )}
    </div>
  )
}

// ── Part 2: the IdP connection ──────────────────────────────────────────────
function ConnectionBlock({
  resp,
  setResp,
}: {
  resp: SSOConnectionResponse
  setResp: React.Dispatch<React.SetStateAction<SSOConnectionResponse | null>>
}) {
  const connection = resp.connection
  const [metadataURL, setMetadataURL] = useState('')
  const [connecting, setConnecting] = useState(false)
  const [toggling, setToggling] = useState(false)

  const connect = async () => {
    const url = metadataURL.trim()
    if (url === '' || connecting) return
    setConnecting(true)
    try {
      const next = await apiJSON<SSOConnectionResponse>('/api/sso/connection', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ metadata_url: url }),
      })
      setResp(next)
      setMetadataURL('')
      toast.success('SSO connection registered')
    } catch (e) {
      // 409 "already registered", 503 "admin token not configured", and the
      // https-only validation 400 all surface their server message verbatim.
      toast.error(httpErrorMessage(e, 'Could not register the SSO connection.'))
    } finally {
      setConnecting(false)
    }
  }

  const toggle = async (enabled: boolean) => {
    if (toggling) return
    setToggling(true)
    try {
      const next = await apiJSON<SSOConnectionResponse>('/api/sso/connection', {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ enabled }),
      })
      setResp(next)
      toast.success(enabled ? 'SSO enabled' : 'SSO disabled')
    } catch (e) {
      toast.error(httpErrorMessage(e, 'Could not update the SSO connection.'))
    } finally {
      setToggling(false)
    }
  }

  // runTest opens the verify-before-enforce round-trip (TFAC-431) in a popup: a
  // GET that 303s through the IdP and renders a pass/fail page server-side. A new
  // window keeps the admin's current session untouched (the test path mints none)
  // and the result visible alongside Settings. Works on a not-yet-enabled
  // connection — that's the point, confirming the IdP before flipping it on.
  //
  // noopener,noreferrer severs window.opener: the popup navigates to the external
  // IdP (Entra), and without this that IdP-controlled page could reach back
  // through window.opener to navigate this Settings tab (reverse tab-nabbing). We
  // never call back to the opener (the result renders in the popup itself), so
  // dropping the handle costs nothing.
  const runTest = () => {
    window.open(
      '/api/sso/connection/test',
      'tf-sso-test',
      'popup,noopener,noreferrer,width=480,height=620',
    )
  }

  return (
    <div className="space-y-3">
      <BlockHeader
        title="Identity provider connection"
        hint={
          connection
            ? 'Your organization is connected. Disable to pause SSO sign-in without removing the connection.'
            : 'Paste your Entra App Federation Metadata URL to register the connection.'
        }
      />

      {connection ? (
        <div className="flex items-center justify-between gap-4 rounded-2xl border border-[var(--color-line-1)] bg-[var(--color-raised)]/40 px-4 py-3">
          <div className="min-w-0">
            <p className="text-body font-medium text-ink-1">
              {connection.enabled ? 'Connected · enabled' : 'Connected · disabled'}
            </p>
            <p className="mt-0.5 truncate font-mono text-reported text-ink-3">
              Provider {connection.provider_id}
            </p>
          </div>
          <div className="flex shrink-0 items-center gap-3">
            <button
              type="button"
              onClick={runTest}
              title="Open a real sign-in in a new window to confirm the connection works — without enabling it."
              className="rounded-full border border-warm/20 px-3.5 py-1.5 text-ui font-medium text-warm transition-colors hover:border-warm/30 hover:text-warm/80"
            >
              Test SSO
            </button>
            <label className="flex cursor-pointer items-center gap-2 text-ui text-ink-3">
              {connection.enabled ? 'Enabled' : 'Disabled'}
              <Switch.Root
                aria-label="Enable SSO"
                checked={connection.enabled}
                disabled={toggling}
                onCheckedChange={(v) => void toggle(v)}
                className="relative h-[18px] w-8 rounded-full transition-colors data-[state=checked]:bg-warm data-[state=unchecked]:bg-line-2 disabled:opacity-50"
              >
                <Switch.Thumb className="block h-[14px] w-[14px] rounded-full bg-raised shadow transition-transform data-[state=checked]:translate-x-[14px] data-[state=unchecked]:translate-x-[2px]" />
              </Switch.Root>
            </label>
          </div>
        </div>
      ) : (
        <div className="flex items-center gap-2">
          <input
            type="url"
            value={metadataURL}
            aria-label="App Federation Metadata URL"
            placeholder="https://login.microsoftonline.com/…/federationmetadata.xml"
            onChange={(e) => setMetadataURL(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter') {
                e.preventDefault()
                void connect()
              }
            }}
            className={`${glassInputClass} font-mono text-ui`}
          />
          <button
            type="button"
            onClick={() => void connect()}
            disabled={metadataURL.trim() === '' || connecting}
            className="shrink-0 rounded-full bg-warm px-5 py-2.5 text-body font-medium text-warm-ink shadow-[0_10px_28px_-10px_var(--color-warm)] transition-all hover:bg-warm/90 disabled:opacity-40 disabled:shadow-none"
          >
            {connecting ? 'Connecting…' : 'Connect'}
          </button>
        </div>
      )}
    </div>
  )
}

// ── Part 3: domain claim + verification ─────────────────────────────────────
function DomainsBlock({
  domains,
  setDomains,
}: {
  domains: SSODomain[]
  setDomains: React.Dispatch<React.SetStateAction<SSODomain[]>>
}) {
  const [newDomain, setNewDomain] = useState('')
  const [adding, setAdding] = useState(false)

  const add = async () => {
    const d = newDomain.trim()
    if (d === '' || adding) return
    setAdding(true)
    try {
      const created = await apiJSON<SSODomain>('/api/sso/domains', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ domain: d }),
      })
      // POST is idempotent (a re-claim returns the existing row), so upsert by id
      // rather than append — adding the same domain twice can't duplicate a row.
      setDomains((ds) => [...ds.filter((x) => x.id !== created.id), created])
      setNewDomain('')
    } catch (e) {
      // 400 "must be a bare domain like example.com" surfaces verbatim.
      toast.error(httpErrorMessage(e, 'Could not add the domain.'))
    } finally {
      setAdding(false)
    }
  }

  return (
    <div className="space-y-3">
      <BlockHeader
        title="Email domains"
        hint="Claim each email domain your members sign in with, then prove control by publishing the DNS record shown. Only a verified domain routes sign-in to your organization."
      />

      <div className="flex items-center gap-2">
        <input
          type="text"
          value={newDomain}
          aria-label="Domain to claim"
          placeholder="example.com"
          onChange={(e) => setNewDomain(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') {
              e.preventDefault()
              void add()
            }
          }}
          className={glassInputClass}
        />
        <button
          type="button"
          onClick={() => void add()}
          disabled={newDomain.trim() === '' || adding}
          className="shrink-0 rounded-full border border-warm/20 px-5 py-2.5 text-body font-medium text-warm transition-colors hover:border-warm/30 hover:text-warm/80 disabled:opacity-40"
        >
          {adding ? 'Adding…' : 'Add domain'}
        </button>
      </div>

      {domains.length > 0 && (
        <ul className="space-y-2">
          {domains.map((d) => (
            <DomainRow key={d.id} domain={d} setDomains={setDomains} />
          ))}
        </ul>
      )}
    </div>
  )
}

function DomainRow({
  domain,
  setDomains,
}: {
  domain: SSODomain
  setDomains: React.Dispatch<React.SetStateAction<SSODomain[]>>
}) {
  const [busy, setBusy] = useState(false)
  const [message, setMessage] = useState<string | null>(null)
  const verified = domain.status === 'verified'

  const verify = async () => {
    if (busy) return
    setBusy(true)
    setMessage(null)
    try {
      const next = await apiJSON<SSODomain>(`/api/sso/domains/${domain.id}/verify`, {
        method: 'POST',
      })
      setDomains((ds) => ds.map((x) => (x.id === next.id ? next : x)))
      toast.success(`${next.domain} verified`)
    } catch (e) {
      // The two 409 states — "DNS record not found yet — propagation can take
      // time, retry." and "this domain is already verified by another
      // organization" — come back as friendly server messages. Surface them
      // inline on the row (not a transient toast) so the operator can read the
      // DNS record and the message side by side, then retry.
      setMessage(httpErrorMessage(e, 'Could not verify the domain yet — try again shortly.'))
    } finally {
      setBusy(false)
    }
  }

  const remove = async () => {
    if (busy) return
    if (
      !window.confirm(
        `Remove ${domain.domain}? Members on this domain will no longer be routed to SSO.`,
      )
    ) {
      return
    }
    setBusy(true)
    try {
      await apiFetch(`/api/sso/domains/${domain.id}`, { method: 'DELETE' })
      // On success the row unmounts, so don't clear `busy` afterwards.
      setDomains((ds) => ds.filter((x) => x.id !== domain.id))
      toast.success(`${domain.domain} removed`)
    } catch (e) {
      toast.error(httpErrorMessage(e, 'Could not remove the domain.'))
      setBusy(false)
    }
  }

  return (
    <li className="rounded-2xl border border-[var(--color-line-1)] bg-[var(--color-raised)]/40 px-4 py-3">
      <div className="flex items-center justify-between gap-3">
        <div className="flex min-w-0 items-center gap-2">
          <span className="truncate text-body font-medium text-ink-1">{domain.domain}</span>
          <StatusBadge verified={verified} />
        </div>
        <div className="flex shrink-0 items-center gap-1.5">
          {!verified && (
            <button
              type="button"
              onClick={() => void verify()}
              disabled={busy}
              className="rounded-full border border-warm/20 px-3.5 py-1.5 text-ui font-medium text-warm transition-colors hover:border-warm/30 hover:text-warm/80 disabled:opacity-40"
            >
              {busy ? 'Verifying…' : 'Verify'}
            </button>
          )}
          <button
            type="button"
            onClick={() => void remove()}
            disabled={busy}
            aria-label={`Remove ${domain.domain}`}
            className="inline-flex items-center rounded-full p-1.5 text-ink-3 transition-colors hover:text-alarm disabled:opacity-40"
          >
            <Trash2 size={14} />
          </button>
        </div>
      </div>

      {!verified && (
        <div className="mt-3 space-y-2">
          <p className="text-reported leading-relaxed text-ink-3">
            Create this DNS record at your domain host, then click Verify:
          </p>
          <CopyField label="Host / name" value={domain.txt_host} />
          <div className="flex items-center gap-2 text-reported text-ink-3">
            <span className="font-medium uppercase tracking-wide">Type</span>
            <span className="font-mono text-ink-2">TXT</span>
          </div>
          <CopyField label="Value" value={domain.txt_value} />
        </div>
      )}

      {message && (
        <p role="status" className="mt-2 text-ui leading-relaxed text-ink-2">
          {message}
        </p>
      )}
    </li>
  )
}

// ── Part 4: require SSO (enforcement) + break-glass ─────────────────────────
function EnforcementBlock({
  resp,
  setResp,
  domains,
}: {
  resp: SSOConnectionResponse
  setResp: React.Dispatch<React.SetStateAction<SSOConnectionResponse | null>>
  domains: SSODomain[]
}) {
  const connection = resp.connection
  const [toggling, setToggling] = useState(false)

  // The toggle is available only behind a proven-working connection — enabled,
  // a passed Test, and at least one verified domain — so enforcing can never
  // brick the org (the backend re-checks all three; this just mirrors it as a
  // disabled-with-explanation control). connection is non-null (the parent only
  // renders this block when one exists).
  const hasVerifiedDomain = domains.some((d) => d.status === 'verified')
  const enabled = connection?.enabled ?? false
  const tested = connection?.tested ?? false
  const enforced = connection?.enforced ?? false
  const canEnforce = enabled && tested && hasVerifiedDomain

  // The single reason the toggle is unavailable, surfaced inline. Order mirrors
  // the operator's setup sequence (enable → test → verify a domain).
  const blockReason = !enabled
    ? 'Enable the connection first.'
    : !tested
      ? 'Run a successful Test first (under the connection above).'
      : !hasVerifiedDomain
        ? 'Verify at least one email domain first.'
        : null

  const toggle = async (next: boolean) => {
    if (toggling || !connection) return
    // Guard the ON direction in the UI too (the switch is disabled when it can't
    // enforce, but a confirm on the irreversible-feeling action is worth it).
    if (next) {
      const ok = window.confirm(
        'Require SSO for your verified domains? Members on those domains will no longer be able to sign in with GitHub — only via SSO. Break-glass principals (below) keep GitHub access so you can recover.',
      )
      if (!ok) return
    }
    setToggling(true)
    try {
      const updated = await apiJSON<SSOConnectionResponse>('/api/sso/connection/enforcement', {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ enforced: next }),
      })
      setResp(updated)
      toast.success(next ? 'SSO is now required' : 'SSO is no longer required')
    } catch (e) {
      // The 409 gating messages ("enable the connection first", etc.) surface
      // verbatim — a race where the connection changed under the admin.
      toast.error(httpErrorMessage(e, 'Could not update SSO enforcement.'))
    } finally {
      setToggling(false)
    }
  }

  return (
    <div className="space-y-3">
      <BlockHeader
        title="Require SSO"
        hint="Make SSO mandatory for your verified domains. Members on those domains must sign in via SSO instead of GitHub. Off-domain users (e.g. invited externals) are unaffected."
      />

      <div className="flex items-center justify-between gap-4 rounded-2xl border border-[var(--color-line-1)] bg-[var(--color-raised)]/40 px-4 py-3">
        <div className="min-w-0">
          <p className="text-body font-medium text-ink-1">
            {enforced ? 'SSO required' : 'SSO optional'}
          </p>
          {!canEnforce && !enforced && blockReason && (
            <p className="mt-0.5 text-ui leading-relaxed text-ink-3">{blockReason}</p>
          )}
        </div>
        <label className="flex shrink-0 cursor-pointer items-center gap-2 text-ui text-ink-3">
          {enforced ? 'Required' : 'Optional'}
          <Switch.Root
            aria-label="Require SSO"
            checked={enforced}
            // Allow turning OFF always (recovery); only gate turning ON.
            disabled={toggling || (!enforced && !canEnforce)}
            onCheckedChange={(v) => void toggle(v)}
            className="relative h-[18px] w-8 rounded-full transition-colors data-[state=checked]:bg-warm data-[state=unchecked]:bg-line-2 disabled:opacity-50"
          >
            <Switch.Thumb className="block h-[14px] w-[14px] rounded-full bg-raised shadow transition-transform data-[state=checked]:translate-x-[14px] data-[state=unchecked]:translate-x-[2px]" />
          </Switch.Root>
        </label>
      </div>

      <BreakGlassManager enforced={enforced} />
    </div>
  )
}

// BreakGlassManager lists the principals who keep GitHub login under enforcement
// and lets an admin add (by email) / remove them. The owner is the default,
// seeded by the backend when enforcement is first enabled. Removing the last one
// while SSO is required is refused server-side (the no-recovery guard) and shown
// as an error toast.
function BreakGlassManager({ enforced }: { enforced: boolean }) {
  const [principals, setPrincipals] = useState<BreakGlassPrincipal[]>([])
  const [loading, setLoading] = useState(true)
  const [email, setEmail] = useState('')
  const [adding, setAdding] = useState(false)

  // Refetch when `enforced` flips: enabling enforcement seeds the owner as the
  // default break-glass principal server-side, so the list must reload to show
  // it (otherwise it would misleadingly read empty right after enabling).
  useEffect(() => {
    let cancelled = false
    void (async () => {
      try {
        const list = await apiJSON<BreakGlassPrincipal[]>('/api/sso/break-glass')
        if (!cancelled) setPrincipals(list)
      } catch {
        // Non-fatal: the section just shows empty. The toggle above still works.
      } finally {
        if (!cancelled) setLoading(false)
      }
    })()
    return () => {
      cancelled = true
    }
  }, [enforced])

  const add = async () => {
    const e = email.trim()
    if (e === '' || adding) return
    setAdding(true)
    try {
      const list = await apiJSON<BreakGlassPrincipal[]>('/api/sso/break-glass', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ email: e }),
      })
      setPrincipals(list)
      setEmail('')
      toast.success('Break-glass principal added')
    } catch (err) {
      // 404 "no member of this organization has that verified email" surfaces verbatim.
      toast.error(httpErrorMessage(err, 'Could not add the break-glass principal.'))
    } finally {
      setAdding(false)
    }
  }

  const remove = async (p: BreakGlassPrincipal) => {
    const label = p.email || p.display_name || p.user_id
    if (!window.confirm(`Remove ${label} as a break-glass principal?`)) return
    try {
      await apiFetch(`/api/sso/break-glass/${p.user_id}`, { method: 'DELETE' })
      setPrincipals((ps) => ps.filter((x) => x.user_id !== p.user_id))
      toast.success('Break-glass principal removed')
    } catch (err) {
      // 409 "can't remove the last break-glass principal while SSO is required …"
      // surfaces verbatim.
      toast.error(httpErrorMessage(err, 'Could not remove the break-glass principal.'))
    }
  }

  return (
    <div className="space-y-2 rounded-2xl border border-[var(--color-line-1)] bg-[var(--color-raised)]/20 px-4 py-3">
      <div className="space-y-0.5">
        <h4 className="text-ui font-medium text-ink-1">Break-glass access</h4>
        <p className="text-reported leading-relaxed text-ink-3">
          These principals can still sign in with GitHub under enforcement — the recovery path if
          your identity provider breaks.{' '}
          {enforced && 'You can’t remove the last one while SSO is required.'}
        </p>
      </div>

      {!loading && principals.length > 0 && (
        <ul className="space-y-1.5">
          {principals.map((p) => (
            <li
              key={p.user_id}
              className="flex items-center justify-between gap-3 rounded-xl bg-[var(--color-raised)]/40 px-3 py-2"
            >
              <span className="min-w-0 truncate text-ui text-ink-2">
                {p.email || p.display_name || p.user_id}
              </span>
              <button
                type="button"
                onClick={() => void remove(p)}
                aria-label={`Remove ${p.email || p.user_id}`}
                className="inline-flex shrink-0 items-center rounded-full p-1.5 text-ink-3 transition-colors hover:text-alarm"
              >
                <Trash2 size={13} />
              </button>
            </li>
          ))}
        </ul>
      )}

      <div className="flex items-center gap-2">
        <input
          type="email"
          value={email}
          aria-label="Break-glass principal email"
          placeholder="person@company.com"
          onChange={(e) => setEmail(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') {
              e.preventDefault()
              void add()
            }
          }}
          className={glassInputClass}
        />
        <button
          type="button"
          onClick={() => void add()}
          disabled={email.trim() === '' || adding}
          className="shrink-0 rounded-full border border-warm/20 px-4 py-2 text-ui font-medium text-warm transition-colors hover:border-warm/30 hover:text-warm/80 disabled:opacity-40"
        >
          {adding ? 'Adding…' : 'Add'}
        </button>
      </div>
    </div>
  )
}

// ── Shared leaf bits ────────────────────────────────────────────────────────

function BlockHeader({ title, hint }: { title: string; hint: string }) {
  return (
    <div className="space-y-1">
      <h3 className="text-body font-medium text-ink-1">{title}</h3>
      <p className="text-ui leading-relaxed text-ink-3">{hint}</p>
    </div>
  )
}

function StatusBadge({ verified }: { verified: boolean }) {
  return verified ? (
    <span className="inline-flex shrink-0 items-center gap-1 rounded-full bg-tint-2 px-2 py-0.5 text-reported font-medium text-ink-2">
      <Check size={11} aria-hidden /> Verified
    </span>
  ) : (
    <span className="inline-flex shrink-0 items-center rounded-full bg-tint-3 px-2 py-0.5 text-reported font-medium text-ink-3">
      Pending
    </span>
  )
}

// CopyField — a read-only value with a Copy button, matching the SP/callback
// field pattern used across settings (AtlassianOAuthAppCard / GitHubAppImportForm).
// The input carries an aria-label so the value is reachable by name; the button
// labels itself "Copy {label}".
function CopyField({ label, value, hint }: { label: string; value: string; hint?: string }) {
  const [copied, setCopied] = useState(false)
  // Hold the "Copied" reset timer so an unmount (or a rapid re-copy) clears it
  // rather than firing setCopied on a gone component.
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null)
  useEffect(
    () => () => {
      if (timerRef.current) clearTimeout(timerRef.current)
    },
    [],
  )
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(value)
      setCopied(true)
      if (timerRef.current) clearTimeout(timerRef.current)
      timerRef.current = setTimeout(() => setCopied(false), 1500)
    } catch {
      // Clipboard denied — the value is still selectable in the field.
    }
  }
  return (
    <div className="space-y-1.5">
      <span className="block text-reported font-medium uppercase tracking-wide text-ink-3">
        {label}
      </span>
      <div className="flex items-center gap-2">
        <input
          type="text"
          readOnly
          value={value}
          aria-label={label}
          onFocus={(e) => e.target.select()}
          className={`${glassInputClass} font-mono text-ui`}
        />
        <button
          type="button"
          onClick={() => void copy()}
          aria-label={`Copy ${label}`}
          className="inline-flex shrink-0 items-center gap-1 rounded-xl border border-[var(--color-line-1)] px-3 py-2 text-ui font-medium text-ink-2 transition-colors hover:text-ink-1"
        >
          {copied ? <Check size={13} /> : <Copy size={13} />}
          {copied ? 'Copied' : 'Copy'}
        </button>
      </div>
      {hint && <p className="text-reported leading-relaxed text-ink-3">{hint}</p>}
    </div>
  )
}
