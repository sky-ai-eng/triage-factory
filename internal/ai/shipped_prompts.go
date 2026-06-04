package ai

import "github.com/sky-ai-eng/triage-factory/internal/domain"

// ShippedPrompts returns the system prompts every org gets seeded with —
// the single source of truth shared by the local provision action
// (db.BootstrapLocalOrg, fired by POST /api/setup/start) and the
// multi-mode org-create bootstrap (db.BootstrapNewOrg). Both run the same
// org-template seed chain, so the two paths can't drift.
//
// Order is not significant for prompts (each upserts independently), but
// note the seed chains through blueprints: each shipped prompt is wrapped by
// a 1-step blueprint (ai.ShippedBlueprints), and the shipped event handlers
// (db.ShippedEventHandlers) carry trigger rows whose blueprint_id FKs into
// those blueprints — so any seed flow must run prompts → blueprints → handlers.
// The SystemSlug values here must stay in sync with the slugs ShippedBlueprints
// wraps and the BlueprintID references in db.ShippedEventHandlers.
//
// SKY-380: prompts are team-scoped. Each entry carries a SystemSlug (the
// stable shipped identifier) rather than a literal id — the seeder mints a
// random UUID per team copy and dedupes on (org_id, team_id, system_slug).
// This stays the content source; only the seed writer changed.
func ShippedPrompts() []domain.Prompt {
	return []domain.Prompt{
		// Default PR review prompt — manual only. The user picks when
		// to review a PR; no automation makes sense for reviewing
		// (including reviewing one's own draft — that's just running
		// this prompt by hand).
		{SystemSlug: "system-pr-review", Name: "PR Code Review", Body: PRReviewPromptTemplate, Source: "system"},

		// Merge conflict resolution prompt — auto-fired on merge
		// conflicts on the user's own PRs via the matching trigger.
		{SystemSlug: "system-conflict-resolution", Name: "Merge Conflict Resolution", Body: ConflictResolutionPromptTemplate, Source: "system"},

		// CI fix prompt — auto-fired on CI failures via prompt_trigger.
		{SystemSlug: "system-ci-fix", Name: "CI Fix", Body: CIFixPromptTemplate, Source: "system"},

		// Jira implementation prompt — auto-fired on issues assigned
		// to the user via the matching trigger.
		{SystemSlug: "system-jira-implement", Name: "Jira Issue Implementation", Body: JiraImplementPromptTemplate, Source: "system"},

		// Fix review feedback — fires on reviews landed on the user's
		// PRs. Same action regardless of whether the reviewer is the
		// user (self-review loop) or someone else (normal code review):
		// read the review, fix what's right, push back on what isn't,
		// push to branch.
		{SystemSlug: "system-fix-review-feedback", Name: "Fix Review Feedback", Body: FixReviewFeedbackPromptTemplate, Source: "system"},

		// Default Curator spec-authorship skill (SKY-221). The Curator
		// materializes whichever prompt a project's blueprint points at as a
		// literal Claude Code skill on each dispatch; new projects start
		// pointing at this one's blueprint. Users override per-project via the
		// Projects page.
		{SystemSlug: domain.SystemTicketSpecPromptID, Name: "Curator: Ticket as a Spec", Body: TicketSpecPromptTemplate, Source: "system"},
	}
}

// ShippedBlueprints returns one 1-step blueprint per shipped prompt: a
// blueprint reuses the wrapped prompt's system_slug (distinct table, no
// collision) and its single step points at that prompt's slug. Every shipped
// prompt is either fired by a trigger (CI-fix, conflict-resolution, PR-review,
// jira-implement, fix-review-feedback) or used by the curator
// (system-ticket-spec), so wrapping each one keeps both the trigger→blueprint
// and project→blueprint references resolvable. The seeder seeds prompts first,
// then these blueprints + steps, then wires triggers to the blueprint slugs.
func ShippedBlueprints() []domain.SeedBlueprint {
	prompts := ShippedPrompts()
	out := make([]domain.SeedBlueprint, 0, len(prompts))
	for _, p := range prompts {
		out = append(out, domain.SeedBlueprint{
			SystemSlug:      p.SystemSlug,
			Name:            p.Name,
			StepPromptSlugs: []string{p.SystemSlug},
		})
	}
	return out
}
