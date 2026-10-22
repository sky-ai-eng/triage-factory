import { useCallback, useEffect, useId, useMemo, useRef, useState } from 'react'
import { X } from 'lucide-react'
import { useMe, useTeamMembers } from '../hooks/useDeploymentConfig'
import { useApiOrgId } from '../hooks/useApiOrgId'
import { findGitHubAccountByLogin, getGitHubAccount } from '../lib/githubAccounts'
import { findLinearUser, isLinearID, linearUserHandle, searchLinearUsers } from '../lib/linearUsers'
import type { MeResponse, TeamMember } from '../types'

/** IdentityListField is the editor for `author_id_in` / `reviewer_id_in` /
 *  `commenter_id_in` / `requested_user_id_in` (GitHub), `assignee_in` /
 *  `reporter_in` / `commenter_in` (Jira) and `assignee_in` / `creator_in` /
 *  `commenter_in` (Linear) predicate fields. Two variants based
 *  on the acting team's size, derived from the team roster
 *  (POST /api/teams/{team_id}/members/list):
 *
 *  - Variant A (solo team — local mode or a multi-mode team with one
 *    member): a single "Match my <actor>" toggle. ON →
 *    ["<current_user.<identityField>>"]. OFF → field omitted (no
 *    filter). Disabled when the identity field is missing with a
 *    configure-<source> hint.
 *
 *  - Variant B (multi-member team): chips + searchable dropdown of team
 *    members. Free-text entry supports external identities (bots,
 *    contractors not on the team). Implements the ARIA combobox
 *    pattern + keyboard navigation (Arrow keys, Enter, Escape).
 *
 *  identityKind selects which user-row column drives Variant A's
 *  toggle and Variant B's roster lookups: 'github' for GitHub account
 *  ids, 'jira' for Atlassian account IDs, 'linear' for Linear user IDs.
 *
 *  A GitHub field stores GitHub's account id, which a rename leaves alone,
 *  and is offered and shown by login: a member's id comes from the roster
 *  (and the viewer's from /api/me), and a typed login is looked up on the
 *  org's GitHub when it is added (GET /api/orgs/{org_id}/github/users/by-name/
 *  {login}), refused beside the field when no account holds it. A stored id
 *  no member holds is named by the login GitHub gives it now.
 *
 *  A Linear field stores Linear's user id. A typed name or email is searched
 *  for in the org's Linear workspace when it is added (POST /api/orgs/{org_id}/
 *  linear/users/list) and the people it matches are offered to pick from, an
 *  integration's app user marked as one and left out of an assignee filter; a
 *  typed id is read directly. A member is offered by the display name their
 *  binding recorded, and a stored id no member holds by the display name
 *  Linear gives it now.
 *
 *  Both variants serialize to the same wire shape: `string[] | undefined`. */
export type IdentityKind = 'github' | 'jira' | 'linear'

export default function IdentityListField({
  fieldName,
  identityKind,
  value,
  onChange,
}: {
  fieldName: string
  identityKind: IdentityKind
  value: string[] | undefined
  onChange: (val: string[] | undefined) => void
}) {
  const { me, loading: meLoading, error: meError } = useMe()
  const { members, loading: membersLoading, error: membersError } = useTeamMembers()
  const labels = labelsForField(fieldName, identityKind)
  const resolver = useIdentityResolver(
    identityKind,
    fieldName,
    !meLoading && !membersLoading,
    value,
    members,
  )

  if (meLoading || membersLoading) {
    return <div className="h-10 rounded-lg bg-tint-2 animate-pulse" />
  }

  // /api/me is the load-bearing call here — without it the editor has
  // no current_user identity to scope filters against. The roster is
  // best-effort: a missing one collapses to Variant A (toggle only),
  // which matches today's multi-mode degraded behavior.
  if (meError || !me) {
    return (
      <div
        role="alert"
        className="rounded-lg border border-alarm/40 bg-alarm/40 px-3 py-2 text-ui text-alarm"
      >
        Couldn’t load your identity{meError ? `: ${meError}` : '.'} Refresh the page to retry.
      </div>
    )
  }

  // Variant choice tracks the roster length. Variant B only renders
  // when the team has more than one member (you + at least one
  // teammate). Roster-fetch failures or single-member rosters fall to
  // Variant A — the conservative default.
  const hasMultiMemberTeam = !membersError && members.length > 1
  if (!hasMultiMemberTeam) {
    return (
      <VariantA
        value={value}
        onChange={onChange}
        me={me}
        labels={labels}
        identityKind={identityKind}
        resolver={resolver}
      />
    )
  }
  return (
    <VariantB
      value={value}
      onChange={onChange}
      members={members}
      labels={labels}
      identityKind={identityKind}
      resolver={resolver}
    />
  )
}

