import { useCallback, useEffect, useMemo, useState } from 'react'
import type { CSSProperties, KeyboardEvent, ReactNode } from 'react'
import { useAuth } from '../../contexts/AuthContext'
import { useActiveOrgId } from '../../contexts/OrgContext'
import { useGitHubIdentity } from '../../hooks/useGitHubIdentity'
import { useJiraIdentity } from '../../hooks/useJiraIdentity'
import { useTokens } from '../../hooks/useTokens'
import { apiJSON, httpErrorMessage } from '../../lib/apiClient'
import { captureGitHubIdentityPat } from '../../lib/githubIdentity'
import { captureJiraIdentityApiToken, captureJiraIdentityPat } from '../../lib/jiraIdentity'
import { getStoredTheme, setTheme, useEffectiveTheme } from '../../lib/theme'
import type { ApiToken, ApiTokenCreated, IdentitiesResponse, LoginMethod } from '../../types'
import { Accounts } from '../../ui/accounts/Accounts'
import type { Account } from '../../ui/accounts/Accounts'
import { Dialog } from '../../ui/dialog/Dialog'
import { Segmented } from '../../ui/segmented/Segmented'
import { TitleField } from '../../ui/shell/TitleField'
import { Table } from '../../ui/table/Table'
import type { TableColumn, TableRow } from '../../ui/table/Table'
import { ago } from '../../ui/table/cells'
import {
  expiresAtFor,
  expiry,
  isoDate,
  MAX_CIDRS,
  minutesSince,
  presetOff,
  PRESETS,
  shortDate,
  validCidr,
} from './tokenMath'
import './usersettings.css'

// The multi-mode personal settings page: who you are here, the accounts the
// factory acts through as you, your appearance setting, and your API tokens.
// One column, three regions and one that takes the slack (the tokens table).
// Local mode never mounts this — /settings there is the Org / Team / User
// stack, and the token routes 404. See the design record in the handoff
// bundle (`docs/user-settings-multi.md`) for the decisions.

const GH_PATH =
  'M12 .5C5.73.5.5 5.73.5 12c0 5.02 3.29 9.28 7.86 10.74.58.11.79-.25.79-.55v-2.1c-3.2.7-3.88-1.54-3.88-1.54-.53-1.34-1.29-1.7-1.29-1.7-1.05-.72.08-.7.08-.7 1.16.08 1.77 1.19 1.77 1.19 1.03 1.77 2.7 1.26 3.36.96.1-.75.4-1.26.73-1.55-2.55-.29-5.23-1.28-5.23-5.7 0-1.26.45-2.29 1.19-3.1-.12-.29-.52-1.46.11-3.05 0 0 .96-.31 3.15 1.18a10.9 10.9 0 0 1 5.74 0c2.19-1.49 3.15-1.18 3.15-1.18.63 1.59.23 2.76.11 3.05.74.81 1.19 1.84 1.19 3.1 0 4.43-2.69 5.4-5.25 5.69.41.36.78 1.06.78 2.14v3.17c0 .3.21.67.8.55C20.71 21.28 24 17.02 24 12 24 5.73 18.77.5 12 .5z'

/** The identity-provider product's name, from the connection's `idp`. A
 *  product the deployment does not name falls back to the protocol. */
const IDP_NAMES: Record<string, string> = {
  entra: 'Microsoft Entra',
  okta: 'Okta',
  google: 'Google',
  onelogin: 'OneLogin',
  ping: 'Ping',
}
const IDP_MARKS = new Set(['entra', 'okta', 'google'])

/** The mark beside a login identity — the vendor's, at 12px. */
function DoorMark({ m, dim }: { m: LoginMethod; dim?: boolean }) {
  const theme = useEffectiveTheme()
  if (m.provider === 'github') {
    return (
      <svg
        className="us-doormark"
        width={12}
        height={12}
        viewBox="0 0 24 24"
        fill={dim ? 'var(--color-ink-4)' : 'var(--color-ink-3)'}
        aria-hidden="true"
      >
        <path d={GH_PATH} />
      </svg>
    )
  }
  const idp = m.idp || ''
  if (!IDP_MARKS.has(idp)) return null
  // Okta ships a dark-ground variant, so it follows the theme; the others are
  // legible on both grounds.
  const file = idp === 'okta' && theme === 'dark' ? 'okta-dark' : idp
  return (
    <img
      className="us-doormark"
      src={`/idp/${file}.svg`}
      width={12}
      height={12}
      alt={IDP_NAMES[idp]}
    />
  )
}

