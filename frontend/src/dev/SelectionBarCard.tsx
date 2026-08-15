import { useState } from 'react'
import SelectionBar from '../ui/selectionbar/SelectionBar'

// The verb bar, mounted over a stand-in for the table it floats above. The
// mount is `position: relative` because `barPosition="absolute"` needs a
// positioned ancestor — the same requirement a real table has.

const ROLES = [
  { id: 'admin', name: 'Team admin', note: 'can add, remove and configure' },
  { id: 'member', name: 'Member', note: 'can see everything, change nothing' },
  { id: 'org', name: 'Organization admin', note: 'set in Organization', blocked: true },
]

export function SelectionBarCard() {
  const [count, setCount] = useState(2)
  const [role, setRole] = useState('member')
  const [last, setLast] = useState<string | null>(null)

  return (
    <div className="gal-card">
      <div className="gal-cardhead">
        <span>SelectionBar</span>
        <span className="gal-route">{last ? last : 'role: ' + role}</span>
      </div>

      <p className="gal-note">
        The bar <em>is</em> the control — no verb gets its own button inside it, and the hold
        measurement crosses all of it. Every verb runs through that one gesture with{' '}
        <code>holdMs</code> as the pressure: <strong>Watch</strong> is 0 and fires on press,{' '}
        <strong>Hold to remove</strong> is 900ms and says so in its label, because a control that
        ignores a click reads as broken.
      </p>

      <div className="gal-chips">
        <button type="button" className="gal-chip" onClick={() => setCount((c) => c + 1)}>
          select another
        </button>
        <button
          type="button"
          className="gal-chip"
          onClick={() => setCount((c) => Math.max(1, c - 1))}
        >
          deselect one
        </button>
        <button type="button" className="gal-chip" onClick={() => setLast(null)}>
          reset
        </button>
      </div>

      <p className="gal-note">
        Open the picker: a stem grows out of a terminus X, ticks extend and labels arrive along
        them, separated by a soft-edged blur field that follows the ragged text edge. No fill, no
        rectangle, no shadow — a dropdown panel is the one shape this system does not have. The noun
        stays visible while it is open, because a word leaves a hole where the thing you clicked
        was.
      </p>

      {/* A stand-in for the table the bar floats over. The bar needs a
          positioned ancestor for barPosition="absolute", which is what
          .selbar-mount provides in a real page. */}
      <div className="gal-barmount">
        <span className="gal-spec-tag">the table this is floating over</span>
        <SelectionBar
          count={count}
          picker={{
            label: 'Role',
            value: role,
            options: ROLES,
            onPick: (o) => setRole(o.id),
            onRevert: () => setRole(role),
          }}
          actions={[
            { label: 'Watch', onSelect: () => setLast('watched ' + count) },
            {
              label: 'Stop watching',
              tone: 'bad',
              holdMs: 900,
              onSelect: () => setLast('unwatched ' + count),
            },
          ]}
          danger={{ label: 'Hold to remove', onConfirm: () => setLast('removed ' + count) }}
          onDismiss={() => setLast('selection cleared')}
        />
      </div>

      <p className="gal-note">
        Pick a role and the bar becomes the undo window — the change lands immediately and you get
        ten seconds to take it back. A pending state that has not applied yet makes the table lie
        for the length of the window, and confirming a reversible edit is what trains people to
        dismiss the dialogs that matter. Which is what makes the hold on the destructive verb work.
      </p>
      <p className="gal-note">
        <strong>Organization admin</strong> is listed and blocked rather than hidden: it states why
        it cannot be chosen. Hiding it leaves the reader wondering whether the product forgot.
      </p>
    </div>
  )
}

export default SelectionBarCard