// SourceIdentity is where each identity kind lives on the current-user
// payload and the team roster, and what the copy calls it. One table so
// call sites never branch on the kind themselves.
interface SourceIdentity {
  /** The source's name in copy: "GitHub", "Jira", "Linear". */
  label: string
  /** What one identifier is called in copy. */
  idNoun: string
  /** The value the field stores for the viewer, from /api/me. */
  fromMe: (me: MeResponse) => string | undefined
  /** What a person knows the viewer's identity by, for hints. */
  displayFromMe: (me: MeResponse) => string | undefined
  /** The value the field stores for a roster row; null when the member
   *  hasn't connected, or (GitHub) their binding recorded no account id. */
  fromMember: (m: TeamMember) => string | null
  /** What a person knows a roster row's identity by — searched, shown, and
   *  for GitHub the login looked up when the row has no account id. */
  handleFromMember: (m: TeamMember) => string | null
}

const SOURCES: Record<IdentityKind, SourceIdentity> = {
  github: {
    label: 'GitHub',
    idNoun: 'login',
    fromMe: (me) => me.github_user_id,
    displayFromMe: (me) => me.github_username,
    fromMember: (m) => m.github_user_id,
    handleFromMember: (m) => m.github_username,
  },
  jira: {
    label: 'Jira',
    idNoun: 'account ID',
    fromMe: (me) => me.jira_account_id,
    displayFromMe: (me) => me.jira_display_name,
    fromMember: (m) => m.jira_account_id,
    handleFromMember: (m) => m.jira_account_id,
  },
  linear: {
    label: 'Linear',
    idNoun: 'name',
    fromMe: (me) => me.linear_user_id,
    displayFromMe: (me) => me.linear_display_name,
    fromMember: (m) => m.linear_user_id,
    handleFromMember: (m) => m.linear_display_name || m.linear_user_id,
  },
}

// --- Resolving what a person types into what the field stores ---------------

/** A person a typed name could mean, offered to pick from: the value the
 *  field would store, and the name and handle they are shown by. */
interface Choice {
  value: string
  label: string
  detail: string
}

/** What a typed name resolves to: the value to store, the people to pick from
 *  when it could mean several, or why there is nothing to store. */
type Resolution =
  | { kind: 'value'; value: string }
  | { kind: 'choices'; choices: Choice[] }
  | { kind: 'error'; error: string }

/** IdentityResolver turns what a person names an identity by into the value
 *  the field stores, and names a stored value back. Jira stores what was
 *  typed. GitHub stores the account id of the login typed. Linear offers the
 *  users a typed name matches, or reads a typed id. Both name a stored id by
 *  what the provider calls it now. */
interface IdentityResolver {
  resolve: (text: string) => Promise<Resolution>
  /** The handle a stored value is shown by when no roster row holds it. */
  handleOf: (value: string) => string
}

