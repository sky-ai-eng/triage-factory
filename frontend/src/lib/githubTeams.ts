// Shared types + key helper for the GitHub-team → TF-team mapping surfaces
// (the onboarding wizard and the Settings editor, which render the same
// GitHubTeamChecklist body). These live here rather than in the component
// file so the component exports only a component — react-refresh's
// only-export-components rule flags a value export sitting beside one.

// One live GitHub team candidate the admin can assign to a Triage Factory
// team — the candidate shape from GET .../github-groups. `member_count`
// drives the "broad team — noisy queue" hint (best-effort; absent/0 =
// unknown); `mine` is true when the acting admin personally belongs to the
// team and is only populated when the caller passes ?include_membership=true
// (the onboarding wizard, to pre-check the admin's own teams). The Settings
// editor omits the flag, so the "your team" badge simply never shows there.
export interface GitHubTeamCandidate {
  org_login: string
  team_slug: string
  name: string
  member_count?: number
  mine?: boolean
}

// Canonical identity for one mapping row, case-folded so "Org/Team" and
// "org/team" collapse to the same Set key on both surfaces.
export const keyOf = (org: string, slug: string) => `${org.toLowerCase()}/${slug.toLowerCase()}`
