import { useState, useEffect, useRef, useCallback } from 'react'
import type { CSSProperties, DragEvent, KeyboardEvent, MouseEvent } from 'react'
import Checkbox from '../checkbox/Checkbox'
import './status-rules.css'

export type StatusColumnId = 'ready' | 'inprogress' | 'review' | 'done'
export type StatusRule = { members: string[]; primary: string | null }
export type StatusMap = Record<StatusColumnId, StatusRule>
export type JiraProject = { key: string; name: string; count: number; watched?: boolean }

export type StatusRulesProps = {
  /** The mapping to open with. Defaults to a worked example. */
  map?: StatusMap | null
  /** Every status the project has. Anything unmapped shows in the tray. */
  statuses?: string[] | null
  /** The project catalog the picker searches. Already permission-scoped. */
  projects?: JiraProject[] | null
  /** Show the project picker above the board. Off on a team sheet. */
  showProjects?: boolean
  /** A line under the board. */
  note?: string
  /** Run the build sequence: columns land left to right, then their chips. */
  build?: boolean
  /**
   * False makes the board readable but not operable — no drag, no staging, no
   * grab cursor, and no tab stops. A control a viewer may never use is absent,
   * not disabled.
   */
  interactive?: boolean
}

// StatusRules — the Jira board: statuses live in columns, you drag them between
// columns, and ★ marks the one TF writes back. Extracted from the settings sheet
// so the local install and a team sheet show the same instrument.
//
// Turn 9a/10: the unmapped tray. Every status the project has is either in a
// column or in the tray under the board — there is no hidden pool and no +
// menu, so adding and removing are one verb in two directions. The tray takes
// the height the columns are not using, up to three rows, then scrolls.

const STATUSES = [
  'Backlog',
  'Selected for Dev',
  'Ready',
  'In Progress',
  'Blocked',
  'In Review',
  'Code Review',
  'QA',
  'Done',
  'Closed',
  'Won\u2019t Do',
]
const COLS: Array<[StatusColumnId, string]> = [
  ['ready', 'READY'],
  ['inprogress', 'IN PROGRESS'],
  ['review', 'IN REVIEW'],
  ['done', 'DONE'],
]
const DEFAULT_MAP: StatusMap = {
  ready: { members: ['Ready', 'Selected for Dev'], primary: 'Ready' },
  inprogress: { members: ['In Progress'], primary: 'In Progress' },
  review: { members: ['In Review', 'Code Review'], primary: 'In Review' },
  done: { members: ['Done', 'Closed'], primary: 'Done' },
}
const EMPTY_MAP: StatusMap = {
  ready: { members: [], primary: null },
  inprogress: { members: [], primary: null },
  review: { members: [], primary: null },
  done: { members: [], primary: null },
}
// Name guesses for the empty board. Review runs before ready so "Ready for
// Review" lands where it belongs; "developing" rather than "dev" so "Selected
// for Dev" does not read as in-progress work.
const GUESS: Array<[StatusColumnId, RegExp]> = [
  ['review', /review|\bqa\b|verif|accept|testing/i],
  ['done', /done|closed|ship|complete|resolved|released/i],
  ['inprogress', /in progress|implement|doing|developing|building|\bwip\b/i],
  ['ready', /ready|selected|triage|groom|to do|todo/i],
]
const PREFERRED: Record<StatusColumnId, string> = {
  ready: 'Ready',
  inprogress: 'In Progress',
  review: 'In Review',
  done: 'Done',
}

