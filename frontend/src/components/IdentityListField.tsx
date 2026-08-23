import { useEffect, useId, useMemo, useRef, useState } from 'react'
import { X } from 'lucide-react'
import { useMe, useTeamMembers } from '../hooks/useDeploymentConfig'
import type { MeResponse, TeamMember } from '../types'

/** IdentityListField is the editor for `author_in` / `reviewer_in` /
 *  `commenter_in` (GitHub) and `assignee_in` / `reporter_in` /
 *  `commenter_in` (Jira) predicate fields. Two variants based
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
 *    members. Free-text entry supports external handles (bots,
 *    contractors not on the team). Implements the ARIA combobox
 *    pattern + keyboard navigation (Arrow keys, Enter, Escape).
 *
 *  identityKind selects which user-row column drives Variant A's
 *  toggle and Variant B's roster lookups: 'github' for GitHub logins,
 *  'jira' for Atlassian account IDs.
 *
 *  Both serialize to the same wire shape: `string[] | undefined`. The
 *  matcher does case-insensitive compare server-side, so client casing
 *  is preserved verbatim for round-trip rendering. */
export type IdentityKind = 'github' | 'jira'

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
    />
  )
}

// identityFromMe returns the canonical identifier on the current-user
// payload for the given identity source. Centralizes the
// github_username vs jira_account_id branch so call sites don't have
// to know.
function identityFromMe(me: MeResponse, kind: IdentityKind): string | null {
  const v = kind === 'jira' ? me.jira_account_id : me.github_username
  return v || null
}

