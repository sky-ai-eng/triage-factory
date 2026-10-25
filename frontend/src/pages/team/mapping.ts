import type { TableColumn } from '../../ui/table/Table'
import type { StatusMap } from '../../ui/statusrules/StatusRules'

// What the Jira and Linear source pages share: both put a status board on the
// left and a table of what can be watched on the right, and differ only in the
// vocabulary they read the board in.

/** The board with nothing in it: what the skeleton draws while nothing is
 *  watched. */
export const EMPTY_STATUS_MAP: StatusMap = {
  ready: { members: [], primary: null },
  inprogress: { members: [], primary: null },
  review: { members: [], primary: null },
  done: { members: [], primary: null },
}

/** A watched row's events in seven days, the frame's window. A row the team
 *  does not watch is counted by nothing, so it reads a dash rather than a
 *  zero, and sorts after every count. */
export const eventsColumn: TableColumn = {
  key: 'events',
  label: 'EVENTS · 7D',
  align: 'end',
  drop: 1,
  render: (r) => r.events ?? '—',
  sortValue: (r) => -((r.events as number | null) ?? -1),
  color: (r) => (r.events == null ? 'var(--color-ink-4)' : 'var(--color-ink-2)'),
}

/**
 * The line under the board. The strip names the shown key when there is one to
 * pick, so the note carries the key only when it is the sole voice. A reader
 * who can change the board is told that it saves; anyone else is only told
 * what is missing. `quiet` withholds the instruction where a flag under the
 * board already says what needs doing.
 */
export function boardNote({
  shownKey,
  sole,
  editable,
  armed,
  quiet = false,
  noun,
}: {
  shownKey: string
  sole: boolean
  editable: boolean
  armed: boolean
  quiet?: boolean
  /** What the source calls a status: Jira's statuses, Linear's states. */
  noun: 'statuses' | 'states'
}): string {
  const parts: string[] = []
  if (sole) parts.push(`showing ${shownKey}`)
  if (editable && !quiet) {
    parts.push(
      armed
        ? 'drag to remap · ★ is what TF writes back · saves as it lands'
        : `not armed yet · drag ${noun} into the columns to arm it`,
    )
  } else if (!armed) {
    parts.push(`${noun} not mapped yet`)
  }
  return parts.join(' · ')
}
