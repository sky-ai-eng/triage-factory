import { useState } from 'react'
import SourceCard from '../ui/sourcecard/SourceCard'
import StatusRules from '../ui/statusrules/StatusRules'
import Dialog from '../ui/dialog/Dialog'

// The team page's remaining parts. Grouped because each is read in the context
// of that page rather than on its own.

export function TeamPartsCard() {
  const [dialog, setDialog] = useState<'flash' | 'quiet' | 'none' | null>(null)
  const [reading, setReading] = useState(false)
  const [kind, setKind] = useState<'normal' | 'destructive'>('destructive')
  const [interactive, setInteractive] = useState(true)
  const [answered, setAnswered] = useState<string | null>(null)

  return (
    <div className="gal-card">
      <div className="gal-cardhead">
        <span>SourceCard</span>
      </div>
      <p className="gal-note">
        One card, three surfaces, and the surface is the whole message: raised and bordered for a
        source this team has configured, sunk into the ground for one the organization has not
        connected, a dashed outline for one that does not exist yet.{' '}
        <strong>Only the configured card is clickable</strong> — there is nothing behind the other
        two, so neither takes a cursor or a hover.
      </p>
      <div className="gal-sources">
        <SourceCard
          name="GitHub"
          source="github"
          state="configured"
          scope="33 of 48 repositories"
          stats={[
            ['events · 7d', 164],
            ['became tasks', 22, 'warm'],
          ]}
          onClick={() => setAnswered('opened GitHub')}
        />
        <SourceCard
          name="Jira"
          source="jira"
          state="configured"
          scope="SKY · 2 projects"
          stats={[
            ['events · 7d', 88],
            ['became tasks', 11, 'warm'],
          ]}
          onClick={() => setAnswered('opened Jira')}
        />
        <SourceCard
          name="Slack"
          source="slack"
          state="unavailable"
          scope="not connected"
          note="An org admin connects Slack once for the whole organization. Until then this team has no channels to watch."
          actionLabel="Ask an org admin"
          requestKey="gallery:slack"
        />
        <SourceCard
          name="Schedule"
          state="soon"
          scope="coming soon"
          note="Runs the factory on a cadence you set, with no event to trigger it."
        />
      </div>
      <p className="gal-note">
        A live source wears its own brand mark; both inert states desaturate it and step the name
        back — one asset, two readings, no badge needed. Press <strong>Ask an org admin</strong>: it
        is replaced in place by a ticked receipt, recorded per user, and never offered again. A
        second request tells an admin nothing the first one didn&rsquo;t.
      </p>

      <div className="gal-cardhead" style={{ marginTop: 'var(--space-10)' }}>
        <span>StatusRules</span>
        <button
          type="button"
          className="gal-chip"
          data-on={interactive ? '' : undefined}
          onClick={() => setInteractive((i) => !i)}
        >
          {interactive ? 'admin' : 'member — read only'}
        </button>
      </div>
      <p className="gal-note">
        Every status the connection reports lives in one of four columns or in the tray. Drag one
        between columns, or to the tray to unmap it — or, without a mouse, click a tray chip to
        stage it and then click a column to land it. That path always existed for anyone who would
        rather not drag; it simply was not reachable.
      </p>
      <p className="gal-note">
        Flip to <strong>member</strong>: the drag, the staging and the grab cursor go together, and{' '}
        <em>nothing is greyed out</em>. The mapping answers &ldquo;where does our work come
        from&rdquo;, which a member legitimately needs — dimming it would hide information to signal
        a permission.
      </p>
      <div className="gal-boardmount">
        <StatusRules interactive={interactive} showProjects={false} />
      </div>

      <div className="gal-cardhead" style={{ marginTop: 'var(--space-10)' }}>
        <span>Dialog</span>
        <span className="gal-route">{answered ?? 'nothing answered'}</span>
      </div>
      <p className="gal-note">
        A decision the product cannot make for you — never &ldquo;are you sure?&rdquo;. It states
        what will actually happen, one line each, and marks what is lost. The build is
        Acquire&rsquo;s vocabulary at a dialog&rsquo;s pace: frame, flash, wireframe, empty,
        resolve.
      </p>
      <div className="gal-chips">
        <button
          type="button"
          className="gal-chip"
          data-on={kind === 'destructive' ? '' : undefined}
          onClick={() => setKind((k) => (k === 'destructive' ? 'normal' : 'destructive'))}
        >
          {kind}
        </button>
        {(['flash', 'quiet', 'none'] as const).map((b) => (
          <button key={b} type="button" className="gal-chip" onClick={() => setDialog(b)}>
            build: {b}
          </button>
        ))}
        <button type="button" className="gal-chip" onClick={() => setReading(true)}>
          a reading, held confirm
        </button>
      </div>
      <p className="gal-note">
        On a <strong>destructive</strong> dialog focus lands on <em>Cancel</em> and Enter does not
        confirm — the port&rsquo;s answer to the open question in <code>Dialog.md</code>. Everywhere
        else in this system an irreversible verb is a press-and-hold precisely so a reflex cannot
        fire it, and a focused Enter-bound Confirm would be that hazard restored. Tab to it and
        press it deliberately.
      </p>
      <p className="gal-note">
        The last chip is the same object with a <strong>reading</strong> in it — free markup between
        the body and the note, via <code>children</code> — and a confirm that must be{' '}
        <strong>held</strong> (<code>confirmHold</code>), so the destructive verb takes the gesture
        every other irreversible verb takes. <code>noConfirm</code> is the other half: a reading
        with nothing to decide, and Close as its one control.
      </p>

      <Dialog
        open={reading}
        build="none"
        width={460}
        title="deploy"
        body="tf_M2rTq8Wd… · sky-ai-eng"
        note="Expires by the org's 90-day cap, not the date you set. A request from any other address fails as an invalid token would."
        confirmLabel="Hold to revoke"
        confirmHold={900}
        cancelLabel="Close"
        onCancel={() => {
          setAnswered('closed the reading')
          setReading(false)
        }}
        onConfirm={() => {
          setAnswered('held to revoke')
          setReading(false)
        }}
      >
        <div
          style={{
            display: 'grid',
            gridTemplateColumns: 'repeat(3, 1fr)',
            gap: 'var(--space-3)',
            font: '500 17px var(--font-mono)',
            fontVariantNumeric: 'tabular-nums',
            color: 'var(--color-ink-1)',
          }}
        >
          <div>
            88d
            <div style={{ font: '400 9.5px var(--font-mono)', color: 'var(--color-ink-3)' }}>
              old · since 9 Jun
            </div>
          </div>
          <div>
            1d
            <div style={{ font: '400 9.5px var(--font-mono)', color: 'var(--color-ink-3)' }}>
              since last use
            </div>
          </div>
          <div style={{ color: 'var(--color-warm)' }}>
            2d
            <div style={{ font: '400 9.5px var(--font-mono)', color: 'var(--color-ink-3)' }}>
              left · expires 7 Sep
            </div>
          </div>
        </div>
      </Dialog>

      <Dialog
        open={dialog !== null}
        build={dialog ?? 'flash'}
        kind={kind}
        title={kind === 'destructive' ? 'Delete platform?' : 'Archive this project?'}
        body={
          kind === 'destructive'
            ? 'This cannot be undone.'
            : 'It stops polling and drops out of the board.'
        }
        consequences={[
          { text: 'Every task this team owns is unassigned', tone: 'loss' },
          { text: 'Repositories stay connected to the organization', tone: 'keep' },
          { text: 'Members keep their organization accounts', tone: 'keep' },
        ]}
        note="Nothing is removed from GitHub or Jira."
        confirmLabel={kind === 'destructive' ? 'Delete team' : 'Archive'}
        cancelLabel="Keep it"
        onCancel={() => {
          setAnswered('cancelled')
          setDialog(null)
        }}
        onConfirm={() => {
          setAnswered('confirmed')
          setDialog(null)
        }}
      />
    </div>
  )
}

export default TeamPartsCard