function useIdentityResolver(
  kind: IdentityKind,
  fieldName: string,
  ready: boolean,
  value: string[] | undefined,
  members: TeamMember[],
): IdentityResolver {
  const orgId = useApiOrgId()
  // An integration's app user creates issues and comments but is not who an
  // issue is assigned to, so an assignee filter is offered people only.
  const offersApps = fieldName !== 'assignee_in'
  // Names by stored id, the GitHub login or the Linear display name: the ones
  // learned in this session, and the ones read for stored ids no member holds.
  // null marks an id the provider did not name, so it is not asked for again.
  const [names, setNames] = useState<Record<string, string | null>>({})

  const resolve = useCallback(
    async (text: string): Promise<Resolution> => {
      const typed = text.trim()
      if (kind === 'jira') return { kind: 'value', value: typed }
      if (!orgId) return { kind: 'error', error: 'The workspace is still loading; try again.' }
      if (kind === 'github') {
        const found = await findGitHubAccountByLogin(orgId, typed)
        if (!found.ok) return { kind: 'error', error: found.error }
        const { github_user_id: id, login } = found.account
        setNames((prev) => ({ ...prev, [id]: login }))
        return { kind: 'value', value: id }
      }
      if (isLinearID(typed)) {
        const found = await findLinearUser(orgId, typed)
        if (!found.ok) return { kind: 'error', error: found.error }
        setNames((prev) => ({ ...prev, [found.user.id]: linearUserHandle(found.user) }))
        return { kind: 'value', value: found.user.id }
      }
      const found = await searchLinearUsers(orgId, typed)
      if (!found.ok) return { kind: 'error', error: found.error }
      const users = offersApps ? found.users : found.users.filter((u) => !u.app)
      if (users.length === 0) {
        return {
          kind: 'error',
          error:
            found.users.length > 0
              ? `Only Linear apps match “${typed}”, and this filter takes people.`
              : `No Linear user matches “${typed}”.`,
        }
      }
      setNames((prev) => ({
        ...prev,
        ...Object.fromEntries(users.map((u) => [u.id, linearUserHandle(u)])),
      }))
      return {
        kind: 'choices',
        choices: users.map((u) => ({
          value: u.id,
          label: u.name || u.display_name,
          detail: u.app ? `${u.display_name} · app` : u.display_name,
        })),
      }
    },
    [kind, orgId, offersApps],
  )

  const memberIDs = new Set(members.map((m) => SOURCES[kind].fromMember(m)).filter(Boolean))
  const unnamed =
    kind !== 'jira' && ready
      ? (value ?? []).filter((id) => !memberIDs.has(id) && !(id in names)).join(',')
      : ''

  useEffect(() => {
    if (!unnamed || !orgId) return
    let cancelled = false
    const nameOf = async (id: string): Promise<string | null> => {
      if (kind === 'github') return (await getGitHubAccount(orgId, id))?.login ?? null
      if (!isLinearID(id)) return null
      const found = await findLinearUser(orgId, id)
      return found.ok ? linearUserHandle(found.user) : null
    }
    Promise.all(unnamed.split(',').map(async (id) => [id, await nameOf(id)] as const)).then(
      (pairs) => {
        if (!cancelled) setNames((prev) => ({ ...prev, ...Object.fromEntries(pairs) }))
      },
    )
    return () => {
      cancelled = true
    }
  }, [unnamed, orgId, kind])

  return { resolve, handleOf: (v) => names[v] ?? v }
}

// --- Actor-specific copy ----------------------------------------------------

interface ActorLabels {
  /** "author" / "reviewer" / "commenter" */
  actor: string
  /** Variant A on-state label, e.g. "Match my PRs". */
  toggleOnLabel: string
  /** Variant A off-state label, e.g. "Match any author". */
  toggleOffLabel: string
  /** Variant A descriptive hint when ON, takes the captured login as a placeholder. */
  scopedHint: (login: string) => string
  /** Variant A descriptive hint when OFF. */
  unscopedHint: string
  /** Variant B placeholder shown in the search input. */
  searchPlaceholder: string
  /** Variant B caption shown beneath the chip area. */
  emptyHint: string
}

