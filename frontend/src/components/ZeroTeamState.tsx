import { Link } from 'react-router'
import { Plus, Users } from 'lucide-react'
import { useOrgHref } from '../hooks/useOrgHref'

// ZeroTeamState is the friendly empty state shown on every team-scoped surface
// (the /team page, the Prompts canvas, the team-filtered Board) when the
// signed-in user belongs to NO team in the active org. This is a first-class,
// non-error state — it already happens today via team-less org invites (the
// default invite adds a user to the org with no team) and the archive slice
// produces it too — so it must never read as a crash or a broken dropdown.
//
// Org admins get a "Create a team" CTA pointing at the org page's Settings tab,
// where the existing TeamManagementSection lives (creating a team auto-enrolls
// the creator as its admin). Everyone else is told to ask an org admin.
export default function ZeroTeamState({ canCreate }: { canCreate: boolean }) {
  const orgHref = useOrgHref()
  return (
    <div className="mx-auto flex max-w-md flex-col items-center px-6 py-20 text-center">
      <span className="mb-4 flex h-12 w-12 items-center justify-center rounded-2xl bg-warm-2 text-warm">
        <Users size={22} />
      </span>
      <h2 className="text-section font-semibold text-ink-1">
        You&rsquo;re not on a team yet
      </h2>
      <p className="mt-2 text-body leading-relaxed text-ink-3">
        {canCreate
          ? 'Create a team to start tracking work, or ask another org admin to add you to an existing one.'
          : 'Ask an org admin to add you to a team. Once you’re on one, your team’s board, prompts, and settings show up here.'}
      </p>
      {canCreate && (
        <Link
          to={`${orgHref('/org')}?tab=settings`}
          className="mt-5 inline-flex items-center gap-1.5 rounded-full bg-warm px-4 py-2 text-body font-semibold text-warm-ink transition-colors hover:bg-warm/90"
        >
          <Plus size={14} />
          Create a team
        </Link>
      )}
    </div>
  )
}
