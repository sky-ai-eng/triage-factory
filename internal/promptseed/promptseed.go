// Package promptseed holds the content Triage Factory seeds every team with:
// mission prompts and the blueprints that wrap them.
//
// Seed content, not runtime text — the distinction the package name carries.
// Each .txt here is copied into the team's `prompts` table on provision and
// kept in step by the boot-time drift sync (db.SyncShippedDefaultsForAllTeams);
// from then on the row is the source of truth and the user may rewrite any of
// it in the UI. Nothing here is read at agent-spawn time — a run resolves its
// mission from the row, never from this package.
//
// That is what separates it from internal/agentprompt, which owns the
// agent-framework text no user edits, and from the system-job prompts that
// live next to their consumers (internal/ai, internal/repoprofile).
package promptseed

import (
	_ "embed"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// PR review is a three-step blueprint: a security pass and a correctness pass
// each write findings to _tfac, then a cheap aggregator posts and submits the
// review. See Blueprints.
//
//go:embed prompts/pr-review-security.txt
var prReviewSecurity string

//go:embed prompts/pr-review-correctness.txt
var prReviewCorrectness string

//go:embed prompts/pr-review-aggregate.txt
var prReviewAggregate string

//go:embed prompts/jira-implement.txt
var jiraImplement string

//go:embed prompts/conflict-resolution.txt
var conflictResolution string

//go:embed prompts/ci-fix.txt
var ciFix string

//go:embed prompts/fix-review-feedback.txt
var fixReviewFeedback string

// Prompts returns the system prompts every team gets seeded with — the single
// source of truth shared by the local provision action (db.BootstrapLocalOrg,
// fired by POST /api/orgs), the multi-mode org-create bootstrap
// (db.BootstrapNewOrg), and the boot-time drift sync. They all run the same
// seed chain, so the paths can't diverge.
//
// Order is not significant for prompts (each upserts independently), but note
// the seed chains through blueprints: each shipped prompt is wrapped by a
// 1-step blueprint (Blueprints), and the shipped event handlers
// (db.ShippedEventHandlers) carry trigger rows whose blueprint_id FKs into
// those blueprints — so any seed flow must run prompts → blueprints →
// handlers. The SystemSlug values here must stay in sync with the slugs
// Blueprints wraps and the BlueprintID references in db.ShippedEventHandlers.
//
// Prompts are team-scoped. Each entry carries a SystemSlug (the stable shipped
// identifier) rather than a literal id — the seeder mints a random UUID per
// team copy and dedupes on (org_id, team_id, system_slug).
//
// No seed names a model. A seed goes to every org whatever its providers, so a
// concrete id here would be a claim about a deployment TF cannot see, and the
// tier words that used to stand in for one no longer exist. Every seeded prompt
// therefore ships Model "" — inherit the team default — which the team owns and
// can override per prompt in the editor. The accepted cost is that a
// deliberately cheap step (the PR-review aggregator) runs at the team default
// until someone pins it.
func Prompts() []domain.Prompt {
	return []domain.Prompt{
		// PR review is a three-step blueprint (system-pr-review, see
		// Blueprints): a security pass and a correctness pass each write
		// findings to _tfac, then a cheap aggregator posts and submits the
		// review. Every step inherits the team default, including the
		// aggregator — see the no-seed-names-a-model note above.
		{SystemSlug: "system-pr-review-security", Name: "PR Review: Security", Body: prReviewSecurity, Source: "system"},
		{SystemSlug: "system-pr-review-correctness", Name: "PR Review: Correctness", Body: prReviewCorrectness, Source: "system"},
		{SystemSlug: "system-pr-review-aggregate", Name: "PR Review: Assemble & Submit", Body: prReviewAggregate, Source: "system"},

		// Merge conflict resolution prompt — auto-fired on merge
		// conflicts on the user's own PRs via the matching trigger.
		{SystemSlug: "system-conflict-resolution", Name: "Merge Conflict Resolution", Body: conflictResolution, Source: "system"},

		// CI fix prompt — auto-fired on CI failures via prompt_trigger.
		{SystemSlug: "system-ci-fix", Name: "CI Fix", Body: ciFix, Source: "system"},

		// Jira implementation prompt — auto-fired on issues assigned
		// to the user via the matching trigger.
		{SystemSlug: "system-jira-implement", Name: "Jira Issue Implementation", Body: jiraImplement, Source: "system"},

		// Fix review feedback — fires on reviews landed on the user's
		// PRs. Same action regardless of whether the reviewer is the
		// user (self-review loop) or someone else (normal code review):
		// read the review, fix what's right, push back on what isn't,
		// push to branch.
		{SystemSlug: "system-fix-review-feedback", Name: "Fix Review Feedback", Body: fixReviewFeedback, Source: "system"},
	}
}

// Blueprints returns the shipped blueprints. Most are a 1-step blueprint
// wrapping a single prompt: the blueprint reuses the wrapped prompt's
// system_slug (distinct table, no collision) and its single step points at that
// prompt's slug. Each such prompt is fired by a trigger (CI-fix,
// conflict-resolution, jira-implement, fix-review-feedback), so wrapping it
// keeps the trigger→blueprint reference resolvable.
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
func Blueprints() []domain.SeedBlueprint {
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
	for _, p := range Prompts() {
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