function labelsForField(fieldName: string, kind: IdentityKind): ActorLabels {
  // Strip the canonical "_id_in" / "_in" suffix to derive the actor name.
  // Falls back to the field name itself if neither is there — future
  // non-identity string_list predicates would hit that path and get
  // generic copy, which is still less wrong than mislabeling everything
  // as "author."
  const actor = fieldName.endsWith('_id_in')
    ? fieldName.slice(0, -'_id_in'.length)
    : fieldName.endsWith('_in')
      ? fieldName.slice(0, -'_in'.length)
      : fieldName
  const idLabel = SOURCES[kind].idNoun
  const teamOrManualHint = {
    github: `Empty = match any ${actor}. A login is looked up on GitHub and kept by account, so a rename doesn’t break the filter.`,
    jira: `Empty = match any ${actor}. Atlassian account IDs are opaque — paste from Jira's user profile URL.`,
    linear: `Empty = match any ${actor}. Type a name or email and press Enter to find the person in Linear.`,
  }[kind]
  // The account and ID wording for assignee_in, which both Jira and
  // Linear offer.
  const account = kind === 'linear' ? 'Linear account' : 'Atlassian account'
  const typedPlaceholder =
    kind === 'linear' ? 'search team or type a Linear name…' : 'paste an Atlassian account ID…'

  switch (actor) {
    case 'author':
      return {
        actor,
        toggleOnLabel: 'Match my PRs',
        toggleOffLabel: 'Match any author',
        scopedHint: (login) => `Scoped to PRs you authored as ${login}.`,
        unscopedHint: 'Fires on every author’s events.',
        searchPlaceholder: 'search team or type a GitHub login…',
        emptyHint: teamOrManualHint,
      }
    case 'reviewer':
      return {
        actor,
        toggleOnLabel: 'Match my reviews',
        toggleOffLabel: 'Match any reviewer',
        scopedHint: (login) => `Scoped to reviews you submitted as ${login}.`,
        unscopedHint: 'Fires on every reviewer’s reviews.',
        searchPlaceholder: 'search team or type a reviewer’s GitHub login…',
        emptyHint: teamOrManualHint,
      }
    case 'requested_user':
      return {
        actor: 'requested reviewer',
        toggleOnLabel: 'Match requests for my review',
        toggleOffLabel: 'Match any requested reviewer',
        scopedHint: (login) => `Scoped to review requests naming ${login}.`,
        unscopedHint: 'Fires on a request naming anyone.',
        searchPlaceholder: 'search team or type a GitHub login…',
        emptyHint: teamOrManualHint,
      }
    case 'commenter':
      return {
        actor,
        toggleOnLabel: 'Match my comments',
        toggleOffLabel: 'Match any commenter',
        scopedHint: (login) => `Scoped to comments left by ${login}.`,
        unscopedHint: 'Fires on every commenter’s comments.',
        searchPlaceholder: `search team or type a commenter ${idLabel}…`,
        emptyHint: teamOrManualHint,
      }
    case 'assignee':
      return {
        actor,
        toggleOnLabel: 'Match issues assigned to me',
        toggleOffLabel: 'Match any assignee',
        scopedHint: (id) => `Scoped to issues assigned to your ${account} (${id}).`,
        unscopedHint: 'Fires on every assignee.',
        searchPlaceholder: typedPlaceholder,
        emptyHint: teamOrManualHint,
      }
    case 'reporter':
      return {
        actor,
        toggleOnLabel: 'Match issues I reported',
        toggleOffLabel: 'Match any reporter',
        scopedHint: (id) => `Scoped to issues reported by ${id}.`,
        unscopedHint: 'Fires on every reporter.',
        searchPlaceholder: 'paste an Atlassian account ID…',
        emptyHint: teamOrManualHint,
      }
    case 'creator':
      return {
        actor,
        toggleOnLabel: 'Match issues I created',
        toggleOffLabel: 'Match any creator',
        scopedHint: (id) => `Scoped to issues created by ${id}.`,
        unscopedHint: 'Fires on every creator.',
        searchPlaceholder: typedPlaceholder,
        emptyHint: teamOrManualHint,
      }
    default:
      return {
        actor: actor || 'actor',
        toggleOnLabel: 'Match my events',
        toggleOffLabel: 'Match anyone',
        scopedHint: (id) => `Scoped to ${id}.`,
        unscopedHint: 'Fires on every actor.',
        searchPlaceholder: `search team or type a ${idLabel}…`,
        emptyHint: teamOrManualHint,
      }
  }
}

// --- Variant A: solo / local toggle -----------------------------------------

