import { createPortal } from 'react-dom'
import { Dialog } from '../../ui/dialog/Dialog'
import { linearTeamNames } from './linearConnect'
import type { LinearSwitchPending } from './linearSwitchWarning'

/**
 * LinearSwitchDialog asks before the org's Linear credential is replaced while
 * tracked teams are private: the question useLinearSwitchWarning puts on
 * `pending`. Each team is a line of what the switch can lose, and the note says
 * where access is granted.
 *
 * It portals to <body>, because the settings groups sit inside a
 * backdrop-blurred section, which would hold a fixed overlay inside itself.
 */
export default function LinearSwitchDialog({ pending }: { pending: LinearSwitchPending | null }) {
  return pending ? createPortal(<SwitchDialog pending={pending} />, document.body) : null
}

function SwitchDialog({ pending: { to, teams, settle } }: { pending: LinearSwitchPending }) {
  const unless =
    to === 'app' ? 'unless it is shared with the app' : "unless the new key's owner is a member"
  return (
    <Dialog
      open
      title={teams.length > 1 ? 'Some tracked teams are private' : 'A tracked team is private'}
      body={
        to === 'app'
          ? 'Installed, Triage Factory acts as its own app user, which reads a private Linear team only once the team is shared with the app.'
          : 'A personal API key reads a private Linear team only when the person it belongs to is a member of the team.'
      }
      consequences={[
        ...teams.map((t) => ({
          text: `${linearTeamNames([t])}: changes stop reaching Triage Factory ${unless}`,
          tone: 'loss' as const,
        })),
        {
          text: 'Issues and tasks already tracked there stay as they are until access returns',
          tone: 'keep' as const,
        },
      ]}
      note={
        to === 'app'
          ? "Share the team on Linear's consent screen when it asks which private teams the app may read, or later on the app's page in Linear. The connection card names any tracked team the app can't read."
          : "The connection card names any tracked team the new key can't read."
      }
      confirmLabel={to === 'app' ? 'Continue to Linear' : 'Replace key'}
      onConfirm={() => settle(true)}
      onCancel={() => settle(false)}
    />
  )
}
