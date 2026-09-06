import { useEffect, useState } from 'react'
import type { CSSProperties, KeyboardEvent, ReactNode } from 'react'
import './accounts.css'

export type AccountKind = 'github' | 'jira'

/**
 * How the org reaches this system, which decides what the verb opens.
 *  github: 'app' (one-click Connect through the org's GitHub App) | 'pat'
 *  jira:   'oauth' (Atlassian OAuth app) | 'cloud' (email + API token) | 'dc' (personal access token)
 */
export type AccountMethod = 'app' | 'pat' | 'oauth' | 'cloud' | 'dc'

export type Account = {
  id: string
  kind: AccountKind
  /** Display name; defaults from kind. */
  name?: string
  /** The bound account — `@login` for GitHub, the Atlassian email for Jira. null = not connected. */
  account: string | null
  /** The host the binding is keyed under: github.com, a GHES origin, an Atlassian site. */
  host?: string
  method?: AccountMethod
  /** Overrides the band's explanatory line. */
  hint?: string
}

export type AccountsProps = {
  accounts: Account[]
  /** Draws the skeleton — outlined bars at the rows' real proportions. */
  loading?: boolean
  /** API unreachable: values read `--`, verbs are absent. */
  offline?: boolean
  /** false: a readout. Verbs are absent, not disabled. */
  interactive?: boolean
  label?: string
  note?: string
  /** Continue with GitHub / Atlassian: the caller redirects. */
  onConnect?: (id: string) => void
  /**
   * The token path. Resolve with the bound account string to show; throw an
   * Error whose message is the server's own words to show under the field.
   */
  onVerify?: (id: string, fields: { token: string; email?: string }) => Promise<string>
  /** Fires after a verify lands. */
  onChange?: (id: string, account: string) => void
  className?: string
  style?: CSSProperties
}

// Accounts — the integration identities a person holds: which GitHub account
// the factory matches their pull requests to, which Jira account it acts as.
// One line per system; the verb on the line is the only thing that can change
// it, and pressing it opens a band under the line whose body follows how the
// org reaches that system. Sign-in is NOT here — a login identity is a fact
// about the session and lives in the page header. See Accounts.md.

const GH_PATH =
  'M12 .5C5.73.5.5 5.73.5 12c0 5.02 3.29 9.28 7.86 10.74.58.11.79-.25.79-.55v-2.1c-3.2.7-3.88-1.54-3.88-1.54-.53-1.34-1.29-1.7-1.29-1.7-1.05-.72.08-.7.08-.7 1.16.08 1.77 1.19 1.77 1.19 1.03 1.77 2.7 1.26 3.36.96.1-.75.4-1.26.73-1.55-2.55-.29-5.23-1.28-5.23-5.7 0-1.26.45-2.29 1.19-3.1-.12-.29-.52-1.46.11-3.05 0 0 .96-.31 3.15 1.18a10.9 10.9 0 0 1 5.74 0c2.19-1.49 3.15-1.18 3.15-1.18.63 1.59.23 2.76.11 3.05.74.81 1.19 1.84 1.19 3.1 0 4.43-2.69 5.4-5.25 5.69.41.36.78 1.06.78 2.14v3.17c0 .3.21.67.8.55C20.71 21.28 24 17.02 24 12 24 5.73 18.77.5 12 .5z'

/**
 * Vendor marks stay literal. GitHub's brand black draws in ink-1 (or the color
 * given) so it survives either ground; Jira keeps its own blue.
 */
