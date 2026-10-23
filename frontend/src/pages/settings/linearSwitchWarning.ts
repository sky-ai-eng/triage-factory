import { useState } from 'react'
import { fetchLinearTeamVisibility, type LinearTrackedTeam } from './linearConnect'

// What replaces the org's current Linear credential: the installed app, or
// another personal API key.
export type LinearSwitchTarget = 'app' | 'key'

/** A switch waiting on the person's answer, as LinearSwitchDialog renders it. */
export interface LinearSwitchPending {
  to: LinearSwitchTarget
  /** The tracked teams the current credential reads that are private. */
  teams: LinearTrackedTeam[]
  settle: (proceed: boolean) => void
}

/**
 * useLinearSwitchWarning stands in front of a switch of the org's Linear
 * credential. `confirmSwitch` reads the tracked teams the current credential
 * sees as private and, when there are any, puts the question on `pending`
 * for LinearSwitchDialog: an installed app reads a private team only once the
 * team is shared with it, and a personal key only when its owner is a member.
 * It resolves true to go ahead. With no private team, or no reading, it
 * resolves true without asking; the connection card names any team the new
 * credential cannot read once it is bound.
 */
export function useLinearSwitchWarning() {
  const [pending, setPending] = useState<LinearSwitchPending | null>(null)

  const confirmSwitch = async (orgId: string, to: LinearSwitchTarget): Promise<boolean> => {
    const visibility = await fetchLinearTeamVisibility(orgId)
    if (!visibility || visibility.private.length === 0) return true
    return new Promise<boolean>((resolve) =>
      setPending({
        to,
        teams: visibility.private,
        settle: (proceed) => {
          setPending(null)
          resolve(proceed)
        },
      }),
    )
  }

  return { confirmSwitch, pending }
}
