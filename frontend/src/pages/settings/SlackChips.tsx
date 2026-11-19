// The chips the Slack card and its switch preview label a connection with.

import type { SlackConnection, SlackConnectionStatus } from './slackConnect'

export function SourceChip({ source }: { source: SlackConnection['source'] }) {
  return <Chip>{source === 'deployment' ? 'Deployment app' : 'Your app'}</Chip>
}

export function TransportChip({ transport }: { transport: 'socket' | 'events_api' }) {
  return <Chip>{transport === 'socket' ? 'Socket Mode' : 'Events API'}</Chip>
}

function Chip({ children }: { children: React.ReactNode }) {
  return (
    <span className="inline-flex shrink-0 items-center rounded-full bg-tint-3 px-2 py-0.5 text-reported font-medium text-ink-3">
      {children}
    </span>
  )
}

// CONNECTION_STATUS_DISPLAY maps the backend's connection.state to the chip's
// label and dot color. "draining" (the brief moment between a disconnect frame
// and redialing) folds into "Reconnecting…", since it resolves in
// milliseconds.
const CONNECTION_STATUS_DISPLAY: Record<
  SlackConnectionStatus['state'],
  { label: string; dot: string; text: string }
> = {
  open: { label: 'Connected', dot: 'bg-ink-1', text: 'text-ink-1' },
  dialing: { label: 'Connecting…', dot: 'bg-warm', text: 'text-warm' },
  draining: { label: 'Reconnecting…', dot: 'bg-warm', text: 'text-warm' },
  backing_off: { label: 'Reconnecting…', dot: 'bg-warm', text: 'text-warm' },
  auth_failed: { label: 'Auth failed', dot: 'bg-alarm', text: 'text-alarm' },
}

// ConnectionStatusChip is the Socket Mode connection status: a colored dot and
// a short label, with the last error, when there is one, on hover. No
// connection object means one of two things: an app that never gets a socket
// (the muted "n/a" chip), or an org's own Socket Mode app the reconciler hasn't
// started yet (a brief window right after connect, rendered as "Connecting…").
export function ConnectionStatusChip({ conn }: { conn: SlackConnection }) {
  const status = conn.connection
  if (!status) {
    if (conn.transport === 'socket') {
      return <StatusDot label="Connecting…" dot="bg-warm" text="text-warm" />
    }
    return <StatusDot label="n/a" dot="bg-ink-3/40" text="text-ink-3" />
  }
  const display = CONNECTION_STATUS_DISPLAY[status.state] ?? CONNECTION_STATUS_DISPLAY.dialing
  return (
    <StatusDot
      label={display.label}
      dot={display.dot}
      text={display.text}
      title={status.last_error || undefined}
    />
  )
}

// StatusDot carries its own `title` rather than being wrapped in an outer span:
// a plain inline wrapper would sit on the text baseline and nudge the pill a
// hair below its sibling chips, so keeping every chip a single inline-flex
// flex item is what lets the row's items-center line them up.
export function StatusDot({
  label,
  dot,
  text,
  title,
}: {
  label: string
  dot: string
  text: string
  title?: string
}) {
  return (
    <span
      title={title}
      className={`inline-flex shrink-0 items-center gap-1 rounded-full bg-tint-3 px-2 py-0.5 text-reported font-medium ${text}`}
    >
      <span className={`h-1.5 w-1.5 rounded-full ${dot}`} />
      {label}
    </span>
  )
}
