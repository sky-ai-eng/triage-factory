import { useEffect, useState } from 'react'
import { apiJSON } from '../lib/apiClient'

// The team spend slice this surface renders: the headline total and the
// per-member costs the roster column reads. GET /api/teams/{id}/usage is
// admin-gated (team admin or org admin), so the hook takes `enabled` and a
// member never fires a read the server would refuse.

export type TeamUsage = {
  total_cost_usd: number
  by_user: Array<{ user_id: string; display_name: string; cost: number }>
  /** Model-attributed spend only — system overhead with no model lands in the
   * total but never here, so shares are computed over this list's own sum. */
  by_model: Array<{ model: string; cost: number }>
}

function sinceParam(days: number): string {
  return new Date(Date.now() - days * 86400_000).toISOString().slice(0, 10)
}

export function useTeamUsage(teamId: string, days: number, enabled: boolean): TeamUsage | null {
  // Stored with the ask it answers — a team or window switch reads null
  // until the new answer lands, with no synchronous reset in the effect.
  const key = `${teamId}:${days}`
  const [got, setGot] = useState<{ key: string; data: TeamUsage } | null>(null)
  useEffect(() => {
    if (!teamId || !enabled) return
    let live = true
    void apiJSON<TeamUsage>(
      `/api/teams/${encodeURIComponent(teamId)}/usage?since=${sinceParam(days)}`,
    )
      .then((u) => {
        if (live) setGot({ key: `${teamId}:${days}`, data: u })
      })
      .catch(() => {
        // The spend figure holds its dash.
      })
    return () => {
      live = false
    }
  }, [teamId, days, enabled])
  return got && got.key === key ? got.data : null
}

/** Settled spend as real money; sub-cent-but-nonzero reads "<$0.01" rather
 * than a misleading "$0.00". Mirrors the Usage page's formatter. */
export function fmtSpendUSD(n: number): string {
  if (!n) return '$0.00'
  if (n > 0 && n < 0.01) return '<$0.01'
  return n.toLocaleString('en-US', { style: 'currency', currency: 'USD' })
}
