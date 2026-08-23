// Whether GitHub is actually delivering this App's webhooks here.
//
// A registered, active App can receive nothing at all — its webhook never
// configured, pointed at another deployment, or delivering into a receiver that
// refuses every one — and until this line existed, all three looked exactly like
// an App whose events simply hadn't fired yet. What breaks is the installation
// mirror: polling stops for the workspace, the repo picker empties, credential
// resolution misfires. So the panel says which of those it is, in words that
// name the consequence rather than the mechanism.
//
// Two things this deliberately does NOT do. It never renders "not configured"
// as a problem — that is the correct state for a local install, where
// installations are discovered by periodic sync. And it renders nothing at all
// when the backend has no probe answer, because an absent answer is "not known",
// never "fine".
import { useState } from 'react'
import { AlertTriangle, CheckCircle2, Info, RefreshCw } from 'lucide-react'
import { toast } from '../../components/Toast/toastStore'
import { replayGitHubWebhookDeliveries, type GitHubAppWebhookHealth } from '../../lib/githubApp'
import { timeAgo } from '../../lib/relativeTime'

// rejectionCause names the likely reason the receiver refused a delivery, from
// what GitHub recorded. The status code is the evidence; the secret's value is
// never compared anywhere, here or on the server.
function rejectionCause(health: GitHubAppWebhookHealth): string {
  if (!health.secret_configured) {
    return 'This App has no webhook secret set on GitHub, so Triage Factory can’t verify the deliveries are genuine.'
  }
  switch (health.last_delivery_status_code) {
    case 401:
      return 'The App’s webhook secret is missing here, or doesn’t match the one set on GitHub.'
    case 404:
      return 'This deployment doesn’t serve the URL the App is delivering to.'
    case 0:
      return 'GitHub couldn’t reach this deployment at all.'
    default:
      return `Triage Factory answered ${health.last_delivery_status_code}.`
  }
}

// notice maps a probe answer to what the panel says, plus how loudly. `warn`
// is reserved for the two states where something a person configured is wrong;
// everything else is information.
function notice(
  health: GitHubAppWebhookHealth,
  isLocal: boolean,
): { warn: boolean; headline: string; detail: string } | null {
  switch (health.state) {
    case 'not_configured':
      return {
        warn: false,
        headline: 'Webhooks aren’t configured for this App.',
        detail: isLocal
          ? 'Normal for a local install — GitHub can’t reach this machine, so installations are discovered by periodic sync instead.'
          : 'Installations are discovered by periodic sync instead, so changes can take a poll cycle to appear.',
      }
    case 'pointing_elsewhere':
      return {
        warn: true,
        headline: `This App’s webhook points at ${health.hook_host || 'another host'}, not this deployment.`,
        detail:
          'Installation changes are being delivered there. Point the App’s webhook URL at this deployment, or expect installs to appear only on the next sync.',
      }
    case 'delivering_rejected':
      return {
        warn: true,
        headline: 'GitHub is delivering webhooks and Triage Factory is rejecting them.',
        detail: rejectionCause(health),
      }
    case 'no_deliveries':
      return {
        warn: false,
        headline: 'Webhooks point at this deployment.',
        detail:
          'GitHub has recorded no delivery in the last 30 days — normal for a new or quiet App.',
      }
    case 'healthy':
      return {
        warn: false,
        headline: 'Webhooks are arriving.',
        detail: health.last_delivery_at
          ? `Last delivery ${timeAgo(health.last_delivery_at)}.`
          : 'The most recent delivery was accepted.',
      }
    case 'unavailable':
      return {
        warn: false,
        headline: 'Webhook delivery history isn’t available on this GitHub host.',
        detail:
          'GitHub Enterprise Server below 3.2 doesn’t expose it, so delivery health can’t be checked from here.',
      }
    default:
      // A state this build doesn't know is not evidence of anything. Say
      // nothing rather than inventing a verdict for it.
      return null
  }
}

export default function GitHubWebhookHealthNotice({
  orgId,
  health,
  isLocal,
}: {
  orgId: string
  // null when nothing has been probed yet — the component renders nothing.
  health: GitHubAppWebhookHealth | null
  isLocal: boolean
}) {
  const [busy, setBusy] = useState(false)

  if (!health) return null
  const n = notice(health, isLocal)
  if (!n) return null

  // The repair is offered only where it repairs something: GitHub is
  // delivering, the receiver refused, and those refused installation events are
  // exactly what left the mirror stale.
  const canReplay = health.state === 'delivering_rejected'

  const replay = async () => {
    if (busy) return
    setBusy(true)
    try {
      const res = await replayGitHubWebhookDeliveries(orgId)
      // The request is finished by the time this runs, so the copy is about
      // what GitHub accepted — not about work still happening here. GitHub
      // performs the redelivery itself, which is why it's "queued" rather than
      // "replayed": the events arrive at the receiver on GitHub's schedule.
      if (res.candidates === 0) {
        toast.success('No missed installation events to replay')
      } else if (res.replayed === 0) {
        toast.error(`GitHub refused all ${res.candidates} redelivery requests`)
      } else {
        const queued = `Queued ${res.replayed} missed installation event${res.replayed === 1 ? '' : 's'} for redelivery`
        toast.success(res.failed > 0 ? `${queued} — ${res.failed} refused by GitHub` : queued)
      }
    } catch (e) {
      toast.error((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  const Icon = n.warn ? AlertTriangle : health.state === 'healthy' ? CheckCircle2 : Info

  return (
    <div
      className={`flex items-start gap-2.5 rounded-xl border px-3.5 py-3 ${
        n.warn ? 'border-alarm/25 bg-alarm/[0.06]' : 'border-line-1 bg-white/[0.02]'
      }`}
    >
      <Icon size={14} className={`mt-0.5 shrink-0 ${n.warn ? 'text-alarm' : 'text-ink-3'}`} />
      <div className="space-y-1">
        <p className="text-[13px] font-medium text-ink-2">{n.headline}</p>
        <p className="text-[12px] leading-relaxed text-ink-3">{n.detail}</p>
        {canReplay && (
          <button
            type="button"
            onClick={() => void replay()}
            disabled={busy}
            className="mt-1 inline-flex items-center gap-1.5 rounded-xl border border-line-1 px-3 py-1.5 text-[12px] font-medium text-ink-2 transition-colors hover:border-warm/40 hover:text-ink-1 disabled:opacity-50"
          >
            <RefreshCw size={12} className={busy ? 'animate-spin' : ''} />
            {busy ? 'Replaying…' : 'Replay missed installation events'}
          </button>
        )}
      </div>
    </div>
  )
}
