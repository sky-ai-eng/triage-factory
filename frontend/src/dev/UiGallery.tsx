// /dev/ui — the design system, mounted in isolation.
//
// The handoff bundle ships a .card.html harness per component, and the
// instruction that came with it was to open the harness and toggle through the
// states BEFORE reading any code, because the behaviour across states is the
// design. This is that harness, in-repo: one place where every ui component is
// mounted with hand-written props, no network, no auth, no org.
//
// It is deliberately reachable before useDeploymentConfig runs, so the design
// system can be reviewed with no backend at all. That also makes it the
// fastest check that the token layer is intact after a change.
//
// DEV only. main.tsx guards the import behind import.meta.env.DEV, which Vite
// statically replaces, so neither this file nor its stylesheet reaches a
// production bundle.
//
// The specimens below read their values back off the document with
// getComputedStyle rather than repeating them from the stylesheet. A specimen
// that prints what it was told would agree with a broken token layer; one that
// reads the resolved value disagrees, and disagreeing is the whole job.

import { useCallback, useEffect, useState } from 'react'
import AcquireCard from './AcquireCard'
import CratePileCard from './CratePileCard'
import FlapCountCard from './FlapCountCard'
import PrimitivesCard from './PrimitivesCard'
import ScanCard from './ScanCard'
import SelectionBarCard from './SelectionBarCard'
import ShellCard from './ShellCard'
import SourcePartsCard from './SourcePartsCard'
import TableCard from './TableCard'
import TeamPartsCard from './TeamPartsCard'
import TooltipCard from './TooltipCard'
import './gallery.css'

type Section = 'color' | 'type' | 'elevation' | 'space' | 'components'

const SECTIONS: Array<{ id: Section; label: string }> = [
  { id: 'color', label: 'Color' },
  { id: 'type', label: 'Type' },
  { id: 'elevation', label: 'Elevation' },
  { id: 'space', label: 'Space & radius' },
  { id: 'components', label: 'Components' },
]

/** Resolve a custom property against the document root, as the browser sees it. */
function resolved(token: string): string {
  if (typeof window === 'undefined') return ''
  return getComputedStyle(document.documentElement).getPropertyValue(token).trim()
}

function Swatch({ token }: { token: string }) {
  // Re-read on every render rather than memoising: the theme toggle changes
  // what these resolve to, and a cached value would quietly show the old one.
  const value = resolved(token)
  return (
    <div className="gal-sw">
      <div className="gal-swatch" style={{ background: `var(${token})` }} />
      <div className="gal-meta">
        <span className="gal-name">{token.replace('--color-', '')}</span>
        <span className="gal-val" data-missing={value ? undefined : ''} title={value}>
          {value || 'unresolved'}
        </span>
      </div>
    </div>
  )
}

function Ramp({ tokens }: { tokens: string[] }) {
  return (
    <div className="gal-ramp">
      {tokens.map((t) => (
        <Swatch key={t} token={t} />
      ))}
    </div>
  )
}

const TYPE_SCALE = [
  '--text-page-title',
  '--text-section',
  '--text-column',
  '--text-body',
  '--text-card-title',
  '--text-ui',
  '--text-secondary',
  '--text-reported',
  '--text-readout',
  '--text-label',
  '--text-label-sm',
]

const ELEVATION = [
  '--shadow-edge',
  '--shadow-edge-strong',
  '--shadow-edge-measure',
  '--shadow-edge-alarm',
  '--shadow-float',
  '--shadow-glow',
]

const RADII = ['--radius-nested', '--radius-inflow', '--radius-float', '--radius-pill']