export function AccountMark({
  kind,
  size = 14,
  color = 'var(--color-ink-1)',
}: {
  kind: AccountKind
  size?: number
  color?: string
}) {
  if (kind === 'github')
    return (
      <svg
        className="ac-mark"
        width={size}
        height={size}
        viewBox="0 0 24 24"
        fill={color}
        aria-hidden="true"
      >
        <path d={GH_PATH} />
      </svg>
    )
  return (
    <svg
      className="ac-mark"
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      aria-hidden="true"
    >
      <path
        d="M11.53 1.5 3.1 9.93a.66.66 0 0 0 0 .94l3.9 3.9 4.53-4.53 4.53 4.53 3.9-3.9a.66.66 0 0 0 0-.94z"
        fill="#2684ff"
      />
      <path
        d="M11.53 22.5 19.96 14.07a.66.66 0 0 0 0-.94l-3.9-3.9-4.53 4.53L7 9.23l-3.9 3.9a.66.66 0 0 0 0 .94z"
        fill="#2684ff"
        opacity=".45"
      />
    </svg>
  )
}

const NAMES: Record<AccountKind, string> = { github: 'GitHub', jira: 'Jira' }

function pressable(onPress: () => void) {
  return (e: KeyboardEvent<HTMLElement>) => {
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault()
      onPress()
    }
  }
}

function Verb({
  children,
  warm,
  onClick,
}: {
  children: ReactNode
  warm?: boolean
  onClick: () => void
}) {
  return (
    <span
      role="button"
      tabIndex={0}
      className="ac-verb"
      data-tone={warm ? 'warm' : undefined}
      onClick={onClick}
      onKeyDown={pressable(onClick)}
    >
      {children}
    </span>
  )
}

function Field({
  label,
  value,
  onChange,
  placeholder,
  type = 'text',
  width,
  autoFocus,
  onEnter,
  invalid,
}: {
  label: string
  value: string
  onChange: (v: string) => void
  placeholder: string
  type?: 'text' | 'password' | 'email'
  width?: number
  autoFocus?: boolean
  onEnter?: () => void
  invalid?: boolean
}) {
  return (
    <label
      className="ac-field"
      data-fixed={width ? '' : undefined}
      style={width ? ({ '--ac-field-w': width + 'px' } as CSSProperties) : undefined}
    >
      <span className="ac-field-label">{label}</span>
      <input
        className="ac-input"
        type={type}
        value={value}
        autoFocus={autoFocus}
        autoComplete="off"
        placeholder={placeholder}
        aria-invalid={invalid || undefined}
        onChange={(e) => onChange(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter' && onEnter) {
            e.preventDefault()
            onEnter()
          }
        }}
      />
    </label>
  )
}