// Which projects the board reads from. Jira Cloud can search this server-side
// and page it; Data Center returns every project the token can see in one call
// and the client filters. Either way the list is already permission-scoped.
const PROJECTS: JiraProject[] = (
  [
    ['PLAT', 'Platform', 1200, true],
    ['DEPLOY', 'Deploy pipeline', 840, true],
    ['INFRA', 'Infrastructure', 612, true],
    ['BUILD', 'Build system', 455, true],
    ['REL', 'Release train', 301, true],
    ['SRE', 'Reliability', 288, true],
    ['OBS', 'Observability', 176, true],
    ['QUEUE', 'Queue metrics', 151, true],
    ['AGENT', 'Agent runtime', 133, true],
    ['SDK', 'Client SDKs', 120, true],
    ['TOOL', 'Internal tooling', 96, true],
    ['MIG', 'Migrations', 74, true],
    ['WEB', 'Web app', 402, false],
    ['SUP', 'Support', 320, false],
    ['MOB', 'Mobile', 210, false],
    ['SEC', 'Security', 204, false],
    ['BILL', 'Billing', 188, false],
    ['ML', 'Model training', 112, false],
    ['DATA', 'Data platform', 98, false],
    ['GROW', 'Growth', 88, false],
    ['EDGE', 'Edge network', 83, false],
    ['SALES', 'Sales', 77, false],
    ['DES', 'Design system', 64, false],
    ['MKT', 'Marketing', 61, false],
    ['DOC', 'Documentation', 57, false],
    ['ONB', 'Onboarding', 45, false],
    ['HR', 'People', 41, false],
    ['EXP', 'Experiments', 39, false],
    ['FIN', 'Finance', 33, false],
    ['ARCH', 'Architecture', 29, false],
    ['LEGAL', 'Legal', 22, false],
  ] as Array<[string, string, number, boolean]>
).map(([key, name, count, watched]) => ({ key, name, count, watched }))

const short = (n: number) => (n >= 1000 ? (n / 1000).toFixed(1) + 'k' : String(n))

/** Enter and Space both activate, and Space must not scroll the board. */
const activates = (e: KeyboardEvent) => {
  if (e.key !== 'Enter' && e.key !== ' ' && e.key !== 'Spacebar') return false
  e.preventDefault()
  return true
}

