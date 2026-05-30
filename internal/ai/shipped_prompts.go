package ai

import "github.com/sky-ai-eng/triage-factory/internal/domain"

// ShippedPrompts returns the system prompts every org gets seeded with —
// the single source of truth shared by the local-mode boot seeder
// (seedDefaultPrompts in main) and the multi-mode org-create bootstrap
// (db.BootstrapNewOrg). Previously this list was inlined in main's
// seed.go; centralizing it here means the two seed paths can't drift.
//
// Order is not significant for prompts (each upserts independently), but
// note the shipped event handlers (db.ShippedEventHandlers) carry
// trigger rows whose prompt_id FKs into these — so any seed flow must
// seed prompts before handlers. The IDs here must stay in sync with the
// PromptID references in db.ShippedEventHandlers.
func ShippedPrompts() []domain.Prompt {
	return []domain.Prompt{
		// Default PR review prompt — manual only. The user picks when
		// to review a PR; no automation makes sense for reviewing
		// (including reviewing one's own draft — that's just running
		// this prompt by hand).
		{ID: "system-pr-review", Name: "PR Code Review", Body: PRReviewPromptTemplate, Source: "system"},

		// Merge conflict resolution prompt — auto-fired on merge
		// conflicts on the user's own PRs via the matching trigger.
		{ID: "system-conflict-resolution", Name: "Merge Conflict Resolution", Body: ConflictResolutionPromptTemplate, Source: "system"},

		// CI fix prompt — auto-fired on CI failures via prompt_trigger.
		{ID: "system-ci-fix", Name: "CI Fix", Body: CIFixPromptTemplate, Source: "system"},

		// Jira implementation prompt — auto-fired on issues assigned
		// to the user via the matching trigger.
		{ID: "system-jira-implement", Name: "Jira Issue Implementation", Body: JiraImplementPromptTemplate, Source: "system"},

		// Fix review feedback — fires on reviews landed on the user's
		// PRs. Same action regardless of whether the reviewer is the
		// user (self-review loop) or someone else (normal code review):
		// read the review, fix what's right, push back on what isn't,
		// push to branch.
		{ID: "system-fix-review-feedback", Name: "Fix Review Feedback", Body: FixReviewFeedbackPromptTemplate, Source: "system"},

		// Default Curator spec-authorship skill (SKY-221). The Curator
		// materializes whichever prompt a project points at as a
		// literal Claude Code skill on each dispatch; new projects start
		// pointing at this one. Users override per-project via the
		// Projects page.
		{ID: domain.SystemTicketSpecPromptID, Name: "Curator: Ticket as a Spec", Body: TicketSpecPromptTemplate, Source: "system"},
	}
}