/** "GitHub @aallchin" / "Microsoft Entra aidan@allchin.com" / "SSO aidan@…". */
function doorWho(m: LoginMethod): string {
  if (m.provider === 'github') return 'GitHub ' + (m.login ? '@' + m.login : m.email || '')
  const name = IDP_NAMES[m.idp || ''] || 'SSO'
  return name + (m.email ? ' ' + m.email : '')
}

type TokenRow = TableRow & {
  id: string
  name: string
  prefix: string
  orgId: string
  orgName: string
  lastUsedMin: number | null
  expDays: number
  expText: string
  token: ApiToken
}

const cellStyle = (extra: CSSProperties): CSSProperties => ({
  font: '400 var(--text-reported)/1.5 var(--font-mono)',
  ...extra,
})

export default function UserSettingsPage() {
  const { me, refresh: refreshMe } = useAuth()
  const orgId = useActiveOrgId()
  const orgs = useMemo(() => me?.orgs ?? [], [me])
  // Names keyed on what they are, not on the memberships array's identity:
  // /api/me is re-read after a verify, and rows derived from a fresh array
  // of the same names would hand the Table new rows to reset on.
  const orgsKey = orgs.map((o) => o.id + ':' + o.name).join('|')
  // eslint-disable-next-line react-hooks/exhaustive-deps -- orgsKey is the content of orgs
  const orgNames = useMemo(() => new Map(orgs.map((o) => [o.id, o.name])), [orgsKey])
  const orgName = useCallback((id: string) => orgNames.get(id) ?? '', [orgNames])
  const multiOrg = orgs.length > 1

  // ---- header: the login identities ----
  const [methods, setMethods] = useState<LoginMethod[] | null>(null)
  useEffect(() => {
    let cancelled = false
    apiJSON<IdentitiesResponse>('/api/me/identities').then(
      (r) => {
        if (!cancelled) setMethods(r.methods)
      },
      () => {
        if (!cancelled) setMethods([])
      },
    )
    return () => {
      cancelled = true
    }
  }, [])
  const current = methods?.find((m) => m.current) ?? null
  const others = (methods ?? []).filter((m) => !m.current)

  // ---- accounts: the integration identities for the active org's hosts ----
  const gh = useGitHubIdentity(orgId)
  const jira = useJiraIdentity(orgId)
  const accounts = useMemo<Account[]>(() => {
    const list: Account[] = []
    const g = gh.state.status === 'ready' ? gh.state.data : null
    list.push({
      id: 'gh',
      kind: 'github',
      account: g?.connected && g.login ? '@' + g.login : null,
      host: g?.host || 'github.com',
      method: g?.connect_available ? 'app' : 'pat',
    })
    const j = jira.state.status === 'ready' ? jira.state.data : null
    // Only when the org has a Jira host: a system the org does not reach has
    // no account to hold.
    if (j?.host) {
      list.push({
        id: 'jira',
        kind: 'jira',
        account: j.connected && j.account ? j.account : null,
        host: j.host,
        method: j.connect_available ? 'oauth' : j.deployment === 'data_center' ? 'dc' : 'cloud',
      })
    }
    return list
  }, [gh.state, jira.state])
  const accountsLoading = gh.state.status === 'loading' || jira.state.status === 'loading'
  const accountsOffline = gh.state.status === 'error' || jira.state.status === 'error'

  const onConnect = (id: string) => {
    if (!orgId) return
    window.location.href =
      '/api/orgs/' +
      encodeURIComponent(orgId) +
      (id === 'gh' ? '/github' : '/jira') +
      '/connect/start?return_to=' +
      encodeURIComponent('/settings')
  }
  const onVerify = async (id: string, f: { token: string; email?: string }) => {
    if (!orgId) throw new Error('No active organization.')
    if (id === 'gh') {
      const got = await captureGitHubIdentityPat(orgId, f.token)
      gh.refresh()
      void refreshMe()
      return '@' + got.login
    }
    const cloud = jira.state.status === 'ready' && jira.state.data.deployment !== 'data_center'
    const got = cloud
      ? await captureJiraIdentityApiToken(orgId, f.email ?? '', f.token)
      : await captureJiraIdentityPat(orgId, f.token)
    jira.refresh()
    void refreshMe()
    return got.account
  }

  // ---- appearance ----
  const [themeLabel, setThemeLabel] = useState(() => {
    const t = getStoredTheme()
    return t === 'auto' ? 'system' : t
  })
  const pickTheme = (t: string) => {
    setThemeLabel(t)
    setTheme(t === 'system' ? 'auto' : t === 'dark' ? 'dark' : 'light')
  }

  // ---- tokens ----
  const tok = useTokens(true, orgs)
  // The clock every age and expiry on the page is measured against. Taken
  // when the tokens arrive rather than on every render: the table's rows
  // derive from it, and the Table resets its working set — selection, the
  // undo window — whenever the rows it is handed change identity.
  // eslint-disable-next-line react-hooks/exhaustive-deps
  const now = useMemo(() => Date.now(), [tok.tokens])
  const [tokensOpen, setTokensOpen] = useState(false)
  const [detail, setDetail] = useState<string | null>(null)
  const [drafting, setDrafting] = useState(false)
  const [secret, setSecret] = useState<{ made: ApiTokenCreated; cap: number | null } | null>(null)
  const [copied, setCopied] = useState(false)
  // A mutation the server refused, in its words: shown above the table (a
  // bulk revoke) or in the sheet (its own revoke or rename). The table's
  // optimistic drop is undone by a re-read, so a token the server kept is
  // never a token the page has lost.
  const [tableErr, setTableErr] = useState('')
  const [sheetErr, setSheetErr] = useState('')

  const rows = useMemo<TokenRow[]>(
    () =>
      tok.tokens.map((t) => {
        const e = expiry(t, now)
        return {
          id: t.id,
          name: t.name,
          prefix: t.token_prefix,
          orgId: t.org_id,
          orgName: orgName(t.org_id),
          lastUsedMin: t.last_used_at ? minutesSince(t.last_used_at, now) : null,
          expDays: e.days,
          expText: e.text,
          token: t,
        }
      }),
    [tok.tokens, orgName, now],
  )
  const soonest = rows
    .filter((r) => r.expDays !== Infinity)
    .sort((a, b) => a.expDays - b.expDays)[0]
  const soonDays = soonest ? soonest.expDays : null
  const count = rows.length

  const cols = useMemo<TableColumn[]>(() => {
    const c: TableColumn[] = [
      {
        key: 'name',
        label: 'NAME',
        mono: false,
        width: 'minmax(0,1fr)',
        measure: (r) => String(r.name),
        render: (r) => <span className="us-tokname">{String(r.name)}</span>,
      },
      {
        key: 'prefix',
        label: 'TOKEN',
        mono: true,
        render: (r) => r.prefix + '…',
        measure: (r) => r.prefix + '…',
        color: () => 'var(--color-ink-3)',
      },
    ]
    // A constant column measures nothing: the org is drawn only when the
    // viewer holds more than one membership.
    if (multiOrg)
      c.push({ key: 'orgName', label: 'ORGANIZATION', color: () => 'var(--color-ink-2)' })
    c.push(
      {
        key: 'lastUsedMin',
        label: 'LAST USED',
        align: 'end',
        render: (r) => (r.lastUsedMin == null ? 'never' : ago(r.lastUsedMin as number)),
        sortValue: (r) => (r.lastUsedMin == null ? 1e12 : (r.lastUsedMin as number)),
        color: (r) => (r.lastUsedMin == null ? 'var(--color-ink-4)' : 'var(--color-ink-2)'),
      },
      {
        key: 'expires',
        label: 'EXPIRES',
        align: 'end',
        sortValue: (r) => r.expDays as number,
        measure: (r) => String(r.expText),
        // The cap's part in the date is the sheet's to explain, not the row's.
        render: (r) => {
          const d = r.expDays as number
          return (
            <span
              style={cellStyle({
                color:
                  d <= 7
                    ? 'var(--color-warm)'
                    : d === Infinity
                      ? 'var(--color-ink-4)'
                      : 'var(--color-ink-2)',
              })}
            >
              {String(r.expText)}
            </span>
          )
        },
      },
      {
        key: 'open',
        label: '',
        width: '14px',
        sortable: false,
        measure: () => '›',
        render: () => <span aria-hidden="true" className="us-rowchev" />,
      },
    )
    return c
  }, [multiOrg])

  const capParts = orgs
    .filter((o) => o.id in tok.caps)
    .map((o) => {
      const cap = tok.caps[o.id]
      return o.name + (cap ? ' caps tokens at ' + cap + ' days' : ' sets no cap')
    })

  // ---- the sheet ----
  const det = detail ? (rows.find((r) => r.id === detail) ?? null) : null
  const sheet = det
    ? (() => {
        const t = det.token
        const cap = tok.caps[t.org_id] ?? null
        const org = orgName(t.org_id)
        const created = new Date(t.created_at).getTime()
        const createdDays = Math.floor((now - created) / 86_400_000)
        const e = expiry(t, now)
        const eff = t.effective_expires_at
        const expNote =
          eff == null
            ? 'No expiry, and ' + org + ' sets no cap.'
            : e.orgLimit
              ? 'Expires by ' +
                org +
                '’s ' +
                cap +
                '-day cap' +
                (t.expires_at
                  ? ', not the ' + shortDate(t.expires_at, now) + ' you set.'
                  : '; no expiry of your own was set.')
              : 'Expires on the date you set.' +
                (cap ? ' ' + org + ' caps tokens at ' + cap + ' days, which this is within.' : '')
        return {
          t,
          org,
          createdN: createdDays === 0 ? 'now' : createdDays + 'd',
          createdL: createdDays === 0 ? 'created' : 'old · since ' + shortDate(created, now),
          usedN: t.last_used_at == null ? '—' : ago(minutesSince(t.last_used_at, now)),
          usedL: t.last_used_at == null ? 'never used' : 'since last use',
          expN: eff == null ? '∞' : e.days <= 0 ? '0' : e.days + 'd',
          expL: eff == null ? 'no expiry' : 'left · expires ' + shortDate(eff, now),
          expWarm: eff != null && e.days <= 7,
          rangesN: t.allowed_cidrs.length + ' of ' + MAX_CIDRS,
          note:
            expNote +
            (t.allowed_cidrs.length
              ? ' A request from any other address fails as an invalid token would.'
              : ''),
        }
      })()
    : null

  const renameDetail = async (name: string) => {
    if (!det) return
    setSheetErr('')
    try {
      await tok.rename(det.id, name)
    } catch (err) {
      // The sheet keeps the stored name, and says why the new one did not take.
      setSheetErr(httpErrorMessage(err, 'Could not rename the token.'))
    }
  }
  const revokeDetail = async () => {
    if (!det) return
    setSheetErr('')
    try {
      await tok.revoke(det.id)
      setDetail(null)
    } catch (err) {
      // The sheet stays open on the token that is still alive.
      setSheetErr(httpErrorMessage(err, 'Could not revoke the token.'))
    }
  }
  const openDetail = (id: string) => {
    setSheetErr('')
    setDetail(id)
  }

  // ---- the draft ----
  const [name, setName] = useState('')
  const [org, setOrg] = useState<string | null>(null)
  const [exp, setExp] = useState('30')
  const [custom, setCustom] = useState('')
  const [cidrs, setCidrs] = useState<string[]>([])
  const [cidrIn, setCidrIn] = useState('')
  const [cidrErr, setCidrErr] = useState<string | null>(null)
  const [createError, setCreateError] = useState<string | null>(null)
  const [creating, setCreating] = useState(false)

  const openDraft = () => {
    setName('')
    setOrg(orgs.length === 1 ? orgs[0].id : null)
    setExp('30')
    setCustom('')
    setCidrs([])
    setCidrIn('')
    setCidrErr(null)
    setCreateError(null)
    setDrafting(true)
  }
  const leaveDraft = () => setDrafting(false)

  const addCidr = () => {
    const v = cidrIn.trim()
    if (!v) return
    if (!validCidr(v)) {
      setCidrErr(v + ' is not a CIDR range (10.4.0.0/16, 2600:1f18::/32)')
      return
    }
    if (cidrs.includes(v)) {
      setCidrIn('')
      setCidrErr(null)
      return
    }
    if (cidrs.length >= MAX_CIDRS) {
      setCidrErr('at most ' + MAX_CIDRS + ' ranges')
      return
    }
    setCidrs(cidrs.concat(v))
    setCidrIn('')
    setCidrErr(null)
  }
  const cidrKey = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'Enter' || e.key === ',' || e.key === ' ') {
      e.preventDefault()
      addCidr()
    } else if (e.key === 'Backspace' && !cidrIn && cidrs.length) {
      setCidrs(cidrs.slice(0, -1))
    }
  }

  // Three answers, not two: the org caps at N, sets no cap, or its policy
  // could not be read. Unknown strikes nothing and pre-checks nothing — a
  // struck preset is a promise about the 422, and the 422 is the enforcement.
  const capKnown = !!org && org in tok.caps
  const cap = org && capKnown ? tok.caps[org] : null
  const pickOrg = (id: string) => {
    setOrg(id)
    setCreateError(null)
    // The preset picked under the last org may be past this one's cap; it
    // moves to the first preset that fits rather than sitting struck and
    // chosen at once.
    const nextCap = id in tok.caps ? tok.caps[id] : null
    const chosen = PRESETS.find((p) => p.id === exp)
    if (chosen && presetOff(chosen, nextCap)) {
      const first = PRESETS.find((p) => !presetOff(p, nextCap))
      if (first) setExp(first.id)
    }
  }
  const create = async () => {
    if (creating) return
    // A range half-typed is added, not dropped: the dialog's Enter is the
    // create, and Enter in the ALLOWED FROM field means "add this one" — so a
    // pending entry is always taken before anything is sent.
    if (cidrIn.trim()) {
      addCidr()
      return
    }
    const n = name.trim()
    if (!n) return setCreateError('a name is required')
    if (!org) return setCreateError('pick the organization it acts in')
    const at = expiresAtFor(exp, custom, now)
    if (at === undefined) return setCreateError('pick a date')
    if (cap && (at === null || (new Date(at).getTime() - now) / 86_400_000 > cap))
      return setCreateError(orgName(org) + ' caps tokens at ' + cap + ' days')
    setCreating(true)
    setCreateError(null)
    try {
      const made = await tok.create({
        name: n,
        org_id: org,
        ...(at ? { expires_at: at } : {}),
        ...(cidrs.length ? { allowed_cidrs: cidrs } : {}),
      })
      setDrafting(false)
      setSecret({ made, cap })
      setCopied(false)
    } catch (err) {
      // The 409 token-limit and 422 cap messages, verbatim.
      setCreateError(httpErrorMessage(err, 'Could not create the token.'))
    } finally {
      setCreating(false)
    }
  }

  const copySecret = () => {
    const t = secret?.made.token
    if (!t) return
    const done = () => setCopied(true)
    if (navigator.clipboard?.writeText) navigator.clipboard.writeText(t).then(done, done)
    else done()
  }
  const closeSecret = () => {
    setSecret(null)
    setCopied(false)
  }

  const chip = (
    on: boolean,
    off: boolean,
    label: string,
    onPick: (() => void) | null,
    why?: string,
  ) => (
    <span
      key={label}
      className="us-chip"
      data-on={on || undefined}
      data-off={off || undefined}
      title={why || undefined}
      role="button"
      tabIndex={off ? -1 : 0}
      aria-pressed={on}
      aria-disabled={off || undefined}
      onClick={onPick ?? undefined}
      onKeyDown={(e) => {
        if (onPick && (e.key === 'Enter' || e.key === ' ')) {
          e.preventDefault()
          onPick()
        }
      }}
    >
      {label}
    </span>
  )

  const custDays =
    exp === 'custom' && custom
      ? (new Date(custom + 'T00:00:00').getTime() - now) / 86_400_000
      : null
  const custBad = exp === 'custom' && !!cap && custDays != null && custDays > cap
  const expHint = !org
    ? 'presets are display math; the request carries an absolute date'
    : !capKnown
      ? orgName(org) + '’s cap could not be read — a date past it is refused when you create'
      : cap
        ? orgName(org) +
          ' caps tokens at ' +
          cap +
          ' days' +
          (custBad ? ' — that date is past it' : '')
        : orgName(org) + ' sets no cap'

  const sec = secret
  const secretCons: string[] = sec
    ? [
        'Acts as you inside ' + orgName(sec.made.org_id) + ', and nowhere else',
        sec.made.expires_at == null
          ? sec.cap
            ? 'Expires in ' + sec.cap + ' days (org limit)'
            : 'Never expires'
          : 'Expires in ' +
            Math.ceil((new Date(sec.made.expires_at).getTime() - now) / 86_400_000) +
            ' days',
        sec.made.allowed_cidrs.length
          ? 'Accepted from ' +
            sec.made.allowed_cidrs.length +
            (sec.made.allowed_cidrs.length === 1 ? ' IP range' : ' IP ranges')
          : 'Accepted from any address',
      ]
    : []

  const headTail: ReactNode = tokensOpen
    ? 'a token acts as you inside one organization and nowhere else'
    : soonDays == null
      ? count
        ? 'none expire'
        : 'headless access to the API, as you'
      : 'next expiry ' + (soonDays <= 0 ? 'passed' : 'in ' + soonDays + 'd')
  const tailWarm = !tokensOpen && soonDays != null && soonDays <= 7

  return (
    <div className="us">
      <div className="us-top">
        <header className="us-head">
          <div className="us-who">
            <span className="us-name">{me?.display_name || me?.email || ''}</span>
            {current ? (
              <span className="us-door">
                <DoorMark m={current} />
                <span className="us-doorline">
                  {doorWho(current)}
                  {current.linked_at ? ' · since ' + shortDate(current.linked_at, now) : ''}
                </span>
              </span>
            ) : null}
            {others.map((m, i) => (
              <span className="us-door" data-also="" key={i}>
                <DoorMark m={m} dim />
                <span className="us-doorline">also signs in with {doorWho(m)}</span>
              </span>
            ))}
          </div>
        </header>

        <div className="us-accounts">
          <Accounts
            accounts={accounts}
            loading={accountsLoading}
            offline={accountsOffline}
            onConnect={onConnect}
            onVerify={onVerify}
          />
        </div>

        <div className="us-appearance">
          <span className="us-seclabel">APPEARANCE</span>
          <span className="us-spacer" />
          <Segmented
            variant="spine"
            options={['light', 'dark', 'system']}
            value={themeLabel}
            onChange={pickTheme}
            label="Appearance"
          />
        </div>
      </div>

      <div className="us-tokens">
        <div
          className="us-tokhead"
          role="button"
          tabIndex={0}
          aria-expanded={tokensOpen}
          onClick={() => setTokensOpen((o) => !o)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' || e.key === ' ') {
              e.preventDefault()
              setTokensOpen((o) => !o)
            }
          }}
        >
          <span className="us-seclabel" data-open={tokensOpen || undefined}>
            API TOKENS
          </span>
          <span className="us-tokcount">
            {tok.loading ? '' : count === 0 ? 'none' : count === 1 ? '1 token' : count + ' tokens'}
          </span>
          <span className="us-spacer" />
          <span className="us-toktail" data-tone={tailWarm ? 'warm' : undefined}>
            {tok.loading ? '' : headTail}
          </span>
          <span className="us-chev" data-open={tokensOpen || undefined} />
        </div>

        {tokensOpen ? (
          <>
            <div className="us-table">
              {tok.error || tableErr ? <p className="us-err">{tok.error || tableErr}</p> : null}
              <Table
                showHeader
                label=""
                columns={cols}
                rows={rows}
                pageSize={6}
                sortKey="lastUsedMin"
                sortDir={1}
                barPosition="absolute"
                add={{ label: 'new token', onSelect: openDraft }}
                bar={{
                  danger: {
                    label: 'Hold to revoke',
                    ms: 900,
                    action: {
                      id: 'revoke',
                      label: 'Hold to revoke',
                      message: (n) =>
                        n === 1
                          ? 'token revoked — anything using it stops working'
                          : n + ' tokens revoked — anything using them stops working',
                    },
                  },
                }}
                mutate={(row, id) => (id === 'revoke' ? null : row)}
                onCommit={(id, ids, ctx) => {
                  if (id !== 'revoke') return
                  setTableErr('')
                  for (const i of ids)
                    tok.revoke(String(i), { keepalive: ctx.reason === 'unload' }).catch((err) => {
                      // The row already left the table; the re-read brings a
                      // token the server kept back, with the refusal beside it.
                      setTableErr(httpErrorMessage(err, 'Could not revoke a token.'))
                      tok.reload()
                    })
                }}
                emptyLabel="No tokens. Everything you do in the browser uses your session instead."
                onRowOpen={(row) => openDetail(String(row.id))}
              />
            </div>
            <div className="us-tokfoot">
              <span className="us-rotate">
                To rotate, create a replacement first, move your automation to it, then revoke this
                one.
              </span>
              <span className="us-capnote">{capParts.join(' · ')}</span>
            </div>
          </>
        ) : null}
      </div>

      <Dialog
        open={drafting}
        build="none"
        width={520}
        title="New API token"
        body="It acts as you inside one organization and nowhere else. The token is shown once, when it is created."
        cancelLabel="Cancel"
        confirmLabel="Create token"
        onCancel={leaveDraft}
        onConfirm={() => void create()}
        note="Presets are display math; the request carries an absolute date. Nothing is sent until you create it."
      >
        <div className="us-draft">
          <div className="us-fieldset">
            <span className="us-flabel">NAME</span>
            <input
              className="us-input"
              autoFocus
              value={name}
              onChange={(e) => {
                setName(e.target.value)
                setCreateError(null)
              }}
              placeholder="what will use it — a CI job, a laptop, a hook"
              aria-label="Token name"
            />
            <span className="us-fhint">names need not be unique · a replacement may share one</span>
          </div>
          <div className="us-fieldset" data-gap="8">
            <span className="us-flabel">ORGANIZATION</span>
            <div className="us-chips">
              {orgs.map((o) => chip(org === o.id, false, o.name, () => pickOrg(o.id)))}
            </div>
            <span className="us-fhint">
              {!multiOrg
                ? 'your only organization'
                : org
                  ? 'fixed at creation · ' +
                    (orgs.find((o) => o.id === org)?.role ?? 'member') +
                    ' there'
                  : 'a token never spans organizations'}
            </span>
          </div>
          <div className="us-fieldset" data-gap="8">
            <span className="us-flabel">EXPIRES</span>
            <div className="us-chips">
              {PRESETS.map((p) => {
                const off = presetOff(p, cap)
                return chip(
                  exp === p.id,
                  off,
                  p.label,
                  off
                    ? null
                    : () => {
                        setExp(p.id)
                        setCreateError(null)
                      },
                  off ? orgName(org ?? '') + ' caps tokens at ' + cap + ' days' : undefined,
                )
              })}
              {exp === 'custom' ? (
                <input
                  className="us-input us-date"
                  type="date"
                  value={custom}
                  min={isoDate(now, 1)}
                  max={isoDate(now, cap ? cap : 3650)}
                  onChange={(e) => {
                    setCustom(e.target.value)
                    setCreateError(null)
                  }}
                  aria-label="Expiry date"
                />
              ) : null}
            </div>
            <span className="us-fhint" data-tone={custBad ? 'alarm' : undefined}>
              {expHint}
            </span>
          </div>
          <div className="us-fieldset" data-gap="8">
            <span className="us-flabel">ALLOWED FROM</span>
            <input
              className="us-input us-mono"
              value={cidrIn}
              onChange={(e) => {
                setCidrIn(e.target.value)
                setCidrErr(null)
              }}
              onKeyDown={cidrKey}
              placeholder={cidrs.length ? 'another · ↵' : 'optional · 10.4.0.0/16 · ↵ to add'}
              aria-label="Allowed IP range"
            />
            {cidrs.length ? (
              <div className="us-cidrs">
                {cidrs.map((c) => (
                  <div className="us-cidr" key={c}>
                    <span className="us-cidrtext">{c}</span>
                    <span
                      className="us-verb"
                      role="button"
                      tabIndex={0}
                      onClick={() => {
                        setCidrs(cidrs.filter((x) => x !== c))
                        setCidrErr(null)
                      }}
                      onKeyDown={(e) => {
                        if (e.key === 'Enter' || e.key === ' ') {
                          e.preventDefault()
                          setCidrs(cidrs.filter((x) => x !== c))
                          setCidrErr(null)
                        }
                      }}
                    >
                      Remove
                    </span>
                  </div>
                ))}
              </div>
            ) : null}
            <span className="us-fhint" data-tone={cidrErr ? 'alarm' : undefined}>
              {cidrErr ||
                (cidrs.length
                  ? cidrs.length +
                    ' of ' +
                    MAX_CIDRS +
                    ' · requests from elsewhere fail as an invalid token would'
                  : 'v4 and v6 · up to ' + MAX_CIDRS + ' · leave empty to accept any address')}
            </span>
          </div>
          {createError ? <span className="us-err">{createError}</span> : null}
        </div>
      </Dialog>

      <Dialog
        open={!!sheet}
        build="none"
        width={460}
        title={
          sheet ? (
            <div className="us-sheettitle">
              <TitleField title={sheet.t.name} onSave={(n) => void renameDetail(n)} />
            </div>
          ) : (
            ''
          )
        }
        cancelLabel="Close"
        confirmLabel="Hold to revoke"
        confirmHold={900}
        onCancel={() => setDetail(null)}
        onConfirm={() => void revokeDetail()}
        note={sheet?.note ?? ''}
      >
        {sheet ? (
          <div className="us-sheet">
            <div className="us-sheetline">
              <span>{sheet.t.token_prefix}…</span>
              <span className="us-spacer" />
              <span>{sheet.org}</span>
            </div>
            <div className="us-figs">
              <div className="us-fig">
                <span className="us-fign">{sheet.createdN}</span>
                <span className="us-figl">{sheet.createdL}</span>
              </div>
              <span className="us-figrule" />
              <div className="us-fig">
                <span className="us-fign">{sheet.usedN}</span>
                <span className="us-figl">{sheet.usedL}</span>
              </div>
              <span className="us-figrule" />
              <div className="us-fig">
                <span className="us-fign" data-tone={sheet.expWarm ? 'warm' : undefined}>
                  {sheet.expN}
                </span>
                <span className="us-figl">{sheet.expL}</span>
              </div>
            </div>
            {sheetErr ? (
              <span className="us-err" role="alert">
                {sheetErr}
              </span>
            ) : null}
            {sheet.t.allowed_cidrs.length ? (
              <div className="us-ranges">
                <div className="us-rangeshead">
                  <span className="us-flabel">ALLOWED FROM</span>
                  <span className="us-rangesn">{sheet.rangesN}</span>
                </div>
                <div className="us-rangegrid">
                  {sheet.t.allowed_cidrs.map((c) => (
                    <span key={c}>{c}</span>
                  ))}
                </div>
              </div>
            ) : (
              <span className="us-anyaddr">Allowed from any IP address</span>
            )}
          </div>
        ) : null}
      </Dialog>

      <Dialog
        open={!!sec}
        build="quiet"
        width={480}
        title="Token created"
        body={sec ? <span className="us-secret">{sec.made.token}</span> : ''}
        consequences={secretCons}
        note="Shown once. Close this without copying and the token is gone; you would create another."
        confirmLabel="I’ve saved it"
        cancelLabel={copied ? 'Copied' : 'Copy'}
        onConfirm={closeSecret}
        onCancel={copySecret}
      />
    </div>
  )
}
