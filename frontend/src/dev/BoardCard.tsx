import { useState } from 'react'
import { TaskCard } from '../components/board/TaskCard'
import AssigneePicker from '../components/board/AssigneePicker'
import type { Task, TeamBot, TeamMember } from '../types'

// The board card harness: one TaskCard in each of its states, on a lane the
// gallery draws by hand, each wearing the real assignee picker over a roster
// past the browse threshold. Nothing here talks to the network — the four
// picker verbs log to the row below — so the page recede, the card's lift
// above the scrim, the crate mark's idle still and the settled footer can all
// be reviewed without a backend behind them.

const PEOPLE = [
  'Aidan Allchin',
  'Priya Raman',
  'Marcus Webb',
  'Yuki Tanaka',
  'Sofia Marchetti',
  'Dan Okoro',
  'Hannah Lindqvist',
  'Raj Patel',
]
const MEMBERS: TeamMember[] = PEOPLE.map(
  (name, i) =>
    ({
      user_id: `u${i + 1}`,
      display_name: name,
      github_username: null,
      jira_account_id: null,
      role: 'member',
      is_current_user: i === 0,
    }) as TeamMember,
)
const BOT = { agent_id: 'a1', display_name: 'machinist' } as TeamBot

// The events the specimens carry, in the vocabulary `lib/eventDisplay` holds.
// Two of the four tones draw colour; `good` is deliberately absent from the
// set because the card draws it as ink.
const EV = {
  ci: { label: 'CI Failed', description: 'A CI check failed on a PR', tone: 'problem' as const },
  review: {
    label: 'Review Requested',
    description: 'Someone requested your review on a PR',
    tone: 'attention' as const,
  },
  commits: {
    label: 'New Commits',
    description: 'A tracked PR has new commits since the last poll',
    tone: 'neutral' as const,
  },
  conflicts: {
    label: 'Conflicts',
    description: 'A PR has merge conflicts',
    tone: 'problem' as const,
  },
  changes: {
    label: 'Changes Requested',
    description: 'A reviewer requested changes on a PR',
    tone: 'problem' as const,
  },
  atomic: {
    label: 'Now Actionable',
    description: 'All subtasks closed — parent ticket is now an atomic work unit',
    tone: 'neutral' as const,
  },
}

function task(id: string, title: string, over: Partial<Task> = {}): Task {
  return {
    id,
    title,
    status: 'queued',
    source: 'github',
    source_id: 'acme/api#761',
    source_url: 'https://github.com/acme/api/pull/761',
    scoring_status: 'scored',
    created_at: new Date(Date.now() - 4 * 3600_000).toISOString(),
    memory_pending: false,
    ...over,
  } as Task
}