export default function UiGallery() {
  const [section, setSection] = useState<Section>('color')
  const [dark, setDark] = useState(() => document.documentElement.classList.contains('dark'))

  // The gallery owns the class while it is mounted so both grounds can be
  // checked without leaving the page. It does NOT write lib/theme's storage
  // key — inspecting dark mode here should not change the user's setting.
  useEffect(() => {
    const root = document.documentElement
    const had = root.classList.contains('dark')
    root.classList.toggle('dark', dark)
    // Braced: classList.toggle returns a boolean, and a cleanup function is
    // typed to return void or a destructor.
    return () => {
      root.classList.toggle('dark', had)
    }
  }, [dark])

  const toggle = useCallback(() => setDark((d) => !d), [])

  return (
    <div className="gal">
      <nav className="gal-rail">
        <div className="gal-mark">Design system</div>
        {SECTIONS.map((s) => (
          <button
            key={s.id}
            type="button"
            className="gal-nav"
            data-on={section === s.id ? '' : undefined}
            aria-current={section === s.id ? 'page' : undefined}
            onClick={() => setSection(s.id)}
          >
            {s.label}
          </button>
        ))}
        <div className="gal-foot">
          <button type="button" className="gal-theme" onClick={toggle}>
            {dark ? '◐ dark' : '◑ light'}
          </button>
        </div>
      </nav>

      <main className="gal-main">
        {section === 'color' && (
          <>
            <h1 className="gal-h">Color</h1>
            <p className="gal-sub">
              Three hues and a neutral ramp. A token is the same hue in light and dark, at the
              lightness that ground demands — the two modes are lighting conditions on one set of
              materials, never two languages. Toggle the ground in the rail and nothing should
              change hue.
            </p>

            <h2 className="gal-sec">Ground — layer 01</h2>
            <Ramp tokens={['--color-ground', '--color-raised', '--color-sunk']} />

            <h2 className="gal-sec">Ink — four steps, no more</h2>
            <Ramp tokens={['--color-ink-1', '--color-ink-2', '--color-ink-3', '--color-ink-4']} />

            <h2 className="gal-sec">Structure — layer 02, drawn never lit</h2>
            <Ramp
              tokens={[
                '--color-line-1',
                '--color-line-2',
                '--color-tint-1',
                '--color-tint-2',
                '--color-tint-3',
              ]}
            />

            <h2 className="gal-sec">Measurement — layer 03</h2>
            <Ramp
              tokens={[
                '--color-warm',
                '--color-warm-1',
                '--color-warm-2',
                '--color-warm-3',
                '--color-warm-4',
              ]}
            />

            <h2 className="gal-sec">Emission — layer 04, an agent is working</h2>
            <p className="gal-note">
              Nothing else, ever. If something on screen is emitting and no agent is running, that
              is a bug in the screen, not a taste question.
            </p>
            <Ramp tokens={['--color-cool', '--color-cool-glow']} />

            <h2 className="gal-sec">Failure</h2>
            <p className="gal-note">Applied to a container&rsquo;s frame, never as a badge.</p>
            <Ramp tokens={['--color-alarm', '--color-alarm-1', '--color-alarm-2']} />

            <h2 className="gal-sec">Inverted chip</h2>
            <Ramp tokens={['--color-inverse', '--color-inverse-ink', '--color-focus']} />
          </>
        )}

        {section === 'type' && (
          <>
            <h1 className="gal-h">Type</h1>
            <p className="gal-sub">
              Sans for human content, mono for machine data. The split is semantic: if a number came
              from the system it is mono and tabular. Tracking scales inversely with size — the
              smaller the label, the wider the letterspacing.
            </p>

            <h2 className="gal-sec">Scale</h2>
            <div className="gal-type">
              {TYPE_SCALE.map((t) => (
                <div className="gal-type-row" key={t}>
                  <span className="gal-type-tag">
                    {t.replace('--text-', '')} · {resolved(t) || '—'}
                  </span>
                  <span style={{ fontSize: `var(${t})` }}>Delegated run awaiting review</span>
                </div>
              ))}
            </div>

            <h2 className="gal-sec">Machine voice — tabular, aligned</h2>
            <div className="gal-type">
              <div className="gal-type-row">
                <span className="gal-type-tag">font-mono</span>
                <span className="tf-tabular">1,048,576 · 00:41:07 · $12.40 · SKY-1183</span>
              </div>
              <div className="gal-type-row">
                <span className="gal-type-tag">font-sans</span>
                <span>1,048,576 · 00:41:07 · $12.40 · SKY-1183</span>
              </div>
            </div>
          </>
        )}

        {section === 'elevation' && (
          <>
            <h1 className="gal-h">Elevation</h1>
            <p className="gal-sub">
              The single most load-bearing rule: in-flow content is borderless. Only something that
              genuinely floats above the plane gets a frame. In flow takes an inset hairline and no
              shadow; floating takes a real border and a cast.
            </p>
            <div className="gal-boxes">
              {ELEVATION.map((t) => (
                <div className="gal-box" key={t} style={{ boxShadow: `var(${t})` }}>
                  <span>{t.replace('--shadow-', '')}</span>
                </div>
              ))}
            </div>
            <p className="gal-note">
              <code>shadow-float</code> is the one to check after a token change: its light/dark
              difference lives in <code>--color-cast</code> rather than in the shadow itself,
              because Tailwind expands a <code>--shadow-*</code> value at build time and a literal
              baked there would never see <code>:root.dark</code>. Toggle the ground — the cast
              should deepen.
            </p>
          </>
        )}

        {section === 'space' && (
          <>
            <h1 className="gal-h">Space &amp; radius</h1>
            <p className="gal-sub">
              The product&rsquo;s real values, not a 4/8 grid snapped over them. Radius is small on
              purpose: 5px in flow, 4px for a nested interactive block, 10px only for something that
              genuinely floats.
            </p>

            <h2 className="gal-sec">Radius</h2>
            <div className="gal-boxes">
              {RADII.map((t) => (
                <div
                  className="gal-box"
                  key={t}
                  style={{ borderRadius: `var(${t})`, boxShadow: 'var(--shadow-edge-strong)' }}
                >
                  <span>
                    {t.replace('--radius-', '')} · {resolved(t) || '—'}
                  </span>
                </div>
              ))}
            </div>

            <h2 className="gal-sec">Space</h2>
            <div className="gal-type">
              {Array.from({ length: 11 }, (_, i) => `--space-${i + 1}`).map((t) => (
                <div className="gal-type-row" key={t}>
                  <span className="gal-type-tag">
                    {t.replace('--', '')} · {resolved(t) || '—'}
                  </span>
                  <span
                    style={{
                      display: 'block',
                      height: 10,
                      width: `var(${t})`,
                      background: 'var(--color-warm-3)',
                      borderRadius: 2,
                    }}
                  />
                </div>
              ))}
            </div>
          </>
        )}

        {section === 'components' && (
          <>
            <h1 className="gal-h">Components</h1>
            <p className="gal-sub">
              Every component in <code>src/ui/</code>, mounted with hand-written props and toggles
              for the states that matter. Nothing here talks to the network — that is the bar for
              living in the design system at all, and it is enforced by{' '}
              <code>eslint-rules/ui-no-app-imports.js</code> rather than asked for. Read the
              component&rsquo;s <code>.md</code> before changing it.
            </p>
            <AcquireCard />
            <PrimitivesCard />
            <TooltipCard />
            <ScanCard />
            <FlapCountCard />
            <CratePileCard />
            <SelectionBarCard />
            <TableCard />
            <TeamPartsCard />
            <SourcePartsCard />
            <ShellCard />
          </>
        )}
      </main>
    </div>
  )
}
