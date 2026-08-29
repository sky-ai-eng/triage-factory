import { useState } from 'react'
import Shell from '../ui/shell/Shell'
import type { Grant } from '../ui/shell/routes'

// The Shell harness — a direct port of the design bundle's shell.card.html, and
// the instruction that came with it: toggle through the grant permutations
// BEFORE reading any code, because the rail's behaviour across grants IS the
// design. A member in local mode reads six glyphs; an org admin on shared
// infrastructure reads eleven.
//
// The shell is mounted at real size inside a framed viewport rather than
// filling the gallery, so the rail's proportions read the way they will in the
// app instead of against whatever height this page happens to have.

const CHIPS: Array<{ key: Grant; label: string }> = [
  { key: 'org-admin', label: 'org admin' },
  { key: 'team-admin', label: 'team admin' },
  { key: 'fleet', label: 'fleet operator' },
]

export function ShellCard() {
  const [multi, setMulti] = useState(true)
  const [grants, setGrants] = useState<Grant[]>(['org-admin', 'team-admin'])
  const [marketplace, setMarketplace] = useState(true)
  const [governance, setGovernance] = useState(true)
  const [offline, setOffline] = useState(false)
  const [known, setKnown] = useState(true)
  const [route, setRoute] = useState('board')
  const [immersive, setImmersive] = useState(false)
  const [needs, setNeeds] = useState(7)

  const toggle = (g: Grant) =>
    setGrants((cur) => (cur.includes(g) ? cur.filter((x) => x !== g) : [...cur, g]))

  const chip = (on: boolean, label: string, onClick: () => void) => (
    <button
      key={label}
      type="button"
      className="gal-chip"
      data-on={on ? '' : undefined}
      onClick={onClick}
    >
      {label}
    </button>
  )

  return (
    <div className="gal-card">
      <div className="gal-cardhead">
        <span>Shell</span>
        <span className="gal-route">{route}</span>
      </div>

      <p className="gal-note">
        The rail is generated, never authored — availability is a function of deployment mode and
        grant, never of role name. Change what this account is and watch sections appear and vanish.
        A route the viewer cannot use is absent, not disabled, because a greyed row advertises a
        feature the reader cannot buy.
      </p>

      <div className="gal-chips">
        {chip(multi, multi ? 'multi' : 'local', () => setMulti((m) => !m))}
        {CHIPS.map((c) => chip(grants.includes(c.key), c.label, () => toggle(c.key)))}
        {chip(marketplace, 'marketplace', () => setMarketplace((m) => !m))}
        {chip(governance, 'governance', () => setGovernance((g) => !g))}
      </div>

      <p className="gal-note">
        Offline is not a loading state: every readout goes inert together, and the condition is
        stated once in words at the foot. A count that has not loaded yet reads the same{' '}
        <code>--</code>, because &ldquo;nothing needs you&rdquo; is a claim the rail should not make
        before it knows.
      </p>

      <div className="gal-chips">
        {chip(offline, 'lose the connection', () => setOffline((o) => !o))}
        {chip(known, known ? 'counts known' : 'counts not loaded', () => setKnown((k) => !k))}
        {chip(false, 'something needs you', () => setNeeds((n) => n + 1))}
        {chip(false, 'one settles', () => setNeeds((n) => Math.max(0, n - 1)))}
      </div>

      <p className="gal-note">
        A page can ask for the whole screen — the factory does it in cinematic mode. The chrome
        fades and stops taking input, including the tab order, but nothing changes size: a collapse
        would reflow the page, and a page immersive enough to ask is usually one that renders
        continuously.
      </p>

      <div className="gal-chips">
        {chip(immersive, 'page wants the screen', () => setImmersive((i) => !i))}
      </div>

      <div className="gal-viewport">
        <Shell
          mode={multi ? 'multi' : 'local'}
          grants={multi ? grants : []}
          flags={{ marketplace: marketplace && multi, governance }}
          offline={offline}
          org={multi ? 'sky-ai-eng' : 'Triage Factory'}
          team={{ id: 't-platform', name: 'platform' }}
          teams={[
            { id: 't-platform', name: 'platform' },
            { id: 't-infra', name: 'infra' },
            { id: 't-growth', name: 'growth' },
            { id: 't-docs', name: 'docs' },
          ]}
          route={route}
          onRoute={setRoute}
          immersive={immersive}
          title="Board"
          subtitle="everything this team is carrying"
          needs={known ? needs : null}
          running={known ? 3 : null}
          queued={known ? 18 : null}
          counts={known ? { repos: 8, pulls: 23, usage: '$41', fleet: 6, gov: 2 } : {}}
          user={multi ? { name: 'Aidan', email: 'aidan@allchin.com' } : null}
        >
          <div className="gal-page">
            <p>
              The shell owns the frame and nothing else — not this page&rsquo;s content, not its
              background, and not its loading state. A skeleton describes the shape of one
              page&rsquo;s own instruments, so every page defines its own; the frame draws
              immediately and never guesses at what is about to fill it.
            </p>
            <p>
              <kbd>⌘\</kbd> pins the rail and the preference survives a reload. <kbd>⌘K</kbd> opens
              the palette, which searches the same route table the rail is generated from, so it can
              never offer a page the rail is hiding. <kbd>Esc</kbd> closes whatever is open,
              innermost first.
            </p>
          </div>
        </Shell>
      </div>
    </div>
  )
}

export default ShellCard