export function StatusRules({
  map = null,
  statuses = null,
  projects = null,
  showProjects = true,
  note = '',
  build = true,
  interactive = true,
}: StatusRulesProps) {
  // Not interactive means not interactive-looking either: a grab cursor over a
  // chip that cannot be picked up is the board telling the reader something that
  // is not true. It also means no tab stops — see StatusRules.md.
  const readonly = !interactive
  const all = statuses || STATUSES
  const catalog = projects || PROJECTS
  const [rules, setRules] = useState<StatusMap>(() =>
    JSON.parse(JSON.stringify(map || DEFAULT_MAP)),
  )
  const [building, setBuilding] = useState(build)
  const [query, setQuery] = useState('')
  const [staged, setStaged] = useState<string | null>(null)
  const [peek, setPeek] = useState<{
    col: string
    name: string
    canonical: boolean
    x: number
    y: number
  } | null>(null)
  // Which column the pointer is currently over mid-drag, so the target says so.
  const [overCol, setOverCol] = useState<StatusColumnId | null>(null)
  // The browser's own drag image paints the chip onto an opaque plate, which
  // reads as a white rectangle around a rounded card on this ground. So the
  // native image is suppressed and we carry our own clone under the pointer.
  const ghost = useRef<{ el: HTMLElement; dx: number; dy: number; src: HTMLElement } | null>(null)
  const place = useRef((x: number, y: number) => {
    const g = ghost.current
    if (!g || (!x && !y)) return
    g.el.style.transform = 'translate(' + (x + g.dx) + 'px,' + (y + g.dy) + 'px)'
  }).current
  const follow = useRef((ev: globalThis.DragEvent) => {
    // Accepting the drag everywhere is what makes a release over dead space end
    // instantly: an unaccepted drop animates the drag image home first, and
    // dragend only fires when that animation finishes, about a second later.
    // Never call this on dragstart — preventDefault there cancels the drag.
    ev.preventDefault()
    place(ev.clientX, ev.clientY)
  }).current
  // Dead-space drops are swallowed here, and every drop ends the drag: a chip
  // that changed columns has been unmounted and remounted by then, and a removed
  // node never gets its dragend.
  const sink = useRef((ev: globalThis.DragEvent) => {
    ev.preventDefault()
    drop()
  }).current
  const drop = useRef(() => {
    document.removeEventListener('dragover', follow)
    document.removeEventListener('drop', sink)
    document.removeEventListener('dragend', drop, true)
    const g = ghost.current
    if (g) {
      g.src.style.opacity = ''
      ghost.current = null
    }
    // Sweep by marker rather than by reference: a clone whose dragend never
    // arrived is still on the page, and there is never a reason to keep one.
    document.querySelectorAll('[data-sr-ghost]').forEach((n) => n.remove())
  }).current
  const lift = (e: DragEvent<HTMLElement>, el: HTMLElement | null) => {
    if (!el) return
    drop()
    if (e.dataTransfer) {
      // An empty 1×1 element, not an Image: a data-URI image may not have
      // decoded by the time the drag starts, and the browser then shows its
      // broken-image glyph flying in from the corner.
      const shim = document.createElement('div')
      shim.setAttribute('data-sr-ghost', '')
      shim.className = 'sr-shim'
      document.body.appendChild(shim)
      e.dataTransfer.setDragImage(shim, 0, 0)
    }
    const r = el.getBoundingClientRect()
    const g = el.cloneNode(true) as HTMLElement
    g.setAttribute('data-sr-ghost', '')
    // The clone hangs off <body>, outside any scope the board sits in — so its
    // look is a class in status-rules.css reading :root's tokens, and the
    // reader's lighting is exactly what it picks up. It KEEPS the chip's own
    // classes (padding, type, the star) and adds .sr-ghost for what a floating
    // copy needs; the state attributes come off, so .sr-ghost's rules are not
    // outranked by a [data-canonical] fill. Only geometry is inline, because
    // only geometry is measured.
    g.className = el.className + ' sr-ghost'
    for (const a of ['data-canonical', 'data-building', 'data-staged', 'data-over'])
      g.removeAttribute(a)
    g.style.width = r.width + 'px'
    g.style.height = r.height + 'px'
    document.body.appendChild(g)
    ghost.current = { el: g, dx: r.left - e.clientX, dy: r.top - e.clientY, src: el }
    place(e.clientX, e.clientY)
    el.style.opacity = '.3'
    document.addEventListener('dragover', follow)
    document.addEventListener('drop', sink)
    document.addEventListener('dragend', drop, true)
  }
  const [more, setMore] = useState<Record<string, boolean>>({})
  const [watched, setWatched] = useState<string[]>(() =>
    catalog.filter((p) => p.watched).map((p) => p.key),
  )
  const [pq, setPq] = useState('')
  const [allChips, setAllChips] = useState(false)
  const chip = useRef<{ name: string; col: StatusColumnId | null } | null>(null)
  const scrollers = useRef<Record<string, HTMLElement | null>>({})

  // A column that scrolls says so by fading its last chip out — a mask, not a
  // gradient overlay, so it holds on whatever ground the panel gives it.
  const gauge = useCallback(() => {
    setMore((m) => {
      let next = m
      for (const k of Object.keys(scrollers.current)) {
        const el = scrollers.current[k]
        if (!el) continue
        const v = el.scrollHeight - el.clientHeight - el.scrollTop > 2
        if (!!m[k] !== v) {
          if (next === m) next = { ...m }
          next[k] = v
        }
      }
      return next
    })
  }, [])

  useEffect(() => {
    if (!build) return
    const t = setTimeout(() => setBuilding(false), 1000)
    return () => clearTimeout(t)
  }, [build])

  useEffect(() => {
    if (!staged) return
    const esc = (e: globalThis.KeyboardEvent) => {
      if (e.key === 'Escape') setStaged(null)
    }
    window.addEventListener('keydown', esc)
    return () => window.removeEventListener('keydown', esc)
  }, [staged])

  useEffect(() => {
    gauge()
    window.addEventListener('resize', gauge)
    return () => window.removeEventListener('resize', gauge)
  })

  const used: string[] = []
  for (const k of Object.keys(rules) as StatusColumnId[]) used.push(...rules[k].members)
  const pool = all.filter((n) => used.indexOf(n) < 0)
  const shown = query ? pool.filter((n) => n.toLowerCase().indexOf(query.toLowerCase()) >= 0) : pool
  const boardEmpty = used.length === 0
  // Nothing left to map: the tray keeps a shallow band so a chip dragged out of
  // a column still has somewhere to land, and drops the filter and the chip
  // area it no longer holds.
  const trayEmpty = pool.length === 0

  // to === null means the tray: a status that leaves every column is unmapped,
  // not deleted, so it stays visible and can come back.
  const move = useCallback(
    (name: string, from: StatusColumnId | null, to: StatusColumnId | null) => {
      if (from === to) return
      setRules((r) => {
        const next: StatusMap = JSON.parse(JSON.stringify(r))
        // Strip it from every column, not just the one it was dragged from: a
        // status belongs to exactly one column, and a staged tray chip has no
        // origin to trust.
        for (const k of Object.keys(next) as StatusColumnId[]) {
          if (k === to) continue
          if (next[k].members.indexOf(name) < 0) continue
          next[k].members = next[k].members.filter((m) => m !== name)
          // A column whose primary just left needs a new one, or the write target
          // silently becomes nothing.
          if (next[k].primary === name) next[k].primary = next[k].members[0] || null
        }
        if (to && next[to]) {
          if (next[to].members.indexOf(name) < 0) next[to].members.push(name)
          if (!next[to].primary) next[to].primary = name
        }
        return next
      })
    },
    [],
  )

  const suggest = useCallback(() => {
    const next: StatusMap = JSON.parse(JSON.stringify(EMPTY_MAP))
    for (const name of all) {
      for (const [col, re] of GUESS) {
        if (re.test(name)) {
          next[col].members.push(name)
          break
        }
      }
    }
    for (const k of Object.keys(next) as StatusColumnId[]) {
      const m = next[k].members
      next[k].primary = m.indexOf(PREFERRED[k]) >= 0 ? PREFERRED[k] : m[0] || null
    }
    setRules(next)
    setStaged(null)
  }, [all])

  const land = (id: StatusColumnId) => {
    if (staged) {
      move(staged, null, id)
      setStaged(null)
      return true
    }
    return false
  }
  const setPrimary = (id: StatusColumnId, name: string) => {
    setRules((r) => {
      const next: StatusMap = JSON.parse(JSON.stringify(r))
      next[id].primary = name
      return next
    })
  }

  const on = catalog.filter((p) => watched.indexOf(p.key) >= 0)
  const hits = pq
    ? catalog
        .filter((p) => (p.key + ' ' + p.name).toLowerCase().indexOf(pq.toLowerCase()) >= 0)
        .slice(0, 6)
    : []
  const typedKey = pq.trim().toUpperCase()
  const unlisted = pq && !catalog.some((p) => p.key === typedKey)
  const toggle = (key: string) =>
    setWatched((w) => (w.indexOf(key) >= 0 ? w.filter((k) => k !== key) : w.concat(key)))
  const chipsShown = allChips ? on : on.slice(0, 7)

  // A column only becomes a landing target once something is staged, so it is a
  // tab stop only then — an inert region in the tab order is noise.
  const colTab = () => (staged && !readonly ? 0 : undefined)

  return (
    <div className="sr" data-readonly={readonly || undefined}>
      {showProjects ? (
        <div className="sr-projects">
          <div className="sr-search-wrap">
            <span className="sr-search">
              <svg
                className="sr-search-icon"
                width="10"
                height="10"
                viewBox="0 0 24 24"
                fill="none"
                strokeWidth="1.8"
                strokeLinecap="round"
                aria-hidden="true"
              >
                <circle cx="11" cy="11" r="7" />
                <path d="m20 20-3.5-3.5" />
              </svg>
              <input
                value={pq}
                onChange={(e) => setPq(e.target.value)}
                placeholder={'search ' + catalog.length + ' projects, or type a key'}
                aria-label="Search Jira projects"
              />
              {pq ? (
                <button
                  type="button"
                  className="sr-clear"
                  onClick={() => setPq('')}
                  aria-label="Clear search"
                >
                  ×
                </button>
              ) : null}
            </span>
            {pq ? (
              <div className="sr-menu" role="listbox" aria-label="Matching projects">
                {hits.map((p) => {
                  const isOn = watched.indexOf(p.key) >= 0
                  return (
                    <div
                      key={p.key}
                      className="sr-opt"
                      role="option"
                      aria-selected={isOn}
                      tabIndex={0}
                      onClick={() => {
                        toggle(p.key)
                        setPq('')
                      }}
                      onKeyDown={(e) => {
                        if (activates(e)) {
                          toggle(p.key)
                          setPq('')
                        }
                      }}
                    >
                      <Checkbox checked={isOn} size={11} />
                      <span className="sr-opt-key">{p.key}</span>
                      <span className="sr-opt-name">{p.name}</span>
                      <span className="sr-opt-count">{short(p.count)}</span>
                    </div>
                  )
                })}
                {unlisted ? (
                  <div
                    className="sr-unlisted"
                    data-after={hits.length ? '' : undefined}
                    role="option"
                    aria-selected={false}
                    tabIndex={0}
                    onClick={() => {
                      toggle(typedKey)
                      setPq('')
                    }}
                    onKeyDown={(e) => {
                      if (activates(e)) {
                        toggle(typedKey)
                        setPq('')
                      }
                    }}
                  >
                    watch <span className="sr-unlisted-key">{typedKey}</span> by key
                  </div>
                ) : null}
              </div>
            ) : null}
          </div>

          <div className="sr-chips">
            {chipsShown.map((p) => (
              <button
                type="button"
                key={p.key}
                className="sr-pchip"
                onClick={() => toggle(p.key)}
                title={'Click to stop watching ' + p.key}
              >
                {p.key}
                <span className="sr-pchip-count">{short(p.count)}</span>
              </button>
            ))}
            {on.length > 7 ? (
              <button type="button" className="sr-more" onClick={() => setAllChips((a) => !a)}>
                {allChips ? 'fewer' : '+' + (on.length - 7) + ' more'}
              </button>
            ) : null}
            {on.length === 0 ? (
              <span className="sr-nowatch">no projects watched — nothing reaches the board</span>
            ) : null}
          </div>

          <div className="sr-head">
            <span className="sr-head-label">STATUS RULES</span>
            <span className="sr-spacer" />
            <span className="sr-head-count">across all {on.length}</span>
          </div>
        </div>
      ) : null}

      {/* The column IS the drop target, so it runs to the tray rather than
          stopping under its last chip — dropping into the empty space below a
          short column is the obvious move. */}
      <div className="sr-board" data-board-empty={boardEmpty || undefined}>
        {COLS.map(([id, label], ci) => {
          const rule = rules[id]
          const colAt = 0.06 + ci * 0.09
          return (
            <div
              key={id}
              className="sr-col"
              data-over={overCol === id || undefined}
              data-staged={staged && !readonly ? '' : undefined}
              data-building={building || undefined}
              style={{ '--at': colAt.toFixed(3) + 's' } as CSSProperties}
              role={staged && !readonly ? 'button' : undefined}
              tabIndex={colTab()}
              aria-label={staged ? 'Put ' + staged + ' in ' + label : undefined}
              onDragOver={(e) => {
                if (readonly) return
                e.preventDefault()
                if (overCol !== id) setOverCol(id)
              }}
              onDragLeave={(e) => {
                // Chips inside the column fire leave events of their own; only a
                // pointer that actually left the column's box clears the state.
                if (!e.currentTarget.contains(e.relatedTarget as Node))
                  setOverCol((c) => (c === id ? null : c))
              }}
              onDrop={(e) => {
                if (readonly) return
                e.preventDefault()
                setOverCol(null)
                if (chip.current) move(chip.current.name, chip.current.col, id)
                chip.current = null
              }}
              onClick={() => land(id)}
              onKeyDown={(e) => {
                if (staged && activates(e)) land(id)
              }}
            >
              <span className="sr-col-label">{label}</span>

              {/* A column that collects every status scrolls inside its own
                  track rather than growing over the tray. */}
              <div
                className="sr-scroll"
                data-more={more[id] || undefined}
                ref={(el) => {
                  scrollers.current[id] = el
                }}
                onScroll={gauge}
              >
                {rule.members.map((name, si) => {
                  const canonical = rule.primary === name
                  return (
                    <div
                      key={name}
                      className="sr-chip"
                      data-canonical={canonical || undefined}
                      data-building={building || undefined}
                      style={
                        { '--at': (colAt + 0.22 + si * 0.055).toFixed(3) + 's' } as CSSProperties
                      }
                      draggable={!readonly}
                      role={readonly ? undefined : 'button'}
                      tabIndex={readonly ? undefined : 0}
                      aria-label={
                        canonical
                          ? name + ' — the status TF writes back'
                          : name + ' — make this the write target'
                      }
                      onDragStart={(e) => {
                        chip.current = { name, col: id }
                        setPeek(null)
                        if (e.dataTransfer) e.dataTransfer.effectAllowed = 'move'
                        lift(e, e.currentTarget)
                      }}
                      onDragEnd={() => {
                        drop()
                        setOverCol(null)
                      }}
                      onMouseEnter={(e: MouseEvent<HTMLDivElement>) => {
                        // Columns are a quarter of a narrow panel, so most names
                        // are ellipsed. Hovering shows the whole one rather than
                        // asking for a tooltip delay.
                        const l = e.currentTarget.querySelector(
                          '[data-label]',
                        ) as HTMLElement | null
                        if (!l || l.scrollWidth <= l.clientWidth + 1) return
                        const r = e.currentTarget.getBoundingClientRect()
                        setPeek({ col: id, name, canonical, x: r.left, y: r.top })
                      }}
                      onMouseLeave={() => setPeek(null)}
                      onClick={(e) => {
                        e.stopPropagation()
                        if (land(id)) return
                        setPrimary(id, name)
                      }}
                      onKeyDown={(e) => {
                        if (!activates(e)) return
                        e.stopPropagation()
                        if (land(id)) return
                        setPrimary(id, name)
                      }}
                      title={
                        canonical
                          ? 'The status TF writes back'
                          : 'Click to make this the write target'
                      }
                    >
                      {canonical ? (
                        <span className="sr-star" aria-hidden="true">
                          ★
                        </span>
                      ) : null}
                      <span
                        className="sr-name"
                        data-label=""
                        // A name too long for its column fades out at the edge
                        // rather than ending in an ellipsis: the column is
                        // already narrow, and three dots spend four characters
                        // saying what the fade says with none. Measured, so a
                        // name that fits is never touched — the measurement sets
                        // an attribute and status-rules.css owns the mask.
                        ref={(el) => {
                          if (!el) return
                          if (el.scrollWidth > el.clientWidth + 1)
                            el.setAttribute('data-clipped', '')
                          else el.removeAttribute('data-clipped')
                        }}
                      >
                        {name}
                      </span>
                    </div>
                  )
                })}

                {rule.members.length === 0 ? <span className="sr-slot" /> : null}
              </div>
            </div>
          )
        })}
      </div>

      <div
        className="sr-tray"
        data-empty={trayEmpty || undefined}
        data-board-empty={boardEmpty || undefined}
        onDragOver={(e) => {
          if (!readonly) e.preventDefault()
        }}
        onDrop={(e) => {
          if (readonly) return
          e.preventDefault()
          if (chip.current) move(chip.current.name, chip.current.col, null)
          chip.current = null
        }}
      >
        <div className="sr-tray-head">
          <span className="sr-tray-label">NOT MAPPED</span>
          <span className="sr-spacer" />
          {trayEmpty ? (
            <span className="sr-tray-note">every status is mapped</span>
          ) : (
            <span className="sr-filter">
              <svg
                width="9"
                height="9"
                viewBox="0 0 24 24"
                fill="none"
                strokeWidth="2"
                strokeLinecap="round"
                aria-hidden="true"
              >
                <circle cx="11" cy="11" r="7" />
                <path d="m20 20-3.5-3.5" />
              </svg>
              <input
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                placeholder="filter"
                aria-label="Filter unmapped statuses"
              />
            </span>
          )}
        </div>

        {trayEmpty ? null : (
          <div className="sr-pool">
            {shown.map((name) => {
              const isStaged = staged === name
              return (
                <div
                  key={name}
                  className="sr-tchip"
                  data-staged={isStaged || undefined}
                  draggable={!readonly}
                  role={readonly ? undefined : 'button'}
                  tabIndex={readonly ? undefined : 0}
                  aria-pressed={readonly ? undefined : isStaged}
                  onDragStart={(e) => {
                    chip.current = { name, col: null }
                    setStaged(null)
                    if (e.dataTransfer) e.dataTransfer.effectAllowed = 'move'
                    lift(e, e.currentTarget)
                  }}
                  onDragEnd={() => {
                    drop()
                    setOverCol(null)
                  }}
                  onClick={() => setStaged((s) => (s === name ? null : name))}
                  onKeyDown={(e) => {
                    if (activates(e)) setStaged((s) => (s === name ? null : name))
                  }}
                  title="Drag to a column, or click and then click a column"
                >
                  {name}
                </div>
              )
            })}
            {shown.length === 0 ? (
              <span className="sr-nohit">no status matches {query}</span>
            ) : null}
          </div>
        )}

        {trayEmpty ? null : (
          <div className="sr-foot">
            <span className="sr-count">
              {pool.length} of {all.length}
            </span>
            <span className="sr-spacer" />
            {staged ? (
              <span className="sr-hint" role="status">
                pick a column for {staged}
              </span>
            ) : boardEmpty ? (
              <button type="button" className="sr-suggest" onClick={suggest}>
                Suggest a mapping
              </button>
            ) : null}
          </div>
        )}
      </div>

      {peek ? (
        <span className="sr-peek" style={{ left: peek.x - 1, top: peek.y - 1 }} aria-hidden="true">
          {peek.canonical ? <span className="sr-star">★</span> : null}
          {peek.name}
        </span>
      ) : null}

      {note ? <p className="sr-note">{note}</p> : null}
    </div>
  )
}

export default StatusRules