export function BoardCard() {
  const [log, setLog] = useState<string[]>([])
  const say = (line: string) => setLog((l) => [line, ...l].slice(0, 4))
  const picker = (t: Task, readOnly = false) => (
    <AssigneePicker
      task={t}
      currentUserID="u1"
      members={MEMBERS}
      bot={BOT}
      onClaim={async () => say(`claim ${t.id}`)}
      onUnclaim={async () => say(`unassign ${t.id}`)}
      onDelegate={() => say(`delegate ${t.id}`)}
      onReassign={async (_, who) => say(`reassign ${t.id} → ${who}`)}
      readOnly={readOnly}
    />
  )
  const queued = task('t1', 'Serialize the cgroup read so two readers cannot split a line')
  const held = task('t2', 'Record actuals at teardown', { claimed_by_user_id: 'u1' })
  const working = task('t3', 'Reap the orphaned worktrees', {
    status: 'in_progress',
    claimed_by_agent_id: 'a1',
  })
  const done = task('t4', 'Confirm the fix across 50 runs', {
    status: 'done',
    claimed_by_agent_id: 'a1',
  })

  return (
    <div className="gal-card">
      <div className="gal-cardhead">
        <span>Board card</span>
      </div>

      <p className="gal-note">
        One card for every lane state. What a card shows follows from what it has: no elapsed, no
        activity and no result is what queued means. Open any assignee mark: the page recedes behind
        one body-level scrim, the card in hand lifts above it, and a click anywhere else closes the
        menu and does nothing else.
      </p>

      <div className="gal-specimens">
        <div className="gal-spec" style={{ width: 360 }}>
          <span className="gal-spec-tag">queued · carrying a draft PR back from a requeue</span>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
            <TaskCard
              title={queued.title}
              entity={queued.source_id}
              entityHref={queued.source_url}
              lifecycle="queued"
              age="4h ago"
              summary="Two readers hit the cgroup file at once and the second gets a partial line."
              event={EV.ci}
              artifacts={{ branch: 1, pulls: 1 }}
              pending={{ pulls: 1 }}
              href="/runs/c0"
              assigneeSlot={picker(queued)}
            />
            <TaskCard
              title={held.title}
              entity={held.source_id}
              entityHref={held.source_url}
              lifecycle="queued"
              age="1h ago"
              summary="The executor budget snapshot never sees what the runtime recorded."
              event={EV.review}
              assigneeSlot={picker(held)}
            />
          </div>
        </div>

        <div className="gal-spec" style={{ width: 360 }}>
          <span className="gal-spec-tag">working · blocked on a permission</span>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
            <TaskCard
              title={working.title}
              entity={working.source_id}
              entityHref={working.source_url}
              lifecycle="working"
              elapsed="08:22"
              command="Reading internal/delegate/teardown.go"
              summary="Worktrees left by crashed runs pile up under the state root."
              event={EV.commits}
              artifacts={{ branch: 1, comment: 2 }}
              href="/runs/c1"
              chain={{ done: 1, total: 3 }}
              assigneeSlot={picker(working)}
            />
            <TaskCard
              title="Rebuild the usage rollup"
              entity="SKY-412"
              entityHref="https://example.test/browse/SKY-412"
              lifecycle="working"
              liveState="idle"
              elapsed="0:12"
              command="Waiting for a run slot · 2 ahead"
              event={EV.conflicts}
              href="/runs/c2"
              permission={{
                command: 'psql -f migrations/0042_up.sql',
                count: 3,
                onAllow: () => say('allow'),
                onDeny: () => say('deny'),
              }}
              assigneeSlot={picker(
                task('t5', 'Rebuild the usage rollup', {
                  status: 'in_progress',
                  claimed_by_agent_id: 'a1',
                }),
              )}
            />
          </div>
        </div>

        <div className="gal-spec" style={{ width: 360 }}>
          <span className="gal-spec-tag">settled · read-only mark</span>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
            <TaskCard
              title={done.title}
              entity={done.source_id}
              entityHref={done.source_url}
              lifecycle="done"
              elapsed="4:12"
              summary="The mutex fix needs proving under load before it ships: fifty runs of the sampler, none of them splitting a line."
              event={EV.changes}
              artifacts={{ branch: 1, pulls: 1, comment: 3 }}
              href="/runs/c3"
              assigneeSlot={picker(done, true)}
            />
            <TaskCard
              title="Fold the two poller passes"
              entity="acme/api#702"
              entityHref="https://github.com/acme/api/pull/702"
              lifecycle="failed"
              summary="The GitHub and Jira pollers walk the same repos twice a cycle; one pass should serve both."
              event={EV.atomic}
              elapsed="2:06"
              href="/runs/c4"
              assigneeSlot={picker(
                task('t6', 'Fold the two poller passes', { status: 'done' }),
                true,
              )}
            />
          </div>
        </div>
      </div>

      <p className="gal-note" style={{ fontFamily: 'var(--font-mono)' }}>
        {log.length ? log.join(' · ') : 'the picker’s verbs land here'}
      </p>
    </div>
  )
}

export default BoardCard
