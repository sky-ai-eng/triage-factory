import { useRef, useState } from 'react'
import Shell from '../ui/shell/Shell'
import { ToastLog, type ToastLevel, type ToastLogItem } from '../ui/toastlog/ToastLog'

// The ToastLog harness. Fire each level and watch the clock run down the
// row's foot; hover the panel and the clocks hold; press Escape and the top
// row leaves. The burst is the case worth reviewing: three rows in 240ms
// should read as one block growing downward, not three cards landing.
//
// The log is position: fixed, so the stage carries a transform to become its
// containing block — otherwise it would hang off the gallery's own viewport.

const COPY: Record<ToastLevel, string[]> = {
  success: ['Slack channels saved', 'Duplicated 3 prompts', 'Re-profiling started'],
  info: [
    'Steps after a deleted prompt are now an untriggered blueprint.',
    'Redrove 4 of 6 — the rest were no longer parked',
  ],
  warning: [
    'Auto-delegation paused: Review on open tripped the breaker (3 consecutive failures on this entity)',
  ],
  error: [
    'Could not claim the task.',
    'Could not move the task — please try again.',
    'Run 7f3a21 failed: the agent exited before writing its completion envelope.',
  ],
}

const LEVELS: ToastLevel[] = ['success', 'info', 'warning', 'error']

export function ToastLogCard() {
  const [items, setItems] = useState<ToastLogItem[]>([])
  const [interactive, setInteractive] = useState(true)
  const n = useRef({ id: 0, next: {} as Partial<Record<ToastLevel, number>> })

  const push = (level: ToastLevel, extra: Partial<ToastLogItem> = {}) => {
    const k = ((n.current.next[level] ?? -1) + 1) % COPY[level].length
    n.current.next[level] = k
    const id = String(++n.current.id)
    setItems((xs) => [...xs, { id, level, body: COPY[level][k], at: Date.now(), ...extra }])
  }
  const burst = () =>
    (['success', 'info', 'error'] as const).forEach((l, i) => setTimeout(() => push(l), i * 120))

  return (
    <div className="gal-card">
      <div className="gal-cardhead">
        <span>ToastLog</span>
        <span className="gal-route">
          {items.length + ' showing' + (interactive ? '' : ' · read-only')}
        </span>
      </div>

      <p className="gal-note">
        <strong>Replies only.</strong> A toast answers something the viewer just did. One panel,
        newest on top, 12px under the header&rsquo;s right end; past five rows the oldest leaves
        early.
      </p>

      <div className="gal-chips">
        {LEVELS.map((l) => (
          <button key={l} type="button" className="gal-btn" onClick={() => push(l)}>
            {l}
          </button>
        ))}
        <button
          type="button"
          className="gal-btn"
          onClick={() =>
            push('error', {
              title: 'Reject failed',
              body: 'Could not reject the pending PR. GitHub answered 422.',
            })
          }
        >
          titled error
        </button>
        <button
          type="button"
          className="gal-btn"
          onClick={() =>
            push('error', {
              body: 'Run 7f3a21 failed: the sandbox ran out of disk.',
              action: { label: 'open run', href: '#run-7f3a21', onSelect: () => {} },
            })
          }
        >
          with action
        </button>
        <button type="button" className="gal-btn" onClick={burst}>
          burst of 3
        </button>
        <button
          type="button"
          className="gal-chip"
          data-on={interactive ? undefined : ''}
          onClick={() => setInteractive((v) => !v)}
        >
          read-only
        </button>
      </div>

      <div className="gal-viewport gal-toaststage">
        <Shell
          route="board"
          title="Board"
          subtitle="infra"
          needs={7}
          running={3}
          grants={['org-admin', 'team-admin']}
        >
          <div className="gal-page">
            <p>The page. The log hangs 12px under the header&rsquo;s right end.</p>
          </div>
        </Shell>
        <ToastLog
          items={items}
          interactive={interactive}
          onDone={(id) => setItems((xs) => xs.filter((x) => x.id !== id))}
        />
      </div>

      <p className="gal-note">
        <strong>The clock is the timer.</strong> Success and info last 3s, warnings 6s, errors 10s.
        The line at each row&rsquo;s foot pauses while the pointer or focus is in the panel and
        while the tab is hidden or blurred. Errors differ only by the red &times; and a red clock.
      </p>
      <p className="gal-note">
        A click does nothing, so the text can be selected and copied. <kbd>Esc</kbd> dismisses the
        top row; press again for the next. Read-only takes away Escape and the hover hold. Under
        reduced motion rows appear and leave with an opacity change only, and the clock steps once a
        second.
      </p>
    </div>
  )
}

export default ToastLogCard
