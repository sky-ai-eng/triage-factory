import { useState, useEffect, useRef, Fragment } from 'react'
import type { CSSProperties, KeyboardEvent as ReactKeyboardEvent, ReactNode } from 'react'
import { Ico } from './glyphs'
import { routes, grantLabel } from './routes'
import type { Flags, Grant, RailCounts, Row } from './routes'
import { TitleField } from './TitleField'
import './shell.css'

export type Crumb = { name: string; onClick?: () => void }

export type ShellProps = {
  /** Deployment mode. Local hides everything multi-tenant. Default 'multi'. */
  mode?: 'local' | 'multi'
  /**
   * The API is unreachable. Not a loading state: readouts go inert rather than
   * holding their last value, because a stale count in a triage rail is worse
   * than no count — 7 could equally be 0 or 40, and only the rail knows which
   * it is claiming. The rail states it once at the foot.
   */
  offline?: boolean
  /** Grants held by the signed-in account. Sections appear per grant, never per role name. */
  grants?: Grant[]
  /** Org-level feature flags. A disabled feature is absent, not greyed. */
  flags?: Flags
  /** Org name shown beside the sigil. */
  org?: string
  /** Current team, shown under the org in multi mode. The id is what keys
   *  the popup's active row — display names are not unique, so a name is
   *  never the currency of a selection, only its face. */
  team?: { id: string; name: string } | null
  /** Teams offered by the scope switcher. */
  teams?: Array<{ id: string; name: string }>
  onTeamChange?: (teamId: string) => void
  /** Active route id, e.g. 'usage' or 'prompts.bindings'. */
  route?: string
  onRoute?: (id: string) => void
  /** Header path segments to the left of the title. */
  crumbs?: Crumb[]
  title?: string
  /**
   * Renaming happens where the name is. Passing this makes the title an inline
   * field: the pencil appears on hover, a click opens it, Enter or a click away
   * saves, Escape abandons.
   */
  onTitleSave?: ((name: string) => void) | null
  subtitle?: string
  /** Header actions, right-aligned before the search affordance. */
  actions?: ReactNode
  /**
   * Live counts above the rule. A change to needs is marked once.
   *
   * `null` means NOT KNOWN YET, and reads as `--` exactly like offline does. It
   * is not the same as 0: "nothing needs you" is a claim, and a rail that makes
   * it before the count has loaded is telling the reader they are done when it
   * has no idea. The same reasoning as the offline rule, one step earlier.
   */
  needs?: number | null
  running?: number | null
  queued?: number | null
  /** Rail readouts, per route. */
  counts?: RailCounts
  user?: { name: string; email: string } | null
  onSignOut?: () => void
  /**
   * The page has asked for the whole screen. The rail and the header fade and
   * stop taking input; the layout does not move.
   *
   * Fading rather than collapsing is the point. Collapsing would reflow the
   * page and, for a page that is immersive because it renders continuously,
   * that reflow is the exact thing it was trying to escape. Nothing here
   * changes size.
   */
  immersive?: boolean
  children?: ReactNode
}

// Shell — rail, scope, counts, sections, drill-in, palette, page header.
//
// Presentational by construction: it takes route ids and hands them back
// through onRoute, and it never fetches. The router and the data live in the
// adapter at src/Shell.tsx. That split is what lets /dev/ui mount all four
// grant permutations side by side, which is how the rail's behaviour across
// grants gets reviewed at all.
//
// Reduced motion: nothing here needs a branch. Every mark this component makes —
// the tick on a count that changed, the flash on a name field that has hit its
// cap — is a CLASS or a data attribute that shell.css animates, so the blanket
// rule in tokens/motion.css removes the motion and leaves the new value in
// place. The timers only take the class back off again. See Shell.md.