// The body of an open band, by the org's access method for that system — the
// reader never chooses. The token path under a Connect is a link, not a second
// button: it exists for the reader whose browser cannot complete the redirect.
function Band({
  acct,
  onConnect,
  onVerify,
  onDone,
}: {
  acct: Account
  onConnect?: (id: string) => void
  onVerify?: AccountsProps['onVerify']
  onDone: (account: string | null) => void
}) {
  const [alt, setAlt] = useState(false)
  const [a, setA] = useState('')
  const [b, setB] = useState('')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  const method = acct.method || (acct.kind === 'github' ? 'pat' : 'cloud')
  const hasConnect = method === 'app' || method === 'oauth'
  const showFields = !hasConnect || alt
  const cloud = acct.kind === 'jira' && method !== 'dc'
  const twoFields = showFields && cloud
  const ready = twoFields ? !!(a.trim() && b.trim()) : !!a.trim()

  const verify = async () => {
    if (!ready || busy) return
    setBusy(true)
    setErr(null)
    try {
      const fields = twoFields ? { email: a.trim(), token: b.trim() } : { token: a.trim() }
      const account = onVerify ? await onVerify(acct.id, fields) : null
      onDone(account)
    } catch (e) {
      // The server's own words, under the field; the field keeps its contents.
      setErr(e instanceof Error && e.message ? e.message : 'the credential was refused')
    } finally {
      setBusy(false)
    }
  }

  const host = acct.host || (acct.kind === 'github' ? 'github.com' : 'Jira')
  const hint = acct.hint
    ? acct.hint
    : acct.kind === 'github'
      ? hasConnect && !alt
        ? `You will be sent to ${host} and back. Triage Factory reads your username and verified email; it keeps no token and gains no access to your repositories.`
        : 'The token is used once, to read which account it belongs to, and discarded. Any scope will do.'
      : hasConnect && !alt
        ? `Triage Factory acts as you on ${host}, so the tickets it claims and updates are attributed to you. The credential is stored in the workspace’s secret store and never shared.`
        : cloud
          ? 'Create a token at id.atlassian.com → Security → API tokens. It is stored so the factory can act as you.'
          : 'A personal access token from your Jira site. It is stored so the factory can act as you.'

  return (
    <div className="ac-form">
      {!showFields && (
        <div className="ac-connect">
          <button
            type="button"
            className="ac-btn"
            data-primary=""
            onClick={() => onConnect?.(acct.id)}
          >
            {acct.kind === 'github' && (
              <AccountMark kind="github" size={13} color="var(--color-warm)" />
            )}
            {acct.kind === 'github' ? 'Continue with GitHub' : 'Continue with Atlassian'}
          </button>
          <span className="ac-or">or</span>
          <span
            role="button"
            tabIndex={0}
            className="ac-link"
            onClick={() => setAlt(true)}
            onKeyDown={pressable(() => setAlt(true))}
          >
            {acct.kind === 'github' ? 'paste a personal access token' : 'use an API token'}
          </span>
        </div>
      )}
      {showFields && (
        <div className="ac-fields">
          {twoFields ? (
            <>
              <Field
                label="ATLASSIAN ACCOUNT EMAIL"
                type="email"
                value={a}
                onChange={(v) => {
                  setA(v)
                  setErr(null)
                }}
                placeholder="you@yourcompany.com"
                width={220}
                autoFocus
                invalid={!!err}
              />
              <Field
                label="API TOKEN"
                type="password"
                value={b}
                onChange={(v) => {
                  setB(v)
                  setErr(null)
                }}
                placeholder="Your Atlassian API token"
                onEnter={verify}
                invalid={!!err}
              />
            </>
          ) : (
            <Field
              label={
                acct.kind === 'github'
                  ? `PERSONAL ACCESS TOKEN · ${host.toUpperCase()}`
                  : 'PERSONAL ACCESS TOKEN'
              }
              type="password"
              value={a}
              onChange={(v) => {
                setA(v)
                setErr(null)
              }}
              placeholder={acct.kind === 'github' ? 'ghp_…' : 'Your Jira token'}
              autoFocus
              onEnter={verify}
              invalid={!!err}
            />
          )}
          <button type="button" className="ac-btn" onClick={verify} disabled={busy || !ready}>
            {busy ? 'Verifying…' : 'Verify'}
          </button>
        </div>
      )}
      {err && <span className="ac-err">{err}</span>}
      <span className="ac-hint">{hint}</span>
    </div>
  )
}

function Row({
  acct,
  interactive,
  open,
  onOpen,
  onClose,
  onConnect,
  onVerify,
  onChanged,
}: {
  acct: Account & { changed?: boolean }
  interactive: boolean
  open: boolean
  onOpen: () => void
  onClose: () => void
  onConnect?: (id: string) => void
  onVerify?: AccountsProps['onVerify']
  onChanged: (id: string, account: string | null) => void
}) {
  const name = acct.name || NAMES[acct.kind] || acct.kind
  const connected = !!acct.account
  const verb = connected ? (acct.kind === 'github' ? 'Change' : 'Reconnect') : 'Connect'
  const host = acct.host || (acct.kind === 'github' ? 'github.com' : '')
  const value = connected
    ? acct.account + (host ? ' · ' + host : '')
    : host
      ? `not connected for ${host}`
      : 'not connected'
  const line = (
    <div className="ac-line">
      <span className="ac-name">
        <AccountMark kind={acct.kind} />
        {name}
      </span>
      {/* Keyed by the value: a change remounts the node and replays ac-tick
          once. The value changing and the mark playing are the same event. */}
      <span
        key={value}
        className={'ac-value' + (acct.changed ? ' ac-tick' : '')}
        data-absent={connected ? undefined : ''}
      >
        {value}
      </span>
      {interactive ? (
        open ? (
          <Verb onClick={onClose}>Cancel</Verb>
        ) : (
          <Verb warm={!connected} onClick={onOpen}>
            {verb}
          </Verb>
        )
      ) : (
        <span />
      )}
    </div>
  )
  if (!open) return <div className="ac-row">{line}</div>
  return (
    <div className="ac-row ac-band">
      <span aria-hidden="true" className="ac-spine" />
      <span aria-hidden="true" className="ac-sweep" />
      {line}
      <div className="ac-body">
        <span />
        <Band
          acct={acct}
          onConnect={onConnect}
          onVerify={onVerify}
          onDone={(account) => {
            onChanged(acct.id, account)
            onClose()
          }}
        />
      </div>
    </div>
  )
}