function VariantA({
  value,
  onChange,
  me,
  labels,
  identityKind,
  resolver,
}: {
  value: string[] | undefined
  onChange: (val: string[] | undefined) => void
  me: MeResponse
  labels: ActorLabels
  identityKind: IdentityKind
  resolver: IdentityResolver
}) {
  const source = SOURCES[identityKind]
  const storedID = source.fromMe(me) || null
  // A GitHub binding that recorded no account id still names a login, which
  // is looked up once to learn the id the field stores.
  const lookupLogin = identityKind === 'github' && !storedID ? me.github_username || '' : ''
  const [looked, setLooked] = useState<{ login: string; result: Resolution } | null>(null)
  const { resolve } = resolver
  useEffect(() => {
    if (!lookupLogin) return
    let cancelled = false
    resolve(lookupLogin).then((result) => {
      if (!cancelled) setLooked({ login: lookupLogin, result })
    })
    return () => {
      cancelled = true
    }
  }, [lookupLogin, resolve])
  const lookup = looked?.login === lookupLogin ? looked.result : null
  const myID = storedID ?? (lookup?.kind === 'value' ? lookup.value : null)
  const lookingUp = !!lookupLogin && lookup === null
  const hasIdentity = !!myID
  // The toggle is ON whenever the allowlist contains the current user's
  // identity. If the user has manually added external IDs too, the
  // toggle still represents "am I in the list" — clicking off removes
  // the user but leaves other entries alone. v1 doesn't surface those
  // other entries in Variant A; if you have external IDs you should
  // probably be on Variant B anyway.
  const isOn = !!myID && (value ?? []).some((h) => h.toLowerCase() === myID.toLowerCase())

  const handleToggle = () => {
    if (!myID) return
    if (isOn) {
      const next = (value ?? []).filter((h) => h.toLowerCase() !== myID.toLowerCase())
      onChange(next.length === 0 ? undefined : next)
    } else {
      onChange([...(value ?? []), myID])
    }
  }

  // For the Variant A scoped hint, prefer the friendly display name
  // when available (GitHub's login, Jira's and Linear's display names).
  const displayForHint = source.displayFromMe(me) || myID || ''

  return (
    <div>
      <button
        type="button"
        onClick={handleToggle}
        disabled={!hasIdentity}
        className={`inline-flex items-center gap-2 px-3 py-1.5 rounded-full text-ui font-medium border transition-colors ${
          isOn ? 'bg-warm/10 text-warm border-warm/25' : 'text-ink-3 border-line-1 hover:text-ink-2'
        } ${!hasIdentity ? 'opacity-50 cursor-not-allowed' : ''}`}
        aria-pressed={isOn}
      >
        <span className={`inline-block w-2 h-2 rounded-full ${isOn ? 'bg-warm' : 'bg-ink-3/40'}`} />
        {isOn ? labels.toggleOnLabel : labels.toggleOffLabel}
      </button>
      {hasIdentity ? (
        <p className="mt-1.5 text-reported text-ink-3">
          {isOn ? labels.scopedHint(displayForHint) : labels.unscopedHint}
        </p>
      ) : lookingUp ? (
        <p className="mt-1.5 text-reported text-ink-3">Looking {lookupLogin} up on GitHub…</p>
      ) : lookup?.kind === 'error' ? (
        <p role="alert" className="mt-1.5 text-reported text-alarm">
          {lookup.error}
        </p>
      ) : (
        <p className="mt-1.5 text-reported text-warm">
          {source.label} identity not yet captured.{' '}
          <a href="/settings" className="underline">
            Configure {source.label} on Settings
          </a>{' '}
          to enable this filter.
        </p>
      )}
    </div>
  )
}

// --- Variant B: team-aware multi-select (ARIA combobox + listbox) ----------
//
// The ARIA 1.2 combobox pattern: a single input element carries
// role="combobox" + aria-expanded + aria-controls + aria-activedescendant.
// A sibling element with role="listbox" holds option rows
// (role="option", aria-selected). The "active descendant" pattern is
// used (rather than DOM focus) so the input never loses focus while the
// user arrow-keys through suggestions — this is the standard pattern
// when typing and choosing must coexist.
//
// Keyboard:
//   ArrowDown    → move active to next item; open if closed
//   ArrowUp      → move active to previous item
//   Enter        → add the active item (or commit free-text)
//   Escape       → close dropdown without changing selection
//   Backspace    → if input is empty, remove last chip (lifted from native)

/** A flat "interactive item" projected from the dropdown's contents.
 *  Combines team suggestions with the "add an external identity" affordance
 *  so the active-descendant index can address them with one shape. */
type DropdownItem =
  | { kind: 'team'; member: TeamMember }
  | { kind: 'external'; handle: string }
  | { kind: 'choice'; choice: Choice }