export function Shell({
  mode = 'multi',
  offline = false,
  grants = [],
  flags = {},
  org = '',
  team = null,
  teams = [],
  onTeamChange,
  route = '',
  onRoute,
  crumbs = [],
  title = '',
  onTitleSave = null,
  subtitle = '',
  actions = null,
  needs = null,
  running = null,
  queued = null,
  counts = {},
  user = null,
  onSignOut,
  immersive = false,
  children,
}: ShellProps) {
  const [pinned, setPinned] = useState(() => {
    try {
      return localStorage.getItem('tf-rail') === '1'
    } catch {
      return false
    }
  })
  const [deep, setDeep] = useState<string | null>(null)
  const [pal, setPal] = useState(false)
  const [q, setQ] = useState('')
  const [sel, setSel] = useState(0)
  const [scope, setScope] = useState(false)
  const [me, setMe] = useState(false)
  const [more, setMore] = useState(false)
  const [tip, setTip] = useState<{ name: string; y: number } | null>(null)

  // Both panels are REGIONS OF THE RAIL, never floats. A borderless haze can
  // only separate over a quiet surface; over live page content it competes with
  // whatever is behind it, and no amount of wash fixes that. So a rail holding
  // an open panel is wide, full stop — DERIVED, not a second piece of state
  // pushed around by an effect. That is what makes the transient expansion
  // unable to write the pin: there is nothing to write.
  const open = pinned || scope || me

  const rootref = useRef<HTMLDivElement | null>(null)
  const qref = useRef<HTMLInputElement | null>(null)
  const colref = useRef<HTMLDivElement | null>(null)

  // Overlays are anchored in the SHELL's own coordinate space, not the
  // viewport's. position:fixed resolves against the nearest transformed
  // ancestor, so a shell inside any scaled or panned frame — a design canvas, a
  // zoomed browser, an embedded preview — would place its popovers by viewport
  // numbers against a different origin and drift inward on both axes. Measuring
  // against the root, and dividing out the scale the root is drawn at, makes
  // placement independent of whatever is above it.
  const local = (el: Element) => {
    const root = rootref.current
    if (!root) return { top: 0, height: 0 }
    const rr = root.getBoundingClientRect()
    const s = root.offsetWidth ? rr.width / root.offsetWidth : 1
    const r = el.getBoundingClientRect()
    return { top: (r.top - rr.top) / s, height: r.height / s }
  }

  const secs = routes({ mode, grants, flags, n: counts })

  // The palette searches the same array the rail is built from, so it can never
  // offer a page the rail is hiding.
  const flat: Array<{ id: string; name: string; sec: string }> = [
    { id: 'queue', name: 'Queue', sec: 'WORK' },
  ]
  secs.forEach((s) =>
    s.rows.forEach((r) => {
      flat.push({ id: r.id, name: r.name, sec: s.name })
      ;(r.children || []).forEach((c) =>
        flat.push({ id: c.id, name: c.name, sec: r.name.toUpperCase() }),
      )
    }),
  )

  const tipOn = (name: string) => (e: { currentTarget: Element }) => {
    if (open) return
    const b = local(e.currentTarget)
    setTip({ name, y: b.top + b.height / 2 })
  }
  const tipOff = () => setTip(null)

  // The focus-shown tip exists for the keyboard user tabbing a collapsed
  // rail's unnamed glyphs, so it is gated on :focus-visible — a pointer
  // click's focus must not raise it, and neither may the browser RE-focusing
  // the last-clicked row when the window regains focus (Cmd+Tab back), which
  // painted a label no pointer would ever leave. The try/catch is for a DOM
  // that cannot parse the pseudo-class (jsdom), where the gate opens rather
  // than silently unlabelling the rail for keyboard users.
  const tipOnKeyFocus = (name: string) => (e: { currentTarget: Element }) => {
    let keyboard = true
    try {
      keyboard = e.currentTarget.matches(':focus-visible')
    } catch {
      // Unparseable selector: keep the tip for whoever focused.
    }
    if (keyboard) tipOn(name)(e)
  }

  // A tip describes what the pointer (or focus) is on RIGHT NOW, so leaving
  // the window ends it: mouseleave never fires on a Cmd+Tab, and a label that
  // outlives the hand that raised it reads as stuck.
  useEffect(() => {
    const off = () => setTip(null)
    window.addEventListener('blur', off)
    return () => window.removeEventListener('blur', off)
  }, [])

  const go = (id: string) => {
    onRoute?.(id)
    setPal(false)
    setDeep(null)
    setTip(null)
  }

  // Closes on a click away or on the mark again — never on the pointer leaving,
  // which turns a deliberate choice into something you can lose by moving your
  // hand.
  useEffect(() => {
    if (!scope) return
    const away = (e: MouseEvent) => {
      const t = e.target as Element | null
      if (t?.closest('.sh-pop') || t?.closest('.sh-mark')) return
      setScope(false)
    }
    document.addEventListener('mousedown', away, true)
    return () => document.removeEventListener('mousedown', away, true)
  }, [scope])

  useEffect(() => {
    if (!me) return
    const away = (e: MouseEvent) => {
      const t = e.target as Element | null
      if (t?.closest('.sh-you') || t?.closest('.sh-me')) return
      setMe(false)
    }
    document.addEventListener('mousedown', away, true)
    return () => document.removeEventListener('mousedown', away, true)
  }, [me])

  const toggle = () => {
    // A panel is a region of the rail, so collapsing the rail dismisses it —
    // otherwise it would still be mounted inside a 52px column that clips its
    // own overflow, which is a panel you cannot see.
    setScope(false)
    setMe(false)
    setPinned((p) => {
      try {
        localStorage.setItem('tf-rail', p ? '0' : '1')
      } catch {
        /* storage unavailable — this session only */
      }
      return !p
    })
  }
  const toggleRef = useRef(toggle)
  useEffect(() => {
    toggleRef.current = toggle
  })

  useEffect(() => {
    const onKey = (e: globalThis.KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key === 'k') {
        e.preventDefault()
        setPal((p) => !p)
        setQ('')
        setSel(0)
      } else if ((e.metaKey || e.ctrlKey) && e.key === '\\') {
        e.preventDefault()
        toggleRef.current()
      } else if (e.key === 'Escape') {
        // The shell consumes Escape only when it has a layer to dismiss, and
        // says so by marking the event. Pages mounted inside it (whose own
        // Escape means something) check defaultPrevented, so one press never
        // does two things.
        if (pal) setPal(false)
        else if (me) setMe(false)
        else if (scope) setScope(false)
        else if (deep) setDeep(null)
        else return
        e.preventDefault()
      }
    }
    // Capture, so the shell always sees the key before the page inside it.
    // Registration order cannot be relied on: this listener re-registers
    // whenever a layer opens, which would otherwise put it after the page's.
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
  }, [pal, scope, deep, me])

  useEffect(() => {
    if (pal && qref.current) qref.current.focus()
  }, [pal])

  // "There is more below" — the column suppresses its scrollbar, so the fade is
  // the only thing that can say so.
  useEffect(() => {
    const el = colref.current
    if (!el) return
    const read = () => setMore(el.scrollHeight - el.clientHeight - el.scrollTop > 4)
    read()
    el.addEventListener('scroll', read)
    window.addEventListener('resize', read)
    // The rail can lose height without the window resizing — a panel opening in
    // the rail, a container laid out differently — and a stale hint is worse
    // than none, because it points at rows that are already visible.
    const ro = typeof ResizeObserver !== 'undefined' ? new ResizeObserver(read) : null
    ro?.observe(el)
    const t = setTimeout(read, 260)
    return () => {
      el.removeEventListener('scroll', read)
      window.removeEventListener('resize', read)
      ro?.disconnect()
      clearTimeout(t)
    }
  }, [open, deep, route, secs.length])

  const hits = q ? flat.filter((r) => r.name.toLowerCase().includes(q.toLowerCase())) : flat
  const deepRow = deep ? secs.flatMap((s) => s.rows).find((r) => r.id === deep) || null : null
  const readout = (v: number | null) => (offline || v === null ? '--' : String(v))
  const queueTip = offline || queued === null ? 'Queue · count unavailable' : `${queued} in queue`

  const enter = (fn: () => void) => (e: ReactKeyboardEvent) => {
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault()
      fn()
    }
  }

  const row = (r: Row) => {
    const on = route === r.id || (!!r.children && route.startsWith(r.id + '.'))
    const hit = () => {
      setTip(null)
      if (r.children) setDeep(r.id)
      else go(r.id)
    }
    return (
      <div
        key={r.id}
        className={'sh-row' + (on ? ' on' : '') + (r.alert && !offline ? ' alerting' : '')}
        onClick={hit}
        // Reachable by keyboard, and it announces what it is: a row with
        // children opens a column, so it is a button; every other row goes
        // somewhere, so it is a link.
        tabIndex={0}
        role={r.children ? 'button' : 'link'}
        aria-current={on ? 'page' : undefined}
        aria-expanded={r.children ? deep === r.id : undefined}
        onKeyDown={enter(hit)}
        onMouseEnter={tipOn(r.name)}
        onMouseLeave={tipOff}
        // A collapsed rail has no labels, so a keyboard user would otherwise tab
        // through eleven unnamed glyphs.
        onFocus={tipOnKeyFocus(r.name)}
        onBlur={tipOff}
      >
        <div className="sh-ico">
          <Ico d={r.icon} />
        </div>
        <div className="sh-name">{r.name}</div>
        {r.counts ? (
          <div className="sh-tail sh-two">
            {/* Keyed by the value: a change remounts the node, which replays
                sh-tick once. A changing count is an event, marked once and then
                over — no pulse, no ambient loop. */}
            <span key={String(needs)} className="warm sh-tick">
              <i />
              {readout(needs)}
            </span>
            <span className="cool">
              <i />
              {readout(running)}
            </span>
          </div>
        ) : r.children ? (
          <div className="sh-tail">
            <Ico d="chev" size={12} />
          </div>
        ) : r.alert && !offline ? (
          <div className="sh-tail sh-alert">
            <Ico d="alert" size={13} />
            {String(r.alert)}
          </div>
        ) : r.tail ? (
          // Every tail is a figure the API supplied, so every tail goes inert
          // together — a rail with two live numbers and six stale ones is
          // harder to read than one with none.
          <div className="sh-tail">{offline ? '--' : r.tail}</div>
        ) : null}
      </div>
    )
  }

  const youKids = () => (
    <>
      {/* Expanded, the rail's own row carries name and email, so the panel
          doesn't repeat them — the same rule the scope switcher follows at the
          other end of the rail. */}
      {mode === 'multi' && !open && user && (
        <div className="sh-youo">
          <b>{user.name}</b>
          <span>{user.email}</span>
        </div>
      )}
      {mode === 'multi' && grants.length > 0 && (
        <div className="sh-yougr">{grants.map((g) => grantLabel[g] || g).join(' · ')}</div>
      )}
      {mode === 'multi' && onSignOut && (
        <div
          className="sh-youout"
          onClick={() => {
            setMe(false)
            onSignOut()
          }}
          tabIndex={0}
          role="button"
          onKeyDown={enter(() => {
            setMe(false)
            onSignOut()
          })}
        >
          Sign out
        </div>
      )}
    </>
  )

  return (
    <div
      className="sh"
      data-immersive={immersive || undefined}
      style={{ position: 'relative' }}
      ref={rootref}
    >
      {/* `inert` as well as pointer-events, so the faded chrome leaves the tab
          order too. Invisible controls that still take focus are worse than
          visible ones — the reader tabs into a rail they cannot see. */}
      <div className={'sh-rail' + (open ? ' open' : '')} inert={immersive || undefined}>
        <div
          className="sh-mark"
          tabIndex={0}
          role="button"
          aria-expanded={mode === 'multi' ? scope : undefined}
          onKeyDown={(e) => {
            if (e.key === 'Enter' || e.key === ' ') {
              e.preventDefault()
              e.currentTarget.click()
            }
          }}
          onClick={() => {
            if (mode !== 'multi') return toggle()
            setScope((s) => !s)
            tipOff()
          }}
          // The tooltip names what the popover already says, so it steps aside
          // rather than sitting on top of it.
          onMouseEnter={(e) => {
            if (scope) return
            tipOn(mode === 'multi' ? `${org} · ${team?.name ?? ''}` : org)(e)
          }}
          onMouseLeave={tipOff}
        >
          <div className="sh-sigil">{org.slice(0, 2).toUpperCase()}</div>
          <div className="sh-scope">
            <b>{org}</b>
            <span>{mode === 'multi' ? (team?.name ?? '') : 'local'}</span>
          </div>
        </div>

        {scope && (
          // Always the in-rail presentation: it is a region of the rail that
          // pushes the nav down, so there is nothing behind it to separate from
          // and no haze to draw.
          <div className="sh-pop in">
            <div
              className="sh-popo"
              role="button"
              tabIndex={0}
              title="Overview"
              // It already names the scope you are looking at, so "go to the
              // overview of this scope" is what clicking it means.
              onClick={(e) => {
                e.stopPropagation()
                setScope(false)
                go('overview')
              }}
              onKeyDown={enter(() => {
                setScope(false)
                go('overview')
              })}
            >
              <b>{org}</b>
              <span>{team?.name ?? ''}</span>
            </div>
            <div className="sh-poph">TEAMS</div>
            {/* Offline, the list is unknowable. Showing the last one fetched
                would offer a switch that cannot happen, and the header above
                already names where you are — so the list states its absence and
                nothing here can be clicked. */}
            {offline ? (
              <div className="sh-popdead">unavailable offline</div>
            ) : (
              <div className="sh-poplist">
                <div
                  className="sh-spine"
                  style={{ height: `${(teams.length || 1) * 25 - 12}px` }}
                />
                {teams.map((t, i) => (
                  <div
                    key={t.id}
                    className={'sh-popr' + (t.id === team?.id ? ' on' : '')}
                    style={{ '--i': i } as CSSProperties}
                    tabIndex={0}
                    role="button"
                    aria-current={t.id === team?.id ? 'true' : undefined}
                    onKeyDown={enter(() => {
                      setScope(false)
                      onTeamChange?.(t.id)
                    })}
                    onClick={() => {
                      setScope(false)
                      onTeamChange?.(t.id)
                    }}
                  >
                    {t.name}
                  </div>
                ))}
              </div>
            )}
          </div>
        )}

        <div className="sh-rule" />

        <div
          className={'sh-queue' + (route === 'queue' ? ' on' : '')}
          onClick={() => go('queue')}
          tabIndex={0}
          role="link"
          aria-current={route === 'queue' ? 'page' : undefined}
          onKeyDown={enter(() => go('queue'))}
          onMouseEnter={tipOn(queueTip)}
          onMouseLeave={tipOff}
          onFocus={tipOnKeyFocus(queueTip)}
          onBlur={tipOff}
        >
          <div className="sh-qico">
            <Ico d="inbox" size={17} />
          </div>
          <div className="sh-qtext">
            <b key={String(queued)} className="sh-tick">
              {readout(queued)}
            </b>{' '}
            in queue
          </div>
          <div className="sh-qchev">
            <Ico d="chev" size={12} />
          </div>
        </div>

        <div className="sh-rule" />

        <div className={'sh-nav' + (more && !deep ? ' more' : '')}>
          <div className={'sh-cols' + (deep ? ' deep' : '')}>
            <div className="sh-col" ref={colref}>
              {secs.map((s, i) => (
                <div key={s.id}>
                  {i === 0 ? (
                    <div className="sh-sec first">{s.name}</div>
                  ) : (
                    <>
                      <div className="sh-secline" />
                      <div className="sh-sec">{s.name}</div>
                    </>
                  )}
                  {s.rows.map(row)}
                </div>
              ))}
            </div>
            <div className="sh-col">
              <div
                className="sh-back"
                onClick={() => {
                  setTip(null)
                  setDeep(null)
                }}
                role="button"
                tabIndex={0}
                onKeyDown={enter(() => setDeep(null))}
                onMouseEnter={tipOn(deepRow ? `Back to ${deepRow.name}` : 'Back')}
                onMouseLeave={tipOff}
                onFocus={tipOnKeyFocus(deepRow ? `Back to ${deepRow.name}` : 'Back')}
                onBlur={tipOff}
              >
                <Ico d="back" size={13} />
                {/* Collapsed, the rail is 52px wide and the parent's name does
                    not fit — it hides the same way every other label does,
                    leaving the arrow, which is the only part that has to be
                    there. */}
                <span className="sh-name">{deepRow ? deepRow.name : ''}</span>
              </div>
              {deepRow?.children?.map((c) => (
                <div
                  key={c.id}
                  className={'sh-row' + (route === c.id ? ' on' : '')}
                  onClick={() => go(c.id)}
                  tabIndex={0}
                  role="link"
                  aria-current={route === c.id ? 'page' : undefined}
                  onKeyDown={enter(() => go(c.id))}
                  // Same as the top-level rows: collapsed, the glyph is all
                  // there is, so it has to be able to say its own name.
                  onMouseEnter={tipOn(c.name)}
                  onMouseLeave={tipOff}
                  onFocus={tipOnKeyFocus(c.name)}
                  onBlur={tipOff}
                >
                  <div className="sh-ico">
                    <Ico d={c.icon || deepRow.icon} />
                  </div>
                  <div className="sh-name">{c.name}</div>
                </div>
              ))}
            </div>
          </div>
        </div>

        <div className="sh-foot">
          {/* Stated once, in words, at the foot. Six readouts each announcing
              their own failure is six copies of one fact. */}
          {offline && (
            <div
              className="sh-offrow"
              role="status"
              tabIndex={0}
              onMouseEnter={tipOn('Connection lost · retrying')}
              onMouseLeave={tipOff}
              onFocus={tipOnKeyFocus('Connection lost · retrying')}
              onBlur={tipOff}
            >
              <div className="sh-ico sh-offico">
                <Ico d="offline" size={16} />
              </div>
              <div className="sh-name">Connection lost</div>
              <div className="sh-tail sh-offtail">retrying</div>
            </div>
          )}
          {row({ id: 'settings', name: 'Settings', icon: 'settings' })}
          <div
            className="sh-row sh-toggle"
            onClick={toggle}
            role="button"
            tabIndex={0}
            aria-label={open ? 'Collapse navigation' : 'Expand navigation'}
            onKeyDown={enter(toggle)}
            onMouseEnter={tipOn('Expand · ⌘\\')}
            onMouseLeave={tipOff}
          >
            <div className="sh-ico">
              <Ico d={open ? 'narrow' : 'wide'} />
            </div>
            <div className="sh-name">Collapse</div>
            <div className="sh-tail">⌘\</div>
          </div>
          {open && (
            <div className={'sh-youwrap' + (me ? ' on' : '')}>
              <div className="sh-you in">{youKids()}</div>
            </div>
          )}
          <div
            className={'sh-me' + (me ? ' on' : '')}
            tabIndex={0}
            role="button"
            aria-expanded={me}
            onKeyDown={(e) => {
              if (e.key === 'Enter' || e.key === ' ') {
                e.preventDefault()
                e.currentTarget.click()
              }
            }}
            onClick={() => {
              setMe((s) => !s)
              tipOff()
            }}
            onMouseEnter={(e) => {
              if (me) return
              tipOn(mode === 'multi' && user ? user.name : 'Preferences')(e)
            }}
            onMouseLeave={tipOff}
          >
            {/* Local mode has no user — no auth, no email, no grants, no sign
                out. Showing a fake person to a single local developer is the
                same mistake as showing them an empty Organization section. */}
            {mode === 'multi' && user ? (
              <div className="sh-av">{user.name.slice(0, 2).toUpperCase()}</div>
            ) : (
              <div className="sh-av sh-avp">
                <Ico d="prefs" size={15} />
              </div>
            )}
            {mode === 'multi' && user ? (
              <div className="sh-name">
                {user.name}
                <span className="sh-mesub">{user.email}</span>
              </div>
            ) : (
              <div className="sh-name">Preferences</div>
            )}
          </div>
        </div>
      </div>

      {tip && !open && (
        <div className="sh-tip" style={{ top: tip.y }}>
          {tip.name}
        </div>
      )}

      <div className="sh-main">
        <div className="sh-head" inert={immersive || undefined}>
          {/* Crumbs, title and subtitle share a baseline inside one group, and
              the group is centered in the band. Centering them individually
              lines up their boxes rather than their type, and different sizes
              then sit at different heights — 11px mono always reads high
              against 15px sans. */}
          <div className="sh-headline">
            {crumbs.map((c, i) => (
              <Fragment key={i}>
                <div className="sh-crumb" onClick={c.onClick}>
                  {c.name}
                </div>
                <div className="sh-sep">/</div>
              </Fragment>
            ))}
            {onTitleSave ? (
              <TitleField title={title} onSave={onTitleSave} />
            ) : (
              <div className="sh-title">{title}</div>
            )}
            {subtitle && <div className="sh-sub">{subtitle}</div>}
          </div>
          <div className="sh-act">
            {actions}
            <div
              className="sh-k"
              onClick={() => setPal(true)}
              tabIndex={0}
              role="button"
              onKeyDown={enter(() => setPal(true))}
            >
              search<span>⌘K</span>
            </div>
          </div>
        </div>
        <div className="sh-body">{children}</div>
      </div>

      {pal && (
        <>
          <div className="sh-scrim" onClick={() => setPal(false)} />
          <div className="sh-pal">
            <input
              ref={qref}
              className="sh-palq"
              placeholder="Go to…"
              aria-label="Go to"
              value={q}
              onChange={(e) => {
                setQ(e.target.value)
                setSel(0)
              }}
              onKeyDown={(e) => {
                if (e.key === 'ArrowDown') {
                  e.preventDefault()
                  setSel((s) => Math.min(s + 1, hits.length - 1))
                } else if (e.key === 'ArrowUp') {
                  e.preventDefault()
                  setSel((s) => Math.max(s - 1, 0))
                } else if (e.key === 'Enter' && hits[sel]) {
                  go(hits[sel].id)
                }
              }}
            />
            <div className="sh-pallist">
              {hits.length ? (
                hits.map((h, i) => (
                  <div
                    key={h.id}
                    className={'sh-palrow' + (i === sel ? ' on' : '')}
                    onMouseEnter={() => setSel(i)}
                    onClick={() => go(h.id)}
                  >
                    <div className="sh-palname">{h.name}</div>
                    <div className="sh-palsec">{h.sec}</div>
                  </div>
                ))
              ) : (
                <div className="sh-palnone">nothing by that name</div>
              )}
            </div>
          </div>
        </>
      )}
    </div>
  )
}

export default Shell
