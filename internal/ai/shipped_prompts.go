package ai

import "github.com/sky-ai-eng/triage-factory/internal/domain"

// ShippedPrompts returns the system prompts every org gets seeded with —
// the single source of truth shared by the local provision action
// (db.BootstrapLocalOrg, fired by POST /api/setup/start) and the
// multi-mode org-create bootstrap (db.BootstrapNewOrg). Both run the same
// seed chain, so the two paths can't drift.
//
// Order is not significant for prompts (each upserts independently), but
// note the seed chains through blueprints: each shipped prompt is wrapped by
// a 1-step blueprint (ai.ShippedBlueprints), and the shipped event handlers
// (db.ShippedEventHandlers) carry trigger rows whose blueprint_id FKs into
// those blueprints — so any seed flow must run prompts → blueprints → handlers.
// The SystemSlug values here must stay in sync with the slugs ShippedBlueprints
// wraps and the BlueprintID references in db.ShippedEventHandlers.
//
// Prompts are team-scoped. Each entry carries a SystemSlug (the
// stable shipped identifier) rather than a literal id — the seeder mints a
// random UUID per team copy and dedupes on (org_id, team_id, system_slug).
// This stays the content source; only the seed writer changed.
func ShippedPrompts() []domain.Prompt {
	return []domain.Prompt{
		// PR review is a three-step blueprint (system-pr-review, see
		// ShippedBlueprints): a security pass and a correctness pass each
		// write findings to _tfac, then a cheap aggregator posts and
		// submits the review. The two reviewer steps inherit the team
		// default model; the aggregator is mechanical assembly, so it runs
		// on haiku (a downgrade — stepModelOrInherit never lets a per-step
		// model escalate past the team's tier).
		{SystemSlug: "system-pr-review-security", Name: "PR Review: Security", Body: PRReviewSecurityPromptTemplate, Source: "system"},
		{SystemSlug: "system-pr-review-correctness", Name: "PR Review: Correctness", Body: PRReviewCorrectnessPromptTemplate, Source: "system"},
		{SystemSlug: "system-pr-review-aggregate", Name: "PR Review: Assemble & Submit", Body: PRReviewAggregatePromptTemplate, Source: "system", Model: "haiku"},

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

		// Default Curator spec-authorship skill. The Curator
		// materializes whichever prompt a project's blueprint points at as a
		// literal Claude Code skill on each dispatch; new projects start
		// pointing at this one's blueprint. Users override per-project via the
		// Projects page.
		{SystemSlug: domain.SystemTicketSpecPromptID, Name: "Curator: Ticket as a Spec", Body: TicketSpecPromptTemplate, Source: "system"},
	}
}

// ShippedBlueprints returns the shipped blueprints. Most are a 1-step blueprint
// wrapping a single prompt: the blueprint reuses the wrapped prompt's
// system_slug (distinct table, no collision) and its single step points at that
// prompt's slug. Each such prompt is either fired by a trigger
// (CI-fix, conflict-resolution, jira-implement, fix-review-feedback) or used by
// the curator (system-ticket-spec), so wrapping it keeps both the
// trigger→blueprint and project→blueprint references resolvable.
//
// The exception is PR review (system-pr-review), the one multi-step shipped
// blueprint: security → correctness → aggregate. Its step prompts
// are NOT auto-wrapped below — a prompt can be a step in at most one blueprint
// (unique index on blueprint_steps.step_prompt_id), and these three belong to
// this chain. The blueprint keeps the slug `system-pr-review` so the
// system-trigger-pr-review handler's blueprint_id still resolves.
//
// The seeder seeds prompts first, then these blueprints + steps, then wires
// triggers to the blueprint slugs.
func ShippedBlueprints() []domain.SeedBlueprint {
	prReviewSteps := []string{
		"system-pr-review-security",
		"system-pr-review-correctness",
		"system-pr-review-aggregate",
	}
	isPRReviewStep := make(map[string]bool, len(prReviewSteps))
	for _, s := range prReviewSteps {
		isPRReviewStep[s] = true
	}

	out := []domain.SeedBlueprint{
		{SystemSlug: "system-pr-review", Name: "PR Code Review", StepPromptSlugs: prReviewSteps},
	}
	for _, p := range ShippedPrompts() {
		if isPRReviewStep[p.SystemSlug] {
			continue // a step of the multi-step PR-review blueprint, not its own wrapper
		}
		out = append(out, domain.SeedBlueprint{
			SystemSlug:      p.SystemSlug,
			Name:            p.Name,
			StepPromptSlugs: []string{p.SystemSlug},
		})
	}
	return out
}