function VariantB({
  value,
  onChange,
  members,
  labels,
  identityKind,
  resolver,
}: {
  value: string[] | undefined
  onChange: (val: string[] | undefined) => void
  members: TeamMember[]
  labels: ActorLabels
  identityKind: IdentityKind
  resolver: IdentityResolver
}) {
  const [search, setSearch] = useState('')
  const [open, setOpen] = useState(false)
  const [activeIndex, setActiveIndex] = useState(0)
  const [pending, setPending] = useState(false)
  // The people a typed name matched, offered in place of the team while the
  // field still holds that name.
  const [choices, setChoices] = useState<{ query: string; items: Choice[] } | null>(null)
  const [lookupError, setLookupError] = useState('')
  const containerRef = useRef<HTMLDivElement>(null)
  const inputRef = useRef<HTMLInputElement>(null)
  const listboxId = `identity-listbox-${useId()}`
  const optionIdFor = (i: number) => `${listboxId}-opt-${i}`
  // Memoize `selected` so the useMemo below has a stable dep when the
  // parent passes the same value array shape. (`value ?? []` returns
  // a fresh empty array every render when value is undefined, which
  // would invalidate every downstream memo.)
  const selected = useMemo(() => value ?? [], [value])
  // A GitHub lookup answers after other edits may have landed, so every write
  // starts from the value as it stands, not from the render that began it.
  const latest = useRef(selected)
  latest.current = selected

  // memberValue is what the field stores for a team member under the active
  // identity kind (github_user_id, jira_account_id or linear_user_id), and
  // memberHandle what a person knows them by (the GitHub login; the id
  // itself for Jira and Linear). Members without a handle show up as "no
  // identity" rows below the option list.
  const source = SOURCES[identityKind]
  const memberValue = (m: TeamMember): string | null => source.fromMember(m) || null
  const memberHandle = (m: TeamMember): string | null => source.handleFromMember(m) || null
  const sourceLabel = source.label

  // Close the dropdown when clicking outside the container.
  useEffect(() => {
    const onClick = (e: MouseEvent) => {
      if (!containerRef.current?.contains(e.target as Node)) setOpen(false)
    }
    document.addEventListener('mousedown', onClick)
    return () => document.removeEventListener('mousedown', onClick)
  }, [])

  const memberByValue = new Map<string, { display: string; handle: string; isSelf: boolean }>()
  for (const m of members) {
    const v = memberValue(m)
    const handle = memberHandle(m)
    if (v && handle) {
      memberByValue.set(v.toLowerCase(), {
        display: m.display_name || handle,
        handle,
        isSelf: m.is_current_user,
      })
    }
  }
  const isSelected = (v: string) => latest.current.some((h) => h.toLowerCase() === v.toLowerCase())

  // The flat dropdown-items list. Filtering and the external-identity
  // affordance combine into one indexable sequence so keyboard nav has
  // a single source of truth.
  const dropdownItems = useMemo<DropdownItem[]>(() => {
    if (choices && choices.query === search.trim()) {
      return choices.items
        .filter((c) => !selected.some((h) => h.toLowerCase() === c.value.toLowerCase()))
        .map((choice) => ({ kind: 'choice', choice }))
    }
    const q = search.trim().toLowerCase()
    const teamMatches = members.filter((m) => {
      const handle = memberHandle(m)
      if (!handle) return false
      const v = memberValue(m)
      if (v && selected.some((h) => h.toLowerCase() === v.toLowerCase())) return false
      return (
        q === '' ||
        handle.toLowerCase().includes(q) ||
        (m.display_name || '').toLowerCase().includes(q)
      )
    })
    const items: DropdownItem[] = teamMatches.map((m) => ({ kind: 'team', member: m }))
    const exactMatchExists = members.some((m) => memberHandle(m)?.toLowerCase() === q)
    const alreadySelected = selected.some(
      (h) => h.toLowerCase() === q || resolver.handleOf(h).toLowerCase() === q,
    )
    if (q !== '' && !exactMatchExists && !alreadySelected) {
      items.push({ kind: 'external', handle: search.trim() })
    }
    return items
    // identityKind, the resolver's names and the choices a lookup offered are
    // the only mutable inputs besides search/selected. members is
    // identity-stable per fetch.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [members, search, selected, identityKind, resolver, choices])

  // Clamp the active-index during render rather than via an effect:
  // storing the raw setState value and reading a clamped derivation
  // avoids a cascading-render cycle when the items list shrinks
  // (e.g. user typed more characters and fewer suggestions match).
  // The stored `activeIndex` may temporarily exceed `dropdownItems.length`
  // but every read goes through `safeActiveIndex`.
  const safeActiveIndex =
    dropdownItems.length === 0 ? 0 : Math.min(activeIndex, dropdownItems.length - 1)

  const addValue = (v: string) => {
    setLookupError('')
    setSearch('')
    setActiveIndex(0)
    setChoices(null)
    if (isSelected(v)) return
    onChange([...latest.current, v])
  }
  // addTyped stores what a handle resolves to: itself for Jira and Linear,
  // the account id of a GitHub login, refused beside the field when GitHub
  // has no account by that login.
  const addTyped = async (handle: string) => {
    const trimmed = handle.trim()
    if (!trimmed || pending) return
    setPending(true)
    const result = await resolver.resolve(trimmed)
    setPending(false)
    if (result.kind === 'value') {
      addValue(result.value)
    } else if (result.kind === 'choices') {
      setChoices({ query: trimmed, items: result.choices })
      setActiveIndex(0)
      setOpen(true)
    } else {
      setLookupError(result.error)
    }
  }
  const addMember = (m: TeamMember) => {
    const v = memberValue(m)
    if (v) {
      addValue(v)
      return
    }
    // A GitHub member whose binding recorded no account id is added by
    // looking their login up.
    const handle = memberHandle(m)
    if (handle) void addTyped(handle)
  }
  const removeValue = (v: string) => {
    const next = latest.current.filter((h) => h.toLowerCase() !== v.toLowerCase())
    onChange(next.length === 0 ? undefined : next)
  }

  const commitActive = () => {
    if (dropdownItems.length === 0) {
      // Nothing in the dropdown but the user has typed something —
      // commit the free text as an external identity.
      if (search.trim()) void addTyped(search)
      return
    }
    const item = dropdownItems[safeActiveIndex]
    if (!item) return
    if (item.kind === 'team') {
      addMember(item.member)
    } else if (item.kind === 'choice') {
      addValue(item.choice.value)
    } else if (item.kind === 'external') {
      void addTyped(item.handle)
    }
  }

  const onKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'Enter') {
      e.preventDefault()
      commitActive()
      return
    }
    if (e.key === 'ArrowDown') {
      e.preventDefault()
      setOpen(true)
      if (dropdownItems.length > 0) {
        setActiveIndex((i) => (i + 1) % dropdownItems.length)
      }
      return
    }
    if (e.key === 'ArrowUp') {
      e.preventDefault()
      setOpen(true)
      if (dropdownItems.length > 0) {
        setActiveIndex((i) => (i - 1 + dropdownItems.length) % dropdownItems.length)
      }
      return
    }
    if (e.key === 'Escape') {
      e.preventDefault()
      setOpen(false)
      return
    }
    if (e.key === 'Backspace' && search === '' && selected.length > 0) {
      removeValue(selected[selected.length - 1])
    }
  }

  // Parent gates VariantB on members.length > 1 and !membersError, so
  // we never render with a loading/error roster — drop those branches.
  const showDropdown = open && dropdownItems.length > 0
  const ariaExpanded = showDropdown
  const externalNoun =
    identityKind === 'github'
      ? 'GitHub account'
      : identityKind === 'linear'
        ? 'Linear user'
        : `external ${source.idNoun}`

  return (
    <div className="relative" ref={containerRef}>
      <div
        className="min-h-[40px] w-full px-2 py-1.5 rounded-lg border border-line-1 bg-raised flex flex-wrap items-center gap-1.5 cursor-text focus-within:border-warm/40 focus-within:ring-1 focus-within:ring-warm/20"
        onClick={() => {
          inputRef.current?.focus()
          setOpen(true)
        }}
      >
        {selected.map((v) => {
          const m = memberByValue.get(v.toLowerCase())
          const handle = m ? m.handle : resolver.handleOf(v)
          return (
            <span
              key={v}
              className={`inline-flex items-center gap-1.5 px-2 py-0.5 rounded-full text-reported border ${
                m ? 'bg-warm/10 text-warm border-warm/25' : 'bg-tint-2 text-ink-2 border-line-2'
              }`}
              title={m ? `${m.display}${m.isSelf ? ' (you)' : ''}` : `${externalNoun}: ${handle}`}
            >
              {m ? `${m.display} · ${handle}` : handle}
              <button
                type="button"
                onClick={(e) => {
                  e.stopPropagation()
                  removeValue(v)
                }}
                aria-label={`Remove ${handle}`}
                className="hover:text-ink-1"
              >
                <X size={11} aria-hidden="true" />
              </button>
            </span>
          )
        })}
        <input
          ref={inputRef}
          type="text"
          role="combobox"
          aria-expanded={ariaExpanded}
          aria-controls={listboxId}
          aria-autocomplete="list"
          aria-activedescendant={
            showDropdown && dropdownItems.length > 0 ? optionIdFor(safeActiveIndex) : undefined
          }
          aria-busy={pending}
          value={search}
          onChange={(e) => {
            setSearch(e.target.value)
            setLookupError('')
            setOpen(true)
            setActiveIndex(0)
          }}
          onKeyDown={onKeyDown}
          onFocus={() => setOpen(true)}
          placeholder={selected.length === 0 ? labels.searchPlaceholder : ''}
          className="flex-1 min-w-[140px] bg-transparent text-body text-ink-1 placeholder:text-ink-3 focus:outline-none"
        />
      </div>

      {showDropdown && (
        <ul
          id={listboxId}
          role="listbox"
          className="absolute z-20 mt-1 w-full max-h-[260px] overflow-y-auto rounded-lg border border-line-1 bg-raised shadow-float list-none p-0 m-0"
        >
          {dropdownItems.map((item, i) => {
            const isActive = i === safeActiveIndex
            const baseCls = `w-full flex items-center justify-between px-3 py-2 text-left cursor-pointer ${
              isActive ? 'bg-warm/10' : 'hover:bg-warm/5'
            }`
            if (item.kind === 'team') {
              const m = item.member
              const handle = memberHandle(m)
              return (
                <li
                  id={optionIdFor(i)}
                  key={m.user_id}
                  role="option"
                  aria-selected={isActive}
                  onMouseDown={(e) => {
                    // mousedown rather than click so the input doesn't blur
                    // and close the dropdown before the selection registers.
                    e.preventDefault()
                    addMember(m)
                  }}
                  onMouseEnter={() => setActiveIndex(i)}
                  className={baseCls}
                >
                  <span className="text-body text-ink-1">
                    {m.display_name || handle}
                    {m.is_current_user && <span className="ml-1 text-label text-ink-3">(you)</span>}
                  </span>
                  <span className="text-reported font-mono text-ink-3">{handle}</span>
                </li>
              )
            }
            if (item.kind === 'choice') {
              const c = item.choice
              return (
                <li
                  id={optionIdFor(i)}
                  key={`choice-${c.value}`}
                  role="option"
                  aria-selected={isActive}
                  onMouseDown={(e) => {
                    e.preventDefault()
                    addValue(c.value)
                  }}
                  onMouseEnter={() => setActiveIndex(i)}
                  className={baseCls}
                >
                  <span className="text-body text-ink-1">{c.label}</span>
                  <span className="text-reported font-mono text-ink-3">{c.detail}</span>
                </li>
              )
            }
            return (
              <li
                id={optionIdFor(i)}
                key={`external-${item.handle}`}
                role="option"
                aria-selected={isActive}
                onMouseDown={(e) => {
                  e.preventDefault()
                  void addTyped(item.handle)
                }}
                onMouseEnter={() => setActiveIndex(i)}
                className={`${baseCls} text-ink-3 border-t border-line-1`}
              >
                <span className="text-ui">
                  {identityKind === 'linear' ? (
                    <>+ Find &ldquo;{item.handle}&rdquo; in Linear</>
                  ) : (
                    <>
                      + Add &ldquo;{item.handle}&rdquo; as {externalNoun}
                    </>
                  )}
                </span>
              </li>
            )
          })}
          {/* Members without captured identity render outside the option
              flow so they're not navigable and don't enter the
              active-descendant rotation. Surfaces the gap honestly. */}
          {members
            .filter((m) => !memberHandle(m))
            .map((m) => (
              <li
                key={m.user_id}
                role="presentation"
                aria-hidden="true"
                className="px-3 py-2 text-ui text-ink-3 opacity-60"
                title={`No ${sourceLabel} identity captured — ask this user to configure ${sourceLabel} on Settings`}
              >
                {m.display_name || (m.github_username ? `@${m.github_username}` : '(no name)')}{' '}
                <span className="text-label">— no {sourceLabel} identity</span>
              </li>
            ))}
        </ul>
      )}

      {pending && identityKind === 'github' && (
        <p className="mt-1.5 text-reported text-ink-3">Looking the login up on GitHub…</p>
      )}
      {pending && identityKind === 'linear' && (
        <p className="mt-1.5 text-reported text-ink-3">Looking the name up in Linear…</p>
      )}
      {lookupError && (
        <p role="alert" className="mt-1.5 text-reported text-alarm">
          {lookupError}
        </p>
      )}
      <p className="mt-1.5 text-reported text-ink-3">{labels.emptyHint}</p>
    </div>
  )
}
