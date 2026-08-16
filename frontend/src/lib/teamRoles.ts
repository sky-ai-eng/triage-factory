// The team-membership role vocabulary's display labels, shared by every surface
// that offers a role: the roster's per-row <select> (via the team adapter's
// roleLabels) and the add-member picker. One map because the two surfaces render
// together — the picker is the roster panel's add affordance — so a second copy
// is a second thing to keep true.
//
// Keys are the raw membership_role values the backend accepts (see teamRoles in
// internal/server/team_members_handler.go); the value sent over the wire is
// always the key, never the label. A role with no entry renders as its raw
// value, which is why only 'viewer' appears here: admin and member need no
// gloss, while viewer is read-only and the label spells that boundary out so an
// admin picking it knows what it grants.
export const TEAM_ROLE_LABELS: Record<string, string> = { viewer: 'viewer (read-only)' }
