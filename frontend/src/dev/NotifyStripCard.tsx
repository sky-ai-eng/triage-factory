import { useRef, useState } from 'react'
import Shell from '../ui/shell/Shell'
import { NotifyStrip, type StripItem } from '../ui/notifystrip/NotifyStrip'
import { ToastLog, type ToastLogItem } from '../ui/toastlog/ToastLog'

// The NotifyStrip harness, with the shell around it. Raise an urgent item and
// watch the header and the page slide down together while the rail stays put;
// open it and the band drops to its read size; resolve it and the line leaves
// showing what it was. Fire a high arrival and its warm clock runs out over
// 8s. A toast fired meanwhile hangs below whatever the strip is showing.

type Demo = StripItem & { admin: boolean; act?: string }

const URGENT: Demo[] = [
  {
    id: 'u1',
    scope: 'ORG',
    title: "GitHub App can't be used",
    since: '1m',
    read: false,
    admin: true,
    act: 'Open GitHub access',
  },
  {
    id: 'u2',
    scope: 'ORG',
    title: 'Memory generation has no model',
    since: '1m',
    read: false,
    admin: true,
    act: 'Set a model',
  },
]
const HIGH: Demo[] = [
  {
    id: 'h1',
    scope: 'TEAM INFRA',
    title: "Linear team ENG can't be read",
    read: false,
    admin: true,
  },
  {
    id: 'h2',
    scope: 'YOU',
    title: 'Run 7f3a21 is paused: its sandbox was lost 3 times in a row',
    read: false,
    admin: false,
  },
]

export function NotifyStripCard() {
  const [admin, setAdmin] = useState(true)
  const [urgent, setUrgent] = useState<Demo[]>([])
  const [highs, setHighs] = useState<Demo[]>([])
  const [arrivals, setArrivals] = useState<Demo[]>([])
  const [toasts, setToasts] = useState<ToastLogItem[]>([])
  const [offline, setOffline] = useState(false)
  const [route, setRoute] = useState('board')
  const n = useRef(0)

  const visible = urgent.filter((u) => admin || !u.admin)
  const unread =
    visible.filter((u) => !u.read).length +
    highs.filter((h) => !h.read && (admin || !h.admin)).length
  const toast = (body: string) =>
    setToasts((t) => [...t, { id: String(++n.current), level: 'info', body, at: Date.now() }])

  const raise = (i: number) => {
    const u = URGENT[i]
    if (urgent.some((x) => x.id === u.id)) return
    const action = u.act ? { label: u.act, onSelect: () => toast(`${u.act} (demo)`) } : undefined
    setUrgent((xs) => [{ ...u, action }, ...xs])
  }
  const arrive = (i: number) => {
    const h = HIGH[i]
    if (!admin && h.admin) return
    const id = `${h.id}:${++n.current}`
    setHighs((xs) => [...xs, { ...h, id }])
    setArrivals((q) => [...q, { ...h, id }])
  }
  const open = (id: string) => {
    setUrgent((xs) => xs.map((x) => (x.id === id ? { ...x, read: true } : x)))
    setHighs((xs) => xs.map((x) => (x.id === id ? { ...x, read: true } : x)))
    setRoute('notifications')
  }

  return (
    <div className="gal-card">
      <div className="gal-cardhead">
        <span>NotifyStrip</span>
        <span className="gal-route">
          {`${visible.length} urgent · ${arrivals.length} arriving · ${unread} unread`}
        </span>
      </div>

      <p className="gal-note">
        <strong>One slot, two lines.</strong> Urgent stays until it resolves: a 40px band while
        unread, a 26px line once opened. A high arrival is a 26px line for 8s, its warm clock
        running out, and then only the band&rsquo;s count in the rail. Low items and notices never
        announce.
      </p>

      <div className="gal-chips">
        <button type="button" className="gal-btn" onClick={() => raise(0)} disabled={!admin}>
          urgent: GitHub App breaks
        </button>
        <button type="button" className="gal-btn" onClick={() => raise(1)} disabled={!admin}>
          urgent: memory model unset
        </button>
        <button type="button" className="gal-btn" onClick={() => visible[0] && open(visible[0].id)}>
          open the top one
        </button>
        <button type="button" className="gal-btn" onClick={() => setUrgent((xs) => xs.slice(1))}>
          resolve the top one
        </button>
      </div>
      <div className="gal-chips">
        <button type="button" className="gal-btn" onClick={() => arrive(0)} disabled={!admin}>
          high: Linear team unreadable
        </button>
        <button type="button" className="gal-btn" onClick={() => arrive(1)}>
          high: my run paused
        </button>
        <button type="button" className="gal-btn" onClick={() => toast('Slack channels saved')}>
          a toast
        </button>
        <button
          type="button"
          className="gal-chip"
          data-on={offline ? '' : undefined}
          onClick={() => setOffline((o) => !o)}
        >
          offline
        </button>
        <button
          type="button"
          className="gal-chip"
          data-on={admin ? undefined : ''}
          onClick={() => {
            setAdmin((a) => !a)
            setArrivals([])
          }}
        >
          view as member
        </button>
      </div>

      <div className="gal-viewport gal-stripstage">
        <Shell
          route={route}
          onRoute={setRoute}
          title={route === 'notifications' ? 'Notifications' : 'Board'}
          subtitle={route === 'notifications' ? 'the page is the next design' : 'infra'}
          needs={7}
          running={3}
          queued={12}
          offline={offline}
          notifications={unread}
          flags={{ notifications: true }}
          grants={admin ? ['org-admin', 'team-admin'] : []}
          banner={
            <NotifyStrip
              urgent={visible}
              arrival={arrivals[0] ?? null}
              onArrivalDone={(id) => setArrivals((q) => q.filter((a) => a.id !== id))}
              onOpen={open}
            />
          }
        >
          <div className="gal-page">
            <p>
              The page. A strip takes height from the page column, so the header and the page slide
              down together and the rail stays full height. Toasts hang below whatever the strip is
              showing.
            </p>
          </div>
        </Shell>
        <ToastLog items={toasts} onDone={(id) => setToasts((t) => t.filter((x) => x.id !== id))} />
      </div>

      <p className="gal-note">
        <strong>The band above the queue</strong> counts unread open urgent and high items. The bell
        stays ink; the warm parts are the count and the dot on the bell&rsquo;s shoulder, so the
        rail still has two warm marks: the queue and this. Offline, the count reads <code>--</code>{' '}
        and the dot goes.
      </p>
      <p className="gal-note">
        Members see only what is sent to them: the org&rsquo;s urgent conditions are for admins, so
        a member&rsquo;s strip stays empty until something of theirs arrives.
      </p>
    </div>
  )
}

export default NotifyStripCard