// identityFromMember returns the canonical identifier for a team-roster
// row. Same shape choice as identityFromMe but the roster type carries
// explicit `null`s (the server distinguishes "not yet captured").
function identityFromMember(m: TeamMember, kind: IdentityKind): string | null {
  return kind === 'jira' ? m.jira_account_id : m.github_username
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
  // Strip the canonical "_in" suffix to derive the actor name. Falls
  // back to the field name itself if the suffix is missing — future
  // non-identity string_list predicates would hit that path and get
  // generic copy, which is still less wrong than mislabeling everything
  // as "author."
  const actor = fieldName.endsWith('_in') ? fieldName.slice(0, -3) : fieldName
  const idLabel = kind === 'jira' ? 'account ID' : 'handle'
  const teamOrManualHint =
    kind === 'jira'
      ? `Empty = match any ${actor}. Atlassian account IDs are opaque — paste from Jira's user profile URL.`
      : `Empty = match any ${actor}. Case-insensitive.`

  switch (actor) {
    case 'author':
      return {
        actor,
        toggleOnLabel: 'Match my PRs',
        toggleOffLabel: 'Match any author',
        scopedHint: (login) => `Scoped to PRs you authored as ${login}.`,
        unscopedHint: 'Fires on every author’s events.',
        searchPlaceholder: 'search team or type a handle…',
        emptyHint: 'Empty = match any author. Case-insensitive.',
      }
    case 'reviewer':
      return {
        actor,
        toggleOnLabel: 'Match my reviews',
        toggleOffLabel: 'Match any reviewer',
        scopedHint: (login) => `Scoped to reviews you submitted as ${login}.`,
        unscopedHint: 'Fires on every reviewer’s reviews.',
        searchPlaceholder: 'search team or type a reviewer handle…',
        emptyHint: 'Empty = match any reviewer. Case-insensitive.',
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
        scopedHint: (id) => `Scoped to issues assigned to your Atlassian account (${id}).`,
        unscopedHint: 'Fires on every assignee.',
        searchPlaceholder: 'paste an Atlassian account ID…',
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
}: {
  value: string[] | undefined
  onChange: (val: string[] | undefined) => void
  me: MeResponse
  labels: ActorLabels
  identityKind: IdentityKind
}) {
  const myID = identityFromMe(me, identityKind)
  const hasIdentity = !!myID
  // The toggle is ON whenever the allowlist contains the current user's
  // identity. If the user has manually added external IDs too, the
  // toggle still represents "am I in the list" — clicking off removes
  // the user but leaves other entries alone. v1 doesn't surface those
  // other entries in Variant A; if you have external IDs you should
  // probably be on Variant B anyway.
  const isOn = !!myID && (value ?? []).some((h) => h.toLowerCase() === myID.toLowerCase())

  const handleToggle = () => {
    if (!hasIdentity) return
    if (isOn) {
      const next = (value ?? []).filter((h) => h.toLowerCase() !== myID!.toLowerCase())
      onChange(next.length === 0 ? undefined : next)
    } else {
      onChange([...(value ?? []), myID!])
    }
  }

  // For the Variant A scoped hint, prefer the friendly display name
  // when available (Jira gives us both account ID and display name;
  // GitHub's login doubles as the display string).
  const displayForHint = identityKind === 'jira' ? me.jira_display_name || myID || '' : myID || ''

  const sourceLabel = identityKind === 'jira' ? 'Jira' : 'GitHub'

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
      ) : (
        <p className="mt-1.5 text-reported text-warm">
          {sourceLabel} identity not yet captured.{' '}
          <a href="/settings" className="underline">
            Configure {sourceLabel} on Settings
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
 *  Combines team suggestions with the "add external handle" affordance
 *  so the active-descendant index can address them with one shape. */
type DropdownItem = { kind: 'team'; member: TeamMember } | { kind: 'external'; handle: string }

function VariantB({
  value,
  onChange,
  members,
  labels,
  identityKind,
}: {
  value: string[] | undefined
  onChange: (val: string[] | undefined) => void
  members: TeamMember[]
  labels: ActorLabels
  identityKind: IdentityKind
}) {
  const [search, setSearch] = useState('')
  const [open, setOpen] = useState(false)
  const [activeIndex, setActiveIndex] = useState(0)
  const containerRef = useRef<HTMLDivElement>(null)
  const inputRef = useRef<HTMLInputElement>(null)
  const listboxId = `identity-listbox-${useId()}`
  const optionIdFor = (i: number) => `${listboxId}-opt-${i}`
  // Memoize `selected` so the useMemo below has a stable dep when the
  // parent passes the same value array shape. (`value ?? []` returns
  // a fresh empty array every render when value is undefined, which
  // would invalidate every downstream memo.)
  const selected = useMemo(() => value ?? [], [value])

  // memberIdentity reads the canonical ID for a team member under the
  // active identity kind: github_username for GitHub predicates,
  // jira_account_id for Jira predicates. Members without that field
  // captured show up as "no identity" rows below the option list.
  const memberIdentity = (m: TeamMember): string | null => identityFromMember(m, identityKind)
  const sourceLabel = identityKind === 'jira' ? 'Jira' : 'GitHub'

  // Close the dropdown when clicking outside the container.
  useEffect(() => {
    const onClick = (e: MouseEvent) => {
      if (!containerRef.current?.contains(e.target as Node)) setOpen(false)
    }
    document.addEventListener('mousedown', onClick)
    return () => document.removeEventListener('mousedown', onClick)
  }, [])

  const memberByLogin = new Map<string, { display: string; isSelf: boolean }>()
  for (const m of members) {
    const id = memberIdentity(m)
    if (id) {
      memberByLogin.set(id.toLowerCase(), {
        display: m.display_name || id,
        isSelf: m.is_current_user,
      })
    }
  }

  // The flat dropdown-items list. Filtering and the external-handle
  // affordance combine into one indexable sequence so keyboard nav has
  // a single source of truth.
  const dropdownItems = useMemo<DropdownItem[]>(() => {
    const q = search.trim().toLowerCase()
    const teamMatches = members.filter((m) => {
      const id = memberIdentity(m)
      if (!id) return false
      if (selected.some((h) => h.toLowerCase() === id.toLowerCase())) return false
      return (
        q === '' || id.toLowerCase().includes(q) || (m.display_name || '').toLowerCase().includes(q)
      )
    })
    const items: DropdownItem[] = teamMatches.map((m) => ({ kind: 'team', member: m }))
    const exactMatchExists = members.some((m) => {
      const id = memberIdentity(m)
      return id && id.toLowerCase() === q
    })
    const alreadySelected = selected.some((h) => h.toLowerCase() === q)
    if (q !== '' && !exactMatchExists && !alreadySelected) {
      items.push({ kind: 'external', handle: search.trim() })
    }
    return items
    // identityKind is the only mutable input besides search/selected.
    // members is identity-stable per fetch.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [members, search, selected, identityKind])

  // Clamp the active-index during render rather than via an effect:
  // storing the raw setState value and reading a clamped derivation
  // avoids a cascading-render cycle when the items list shrinks
  // (e.g. user typed more characters and fewer suggestions match).
  // The stored `activeIndex` may temporarily exceed `dropdownItems.length`
  // but every read goes through `safeActiveIndex`.
  const safeActiveIndex =
    dropdownItems.length === 0 ? 0 : Math.min(activeIndex, dropdownItems.length - 1)

  const addHandle = (handle: string) => {
    const trimmed = handle.trim()
    if (!trimmed) return
    if (selected.some((h) => h.toLowerCase() === trimmed.toLowerCase())) return
    onChange([...selected, trimmed])
    setSearch('')
    setActiveIndex(0)
  }
  const removeHandle = (handle: string) => {
    const next = selected.filter((h) => h.toLowerCase() !== handle.toLowerCase())
    onChange(next.length === 0 ? undefined : next)
  }

  const commitActive = () => {
    if (dropdownItems.length === 0) {
      // Nothing in the dropdown but the user has typed something —
      // commit the free-text as a literal external handle.
      if (search.trim()) addHandle(search)
      return
    }
    const item = dropdownItems[safeActiveIndex]
    if (!item) return
    if (item.kind === 'team') {
      const id = memberIdentity(item.member)
      if (id) addHandle(id)
    } else if (item.kind === 'external') {
      addHandle(item.handle)
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
      removeHandle(selected[selected.length - 1])
    }
  }

  // Parent gates VariantB on members.length > 1 and !membersError, so
  // we never render with a loading/error roster — drop those branches.
  const showDropdown = open && dropdownItems.length > 0
  const ariaExpanded = showDropdown

  return (
    <div className="relative" ref={containerRef}>
      <div
        className="min-h-[40px] w-full px-2 py-1.5 rounded-lg border border-line-1 bg-raised flex flex-wrap items-center gap-1.5 cursor-text focus-within:border-warm/40 focus-within:ring-1 focus-within:ring-warm/20"
        onClick={() => {
          inputRef.current?.focus()
          setOpen(true)
        }}
      >
        {selected.map((h) => {
          const m = memberByLogin.get(h.toLowerCase())
          return (
            <span
              key={h}
              className={`inline-flex items-center gap-1.5 px-2 py-0.5 rounded-full text-reported border ${
                m ? 'bg-warm/10 text-warm border-warm/25' : 'bg-tint-2 text-ink-2 border-line-2'
              }`}
              title={m ? `${m.display}${m.isSelf ? ' (you)' : ''}` : `External handle: ${h}`}
            >
              {m ? `${m.display} · ${h}` : h}
              <button
                type="button"
                onClick={(e) => {
                  e.stopPropagation()
                  removeHandle(h)
                }}
                aria-label={`Remove ${h}`}
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
          value={search}
          onChange={(e) => {
            setSearch(e.target.value)
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
              const id = memberIdentity(m)
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
                    if (id) addHandle(id)
                  }}
                  onMouseEnter={() => setActiveIndex(i)}
                  className={baseCls}
                >
                  <span className="text-body text-ink-1">
                    {m.display_name || id}
                    {m.is_current_user && <span className="ml-1 text-label text-ink-3">(you)</span>}
                  </span>
                  <span className="text-reported font-mono text-ink-3">{id}</span>
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
                  addHandle(item.handle)
                }}
                onMouseEnter={() => setActiveIndex(i)}
                className={`${baseCls} text-ink-3 border-t border-line-1`}
              >
                <span className="text-ui">
                  + Add &ldquo;{item.handle}&rdquo; as external handle
                </span>
              </li>
            )
          })}
          {/* Members without captured identity render outside the option
              flow so they're not navigable and don't enter the
              active-descendant rotation. Surfaces the gap honestly. */}
          {members
            .filter((m) => !memberIdentity(m))
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

      <p className="mt-1.5 text-reported text-ink-3">{labels.emptyHint}</p>
    </div>
  )
}
