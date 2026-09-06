import { useState } from 'react'
import Table from '../ui/table/Table'
import type { TableRow } from '../ui/table/Table'

// The table, with the two shapes the team page needs: a roster that can be
// grown from the table itself, and a source list whose verbs are bulk actions.
//
// The rows are invented. What is worth looking at is the behaviour — the fixed
// frame, the draft page, and the undo window — not the data.

const NAMES = [
  ['Aidan Allchin', 'aidan@allchin.com', 'admin', 41],
  ['Cass Moreau', 'cass@sky-ai.eng', 'member', 28],
  ['Bo Lindqvist', 'bo@sky-ai.eng', 'member', 19],
  ['Dara Okonkwo', 'dara@sky-ai.eng', 'admin', 17],
  ['Erin Vasquez', 'erin@sky-ai.eng', 'member', 12],
  ['Fen Zhao', 'fen@sky-ai.eng', 'member', 9],
  ['Gil Haddad', 'gil@sky-ai.eng', 'member', 6],
  ['Halle Nyström', 'halle@sky-ai.eng', 'member', 3],
] as const

const MEMBERS: TableRow[] = NAMES.map(([name, email, role, tasks], i) => ({
  id: String(i + 1),
  name,
  email,
  role,
  tasks,
  // A trail of bars, as fractions the cell scales.
  spark: Array.from({ length: 14 }, (_, d) => ((i * 7 + d * 5) % 11) / 11),
  lastMin: 40 + i * 260,
}))

const CANDIDATES = [
  { id: 'c1', name: 'Cassidy Bloom' },
  { id: 'c2', name: 'Casper Weiss' },
  { id: 'c3', name: 'Cassian Ferreira' },
  { id: 'c4', name: 'Cass Moreau', disabled: true, disabledNote: 'already on this team' },
  { id: 'c5', name: 'Rina Castellanos' },
]

export function TableCard() {
  const [rows, setRows] = useState(MEMBERS)
  const [log, setLog] = useState<string | null>(null)
  const [build, setBuild] = useState(0)
  const [opens, setOpens] = useState(false)

  return (
    <div className="gal-card">
      <div className="gal-cardhead">
        <span>Table</span>
        <span className="gal-route">{log ?? rows.length + ' rows'}</span>
      </div>

      <p className="gal-note">
        A page is a fixed frame, not a scroller: the row under your cursor never moves and the count
        below is always true. A short last page holds its height open, so the pager does not walk up
        the screen on page three. Sort by clicking a heading — blanks sort last in <em>both</em>{' '}
        directions, because no value is not a value below &lsquo;a&rsquo;.
      </p>

      <div className="gal-chips">
        <button type="button" className="gal-chip" onClick={() => setBuild((b) => b + 1)}>
          replay the build
        </button>
        <button
          type="button"
          className="gal-chip"
          onClick={() => {
            setRows(MEMBERS)
            setLog(null)
          }}
        >
          reset rows
        </button>
        <button
          type="button"
          className="gal-chip"
          data-on={opens ? '' : undefined}
          onClick={() => setOpens((o) => !o)}
        >
          rows open
        </button>
      </div>

      <p className="gal-note">
        Select a row or two. The verbs float over the table rather than lighting up a toolbar across
        the page from the checkboxes that produced them, and a verb the whole selection cannot use
        is <strong>dropped, never shown disabled</strong>. Everything applies immediately and holds
        the request for ten seconds — Undo means it is never sent, so there is no compensating write
        and no window where the server disagrees with the screen.
      </p>

      <div className="gal-tablemount">
        <Table
          key={build}
          build
          label={'MEMBERS · ' + rows.length}
          columns={[
            { key: 'name', label: 'NAME', type: 'identity' },
            { key: 'email', label: 'EMAIL', color: () => 'var(--color-ink-3)', drop: 1 },
            { key: 'role', label: 'ROLE', align: 'end' },
            { key: 'tasks', label: 'TASKS · 14D', align: 'end' },
            { key: 'spark', label: 'ACTIVITY', type: 'spark', width: '118px', drop: 2 },
            { key: 'lastMin', label: 'LAST', type: 'ago', drop: 1 },
          ]}
          rows={rows}
          pageSize={5}
          sortKey="tasks"
          sortDir={-1}
          barPosition="absolute"
          // With the chip on, a row click opens (logged here) and only the
          // checkbox selects — the shape a table of tokens needs, where every
          // row leads to its own sheet.
          onRowOpen={opens ? (row) => setLog('opened ' + String(row.name)) : null}
          add={{
            label: 'add teammate',
            placeholder: 'name or email — three characters',
            options: CANDIDATES,
            onAdd: (person) => {
              setRows((r) => [
                ...r,
                {
                  id: 'n' + person.id,
                  name: person.name,
                  email: '—',
                  role: 'member',
                  tasks: 0,
                  spark: [],
                  lastMin: 0,
                },
              ])
              setLog('added ' + person.name)
            },
          }}
          bar={{
            actions: [
              {
                id: 'watch',
                label: 'Watch',
                message: (n) => n + ' members watched',
              },
              {
                id: 'promote',
                label: 'Make admin',
                // Dropped from the bar when any selected row is already one.
                enabledFor: (sel) => sel.every((r) => r.role !== 'admin'),
                message: (n) => n + ' promoted',
              },
            ],
            danger: {
              label: 'Hold to remove',
              action: { id: 'remove', label: 'remove', message: (n) => n + ' members removed' },
            },
          }}
          mutate={(row, id) => {
            if (id === 'remove') return null
            if (id === 'promote') return { ...row, role: 'admin' }
            return row
          }}
          onCommit={(id, ids) => setLog(id + ' committed for ' + ids.length)}
        />
      </div>

      <p className="gal-note">
        Press <strong>+ add teammate</strong> and type <code>cas</code>. Nothing is offered until
        three characters — a directory dumped the moment the row opens invites picking a name off a
        list, and the reader is meant to know who they are adding. Stage two people: they leave the
        candidates and collect as pills along the bottom, so clearing the search does not lose them.
        Then press <kbd>esc</kbd>, or any numbered page — <em>leaving is what commits</em>, and the
        whole page is one reversible act.
      </p>
      <p className="gal-note">
        The pager gains a <code>+</code> to the left of page 1 while the draft is open, and loses it
        again on the way out: a pager reports where you can go, and there is nowhere to go until the
        row exists. The draft page is the same height as the page it replaced, so nothing moves.
      </p>
      <p className="gal-note">
        Narrow the window. Columns are measured in px and dropped one at a time, least important
        first — <code>drop: 1</code> before <code>drop: 2</code>, and a column with no{' '}
        <code>drop</code> never goes. Width is read from the table element rather than the window,
        so the same table is wide in a page and narrow in a column with no breakpoints to keep in
        sync.
      </p>
    </div>
  )
}

export default TableCard