// The component draws its own layout in hairlines while data lands: three
// outlined bars at the row's real proportions, so loading is legible and the
// section holds its height.
function SkeletonRow({ i }: { i: number }) {
  const bar = (w: number, d: number) => (
    <span
      className="ac-skel-bar"
      style={{ '--ac-bar-w': w + 'px', '--ac-bar-d': d + 's' } as CSSProperties}
    />
  )
  return (
    <div aria-hidden="true" className="ac-skel">
      <span className="ac-name">
        <span className="ac-skel-disc" />
        {bar(i ? 42 : 58, i * 0.12)}
      </span>
      {bar(i ? 260 : 190, i * 0.12 + 0.06)}
      <span className="ac-skel-end">{bar(48, i * 0.12 + 0.12)}</span>
    </div>
  )
}

export function Accounts({
  accounts,
  loading = false,
  offline = false,
  interactive = true,
  label = 'ACCOUNTS',
  note = 'who the factory is, when it acts for you',
  onConnect,
  onVerify,
  onChange,
  className = '',
  style,
}: AccountsProps) {
  const [openId, setOpenId] = useState<string | null>(null)
  // A verify that landed: the value the row shows until the caller's own
  // read catches up, marked once as changed.
  const [local, setLocal] = useState<Record<string, { account: string }>>({})

  useEffect(() => {
    if (openId == null) return
    const onKey = (e: globalThis.KeyboardEvent) => {
      if (e.defaultPrevented || e.key !== 'Escape') return
      e.preventDefault()
      setOpenId(null)
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [openId])

  const rows = accounts.map((a) => {
    const l = local[a.id]
    return l ? { ...a, account: l.account, changed: true } : a
  })

  return (
    <div
      className={('ac ' + className).trim()}
      aria-busy={loading || undefined}
      data-offline={offline ? '' : undefined}
      style={style}
    >
      <div className="ac-head">
        <span className="ac-label">{label}</span>
        <span className="ac-note">{offline ? '--' : note}</span>
      </div>
      <div className="ac-list">
        {loading
          ? [0, 1].map((i) => <SkeletonRow key={i} i={i} />)
          : offline
            ? rows.map((a) => (
                <div key={a.id} className="ac-row">
                  <div className="ac-line">
                    <span className="ac-name">
                      <AccountMark kind={a.kind} />
                      {a.name || NAMES[a.kind]}
                    </span>
                    <span className="ac-value" data-absent="">
                      --
                    </span>
                    <span />
                  </div>
                </div>
              ))
            : rows.map((a) => (
                <Row
                  key={a.id}
                  acct={a}
                  interactive={interactive}
                  open={openId === a.id}
                  onOpen={() => setOpenId(a.id)}
                  onClose={() => setOpenId(null)}
                  onConnect={onConnect}
                  onVerify={onVerify}
                  onChanged={(id, account) => {
                    if (account) {
                      setLocal((m) => ({ ...m, [id]: { account } }))
                      onChange?.(id, account)
                    }
                  }}
                />
              ))}
      </div>
    </div>
  )
}

export default Accounts
