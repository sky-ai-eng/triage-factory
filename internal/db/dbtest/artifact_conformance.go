package dbtest

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ArtifactFixture is one fresh, empty org for RunArtifactConformance: the
// store bundle, the org every call is for, a team the ledger rows hang off
// and a second team in the same org, a way to mint a conversation in the
// first team, a way to set a ledger row's occurred_at to a given instant in
// the shape the column's default writes, and the same for a deliverable's
// created_at.
type ArtifactFixture struct {
	Stores                  db.Stores
	OrgID, TeamID           string
	OtherTeamID             string
	NewConversation         func(t *testing.T) string
	SetOccurredAt           func(t *testing.T, actionID string, at time.Time)
	SetDeliverableCreatedAt func(t *testing.T, deliverableID string, at time.Time)
}

// ArtifactFactory builds a fresh fixture per subtest.
type ArtifactFactory func(t *testing.T) ArtifactFixture

// RunArtifactConformance covers db.ArtifactStore: a conversation's agent rows
// that name an object, grouped per object, with the latest row's fields, the
// row count and the first and last times; a second conversation's artifact
// for the same object; the rows that never form one (a person's, TF's own, a
// refusal, a catch-all, a kind with no identifying column); the
// entity-backed display rule across a rename; the per-kind counts and the
// existence check; the read grouped across conversations; the action list's
// object and refusal filters, which read the same identity; paging totals
// that count groups; the team and org reads, grouped by object across runs
// with their run counts and teams, windowed and filtered by row; and the
// deliverable reads that answer an artifact's match.
func RunArtifactConformance(t *testing.T, mk ArtifactFactory) {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	at := func(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute) }

	type seed struct {
		f    ArtifactFixture
		repo domain.Repository
		pr   *domain.Entity
	}
	setup := func(t *testing.T) seed {
		t.Helper()
		f := mk(t)
		repo, err := f.Stores.Repos.UpsertSystem(ctx, f.OrgID, domain.Repository{
			Host: TestGitHubHost, Owner: "octo", Repo: "api", ExternalID: "31",
		})
		if err != nil {
			t.Fatalf("seed repository: %v", err)
		}
		pr, _, err := f.Stores.Entities.FindOrCreateSystem(ctx, f.OrgID, "github", TestGitHubHost,
			"octo/api#7", "PR_art7", domain.EntityKindPullRequest, "A PR", TestGitHubHost+"/octo/api/pull/7")
		if err != nil {
			t.Fatalf("seed pull request entity: %v", err)
		}
		return seed{f: f, repo: repo, pr: pr}
	}
	// record writes a through the app pool at the given instant, filling the
	// row's identity; an empty PerformedBy is the agent.
	record := func(t *testing.T, s seed, conversationID string, a domain.ExternalAction, when time.Time) domain.ExternalAction {
		t.Helper()
		if a.ID == "" {
			a.ID = uuid.NewString()
		}
		if a.TeamID == "" {
			a.TeamID = s.f.TeamID
		}
		a.ConversationID = conversationID
		if a.Credential == "" {
			a.Credential = domain.CredentialGitHubApp
		}
		if a.PerformedBy == "" {
			a.PerformedBy = domain.PerformedByAgent
		}
		if err := s.f.Stores.ExternalActions.Record(ctx, s.f.OrgID, a); err != nil {
			t.Fatalf("record %s: %v", a.Action, err)
		}
		s.f.SetOccurredAt(t, a.ID, when)
		return a
	}
	branch := func(s seed, action, target, url string) domain.ExternalAction {
		return domain.ExternalAction{
			Provider: domain.ProviderGitHub, Scope: TestGitHubHost, Action: action,
			Target: target, URL: url, ExternalID: "refs/heads/fix", RepositoryID: s.repo.ID,
			ObjectKind: domain.ObjectKindBranch, ObjectKey: "refs/heads/fix",
		}
	}
	pullRequest := func(s seed, action string) domain.ExternalAction {
		return domain.ExternalAction{
			Provider: domain.ProviderGitHub, Scope: TestGitHubHost, Action: action,
			Target: "octo/api#7", URL: TestGitHubHost + "/octo/api/pull/7", EntityID: s.pr.ID,
			RepositoryID: s.repo.ID, ObjectKind: domain.ObjectKindPullRequest,
		}
	}
	comment := func(id int64) domain.ExternalAction {
		return domain.ExternalAction{
			Provider: domain.ProviderGitHub, Scope: TestGitHubHost, Action: domain.ActionCommentPosted,
			Target: "octo/api#7", ObjectKind: domain.ObjectKindComment,
			ObjectKey: domain.GitHubCommentKey(domain.GitHubIssueComment, id),
		}
	}
	objectID := func(t *testing.T, a domain.ExternalAction) string {
		t.Helper()
		id, ok := domain.ObjectID(a)
		if !ok {
			t.Fatalf("row %s names no object", a.Action)
		}
		return id
	}
	objectRef := func(t *testing.T, a domain.ExternalAction) domain.ObjectRef {
		t.Helper()
		return domain.ObjectRef(objectID(t, a))
	}
	list := func(t *testing.T, s seed, conversationID string, opts db.ListOpts) ([]domain.Artifact, int) {
		t.Helper()
		got, total, err := s.f.Stores.Artifacts.ListByConversation(ctx, s.f.OrgID, conversationID, opts)
		if err != nil {
			t.Fatalf("ListByConversation: %v", err)
		}
		system, systemTotal, err := s.f.Stores.Artifacts.ListByConversationSystem(ctx, s.f.OrgID, conversationID, opts)
		if err != nil {
			t.Fatalf("ListByConversationSystem: %v", err)
		}
		if len(system) != len(got) || systemTotal != total {
			t.Fatalf("ListByConversationSystem = %d items (total %d), ListByConversation = %d (total %d)",
				len(system), systemTotal, len(got), total)
		}
		for i := range got {
			if !sameArtifact(system[i], got[i]) {
				t.Errorf("item %d: ListByConversationSystem = %+v, ListByConversation = %+v", i, system[i], got[i])
			}
		}
		return got, total
	}

	t.Run("Groups_a_conversations_rows_per_object_with_the_latest_rows_fields", func(t *testing.T) {
		s := setup(t)
		conv := s.f.NewConversation(t)
		first := record(t, s, conv, branch(s, domain.ActionBranchPushed, "octo/old", TestGitHubHost+"/octo/old/tree/fix"), at(1))
		latest := record(t, s, conv, branch(s, domain.ActionBranchPushed, "octo/api", TestGitHubHost+"/octo/api/tree/fix"), at(5))
		// Two rows of one comment at the same instant: the greater id is the
		// latest row.
		lo, hi := uuid.NewString(), uuid.NewString()
		if hi < lo {
			lo, hi = hi, lo
		}
		edited := comment(901)
		edited.ID, edited.Action = hi, domain.ActionCommentEdited
		posted := comment(901)
		posted.ID = lo
		record(t, s, conv, posted, at(3))
		record(t, s, conv, edited, at(3))

		got, total := list(t, s, conv, db.Unwindowed)
		if len(got) != 2 || total != 2 {
			t.Fatalf("read %d artifacts (total %d), want 2: %+v", len(got), total, got)
		}
		// LastAt descending: the branch (5) before the comment (3).
		b, c := got[0], got[1]
		if b.ObjectID != objectRef(t, first) || b.ObjectID != objectRef(t, latest) {
			t.Errorf("branch object id = %q, want %q", b.ObjectID, objectID(t, latest))
		}
		if b.ObjectKind != domain.ObjectKindBranch || b.Provider != domain.ProviderGitHub || b.Scope != TestGitHubHost ||
			b.RepositoryID != s.repo.ID || b.ObjectKey != "refs/heads/fix" || b.EntityID != "" {
			t.Errorf("branch identity = %+v", b)
		}
		if b.ActionCount != 2 || b.LastAction != domain.ActionBranchPushed {
			t.Errorf("branch count/action = %d/%q, want 2/%q", b.ActionCount, b.LastAction, domain.ActionBranchPushed)
		}
		if b.Target != "octo/api" || b.URL != TestGitHubHost+"/octo/api/tree/fix" {
			t.Errorf("branch display = (%q, %q), want the latest row's", b.Target, b.URL)
		}
		if !b.FirstAt.Equal(at(1)) || !b.LastAt.Equal(at(5)) {
			t.Errorf("branch first/last = %v/%v, want %v/%v", b.FirstAt, b.LastAt, at(1), at(5))
		}
		if c.ObjectID != objectRef(t, edited) || c.ActionCount != 2 || c.LastAction != domain.ActionCommentEdited {
			t.Errorf("comment = %+v, want object %q with 2 rows, last %q (the greater id at a tied instant)",
				c, objectID(t, edited), domain.ActionCommentEdited)
		}
		if !c.FirstAt.Equal(at(3)) || !c.LastAt.Equal(at(3)) {
			t.Errorf("comment first/last = %v/%v, want both %v", c.FirstAt, c.LastAt, at(3))
		}
	})

	t.Run("A_second_conversation_gets_its_own_artifact_for_the_same_object", func(t *testing.T) {
		s := setup(t)
		one, two := s.f.NewConversation(t), s.f.NewConversation(t)
		push := record(t, s, one, branch(s, domain.ActionBranchPushed, "octo/api", ""), at(1))
		record(t, s, one, branch(s, domain.ActionBranchPushed, "octo/api", ""), at(2))
		record(t, s, two, branch(s, domain.ActionBranchPushed, "octo/api", ""), at(3))

		for conv, want := range map[string]int{one: 2, two: 1} {
			got, total := list(t, s, conv, db.Unwindowed)
			if len(got) != 1 || total != 1 {
				t.Fatalf("conversation %s read %d artifacts (total %d), want 1", conv, len(got), total)
			}
			if got[0].ObjectID != objectRef(t, push) || got[0].ActionCount != want {
				t.Errorf("conversation %s artifact = %+v, want object %q with %d rows", conv, got[0], objectID(t, push), want)
			}
		}
	})

	t.Run("System_refusal_and_object_less_rows_form_no_artifact", func(t *testing.T) {
		s := setup(t)
		conv := s.f.NewConversation(t)
		system := pullRequest(s, domain.ActionPRClosed)
		system.PerformedBy = domain.PerformedBySystem
		record(t, s, conv, system, at(2))
		record(t, s, conv, domain.ExternalAction{
			Provider: domain.ProviderGitHub, Scope: TestGitHubHost, Action: domain.ActionGitDenied, Target: "octo/api",
		}, at(3))
		record(t, s, conv, domain.ExternalAction{
			Provider: domain.ProviderGitHub, Scope: TestGitHubHost, Action: domain.ActionGHChannelWrite, Target: "octo/api",
		}, at(4))
		// A kind whose identifying column is empty identifies nothing.
		record(t, s, conv, domain.ExternalAction{
			Provider: domain.ProviderGitHub, Scope: TestGitHubHost, Action: domain.ActionPRMerged,
			Target: "octo/api#7", ObjectKind: domain.ObjectKindPullRequest,
		}, at(5))
		// A refusal and a catch-all are no artifact even when their writer
		// named an object, which the recording funnel never lets one do.
		refusedPush := branch(s, domain.ActionBranchPushFailed, "octo/api", "")
		record(t, s, conv, refusedPush, at(6))
		catchAll := pullRequest(s, domain.ActionGHChannelWrite)
		record(t, s, conv, catchAll, at(7))

		if got, total := list(t, s, conv, db.Unwindowed); len(got) != 0 || total != 0 {
			t.Fatalf("read %d artifacts (total %d), want none: %+v", len(got), total, got)
		}
		if exists, err := s.f.Stores.Artifacts.ExistsForConversationSystem(ctx, s.f.OrgID, conv); err != nil || exists {
			t.Errorf("ExistsForConversationSystem = (%v, %v), want (false, nil)", exists, err)
		}
		counts, err := s.f.Stores.Artifacts.CountByConversations(ctx, s.f.OrgID, []string{conv})
		if err != nil {
			t.Fatalf("CountByConversations: %v", err)
		}
		if _, ok := counts[conv]; ok {
			t.Errorf("CountByConversations = %v, want no entry for the conversation", counts)
		}

		// The agent's own row on the same pull request is the artifact, and
		// TF's and the catch-all's rows do not count toward it.
		record(t, s, conv, pullRequest(s, domain.ActionPRCreated), at(8))
		got, _ := list(t, s, conv, db.Unwindowed)
		if len(got) != 1 || got[0].ActionCount != 1 || got[0].LastAction != domain.ActionPRCreated {
			t.Errorf("after the agent's row: %+v, want one artifact of one pr_created row", got)
		}
	})

	t.Run("A_persons_rows_on_the_runs_deliverables_are_its_artifacts", func(t *testing.T) {
		s := setup(t)
		conv := s.f.NewConversation(t)
		record(t, s, conv, pullRequest(s, domain.ActionPRCreated), at(1))
		// A person's approval of the run's draft joins the agent's artifact.
		ready := pullRequest(s, domain.ActionPRMarkedReady)
		ready.PerformedBy = domain.PerformedByPerson
		record(t, s, conv, ready, at(2))
		// The review a person's approval posts is an artifact of its own.
		review := domain.ExternalAction{
			Provider: domain.ProviderGitHub, Scope: TestGitHubHost, Action: domain.ActionReviewSubmitted,
			Target: "octo/api#7", EntityID: s.pr.ID, PerformedBy: domain.PerformedByPerson,
			ObjectKind: domain.ObjectKindReview, ObjectKey: domain.GitHubReviewObjectKey("4242"),
		}
		record(t, s, conv, review, at(3))

		got, total := list(t, s, conv, db.Unwindowed)
		if len(got) != 2 || total != 2 {
			t.Fatalf("read %d artifacts (total %d), want 2: %+v", len(got), total, got)
		}
		byKind := map[string]domain.Artifact{}
		for _, a := range got {
			byKind[a.ObjectKind] = a
		}
		if pr := byKind[domain.ObjectKindPullRequest]; pr.ActionCount != 2 || pr.LastAction != domain.ActionPRMarkedReady {
			t.Errorf("pull request artifact = %+v, want two rows ending in pr_marked_ready", pr)
		}
		if rv, ok := byKind[domain.ObjectKindReview]; !ok || rv.ObjectID != objectRef(t, review) || rv.LastAction != domain.ActionReviewSubmitted {
			t.Errorf("review artifact = %+v (found %v), want the posted review", rv, ok)
		}
		counts, err := s.f.Stores.Artifacts.CountByConversations(ctx, s.f.OrgID, []string{conv})
		if err != nil {
			t.Fatalf("CountByConversations: %v", err)
		}
		if c := counts[conv]; c[domain.ObjectKindReview] != 1 || c[domain.ObjectKindPullRequest] != 1 {
			t.Errorf("CountByConversations = %v, want one pull request and one review", counts)
		}
	})

	t.Run("An_entity_backed_artifact_displays_the_entitys_current_key_and_url", func(t *testing.T) {
		s := setup(t)
		conv := s.f.NewConversation(t)
		record(t, s, conv, pullRequest(s, domain.ActionPRCreated), at(1))
		if _, err := s.f.Stores.Repos.RenameSystem(ctx, s.f.OrgID, domain.RepoRef{
			Host: TestGitHubHost, Owner: "octo", Repo: "web", ExternalID: "31",
		}); err != nil {
			t.Fatalf("rename repository: %v", err)
		}
		renamed, err := s.f.Stores.Entities.GetSystem(ctx, s.f.OrgID, s.pr.ID)
		if err != nil || renamed == nil {
			t.Fatalf("read renamed entity: %v, %v", renamed, err)
		}
		if renamed.SourceID != "octo/web#7" {
			t.Fatalf("entity key after the rename = %q, want octo/web#7", renamed.SourceID)
		}
		got, _ := list(t, s, conv, db.Unwindowed)
		if len(got) != 1 {
			t.Fatalf("read %d artifacts, want 1", len(got))
		}
		if got[0].Target != renamed.SourceID || got[0].URL != renamed.URL || got[0].EntityID != s.pr.ID {
			t.Errorf("display = (%q, %q, entity %q), want the entity's (%q, %q, %q)",
				got[0].Target, got[0].URL, got[0].EntityID, renamed.SourceID, renamed.URL, s.pr.ID)
		}
	})

	t.Run("An_entity_with_no_url_displays_the_latest_rows_url", func(t *testing.T) {
		s := setup(t)
		conv := s.f.NewConversation(t)
		stub, _, err := s.f.Stores.Entities.FindOrCreateSystem(ctx, s.f.OrgID, "github", TestGitHubHost,
			"octo/api#8", "PR_art8", domain.EntityKindPullRequest, "", "")
		if err != nil {
			t.Fatalf("seed url-less entity: %v", err)
		}
		row := pullRequest(s, domain.ActionPRCreated)
		row.EntityID, row.Target, row.URL = stub.ID, "octo/api#8", TestGitHubHost+"/octo/api/pull/8"
		record(t, s, conv, row, at(1))
		got, _ := list(t, s, conv, db.Unwindowed)
		if len(got) != 1 || got[0].Target != "octo/api#8" || got[0].URL != row.URL {
			t.Errorf("artifacts = %+v, want the entity's key and the row's url %q", got, row.URL)
		}
	})

	t.Run("Counts_by_kind_and_existence", func(t *testing.T) {
		s := setup(t)
		one, two, none := s.f.NewConversation(t), s.f.NewConversation(t), s.f.NewConversation(t)
		record(t, s, one, branch(s, domain.ActionBranchPushed, "octo/api", ""), at(1))
		record(t, s, one, branch(s, domain.ActionBranchPushed, "octo/api", ""), at(2))
		record(t, s, one, pullRequest(s, domain.ActionPRCreated), at(3))
		record(t, s, one, comment(1), at(4))
		record(t, s, one, comment(2), at(5))
		record(t, s, two, branch(s, domain.ActionBranchPushed, "octo/api", ""), at(6))

		counts, err := s.f.Stores.Artifacts.CountByConversations(ctx, s.f.OrgID, []string{one, two, none})
		if err != nil {
			t.Fatalf("CountByConversations: %v", err)
		}
		want := map[string]map[string]int{
			one: {domain.ObjectKindBranch: 1, domain.ObjectKindPullRequest: 1, domain.ObjectKindComment: 2},
			two: {domain.ObjectKindBranch: 1},
		}
		if len(counts) != len(want) {
			t.Errorf("CountByConversations = %v, want %v", counts, want)
		}
		for conv, kinds := range want {
			if len(counts[conv]) != len(kinds) {
				t.Errorf("conversation %s counts = %v, want %v", conv, counts[conv], kinds)
			}
			for kind, n := range kinds {
				if counts[conv][kind] != n {
					t.Errorf("conversation %s %s = %d, want %d", conv, kind, counts[conv][kind], n)
				}
			}
		}
		if empty, err := s.f.Stores.Artifacts.CountByConversations(ctx, s.f.OrgID, nil); err != nil || len(empty) != 0 {
			t.Errorf("CountByConversations(nil) = (%v, %v), want an empty map", empty, err)
		}
		for conv, want := range map[string]bool{one: true, two: true, none: false} {
			if got, err := s.f.Stores.Artifacts.ExistsForConversationSystem(ctx, s.f.OrgID, conv); err != nil || got != want {
				t.Errorf("ExistsForConversationSystem(%s) = (%v, %v), want %v", conv, got, err, want)
			}
		}
	})

	t.Run("Grouped_across_conversations_one_artifact_per_object", func(t *testing.T) {
		s := setup(t)
		one, two, other := s.f.NewConversation(t), s.f.NewConversation(t), s.f.NewConversation(t)
		push := record(t, s, one, branch(s, domain.ActionBranchPushed, "octo/api", ""), at(1))
		posted := record(t, s, one, comment(7), at(2))
		record(t, s, two, branch(s, domain.ActionBranchDeleted, "octo/api", ""), at(3))
		// A conversation outside the set contributes nothing.
		record(t, s, other, branch(s, domain.ActionBranchPushed, "octo/api", ""), at(9))

		got, err := s.f.Stores.Artifacts.ListForConversationsSystem(ctx, s.f.OrgID, []string{one, two})
		if err != nil {
			t.Fatalf("ListForConversationsSystem: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("read %d artifacts, want 2: %+v", len(got), got)
		}
		// FirstAt ascending: the branch (1) before the comment (2).
		b, c := got[0], got[1]
		if b.ObjectID != objectRef(t, push) || b.ActionCount != 2 || b.LastAction != domain.ActionBranchDeleted ||
			!b.FirstAt.Equal(at(1)) || !b.LastAt.Equal(at(3)) {
			t.Errorf("branch = %+v, want 2 rows from %v to %v, last %q", b, at(1), at(3), domain.ActionBranchDeleted)
		}
		if c.ObjectID != objectRef(t, posted) || c.ActionCount != 1 {
			t.Errorf("comment = %+v, want object %q with 1 row", c, objectID(t, posted))
		}
		if none, err := s.f.Stores.Artifacts.ListForConversationsSystem(ctx, s.f.OrgID, nil); err != nil || len(none) != 0 {
			t.Errorf("ListForConversationsSystem(nil) = (%v, %v), want nothing", none, err)
		}
	})

	t.Run("The_action_list_filters_by_object_and_by_refusal", func(t *testing.T) {
		s := setup(t)
		conv := s.f.NewConversation(t)
		push := record(t, s, conv, branch(s, domain.ActionBranchPushed, "octo/api", ""), at(1))
		again := record(t, s, conv, branch(s, domain.ActionBranchPushed, "octo/api", ""), at(2))
		record(t, s, conv, comment(1), at(3))
		denied := record(t, s, conv, domain.ExternalAction{
			Provider: domain.ProviderGitHub, Scope: TestGitHubHost, Action: domain.ActionGitDenied, Target: "octo/api",
		}, at(4))
		failed := record(t, s, conv, domain.ExternalAction{
			Provider: domain.ProviderGitHub, Scope: TestGitHubHost, Action: domain.ActionBranchPushFailed, Target: "octo/api",
		}, at(5))

		ids := func(t *testing.T, opts domain.ExternalActionListOpts) ([]string, int) {
			t.Helper()
			rows, total, err := s.f.Stores.ExternalActions.ListByConversation(ctx, s.f.OrgID, conv, opts)
			if err != nil {
				t.Fatalf("ListByConversation(%+v): %v", opts, err)
			}
			out := make([]string, len(rows))
			for i, r := range rows {
				out[i] = r.ID
			}
			return out, total
		}
		if got, total := ids(t, domain.ExternalActionListOpts{ObjectID: objectID(t, push)}); !slices.Equal(got, []string{again.ID, push.ID}) || total != 2 {
			t.Errorf("object filter = %v (total %d), want the branch's two rows newest first", got, total)
		}
		if got, total := ids(t, domain.ExternalActionListOpts{Refused: true}); !slices.Equal(got, []string{failed.ID, denied.ID}) || total != 2 {
			t.Errorf("refusal filter = %v (total %d), want the two refused or failed rows", got, total)
		}
		if got, total := ids(t, domain.ExternalActionListOpts{ObjectID: objectID(t, push), Refused: true}); len(got) != 0 || total != 0 {
			t.Errorf("both filters = %v (total %d), want nothing: no row is both", got, total)
		}
		if got, total := ids(t, domain.ExternalActionListOpts{ObjectID: "branch|nothing|refs/heads/none"}); len(got) != 0 || total != 0 {
			t.Errorf("an object no row names = %v (total %d), want nothing", got, total)
		}
	})

	t.Run("Paging_totals_count_groups", func(t *testing.T) {
		s := setup(t)
		conv := s.f.NewConversation(t)
		record(t, s, conv, branch(s, domain.ActionBranchPushed, "octo/api", ""), at(1))
		record(t, s, conv, branch(s, domain.ActionBranchPushed, "octo/api", ""), at(2))
		record(t, s, conv, comment(1), at(3))
		record(t, s, conv, comment(1), at(4))
		record(t, s, conv, pullRequest(s, domain.ActionPRCreated), at(5))

		all, total := list(t, s, conv, db.Unwindowed)
		if len(all) != 3 || total != 3 {
			t.Fatalf("unwindowed read = %d (total %d), want 3/3", len(all), total)
		}
		first, total := list(t, s, conv, db.ListOpts{Limit: 2})
		if len(first) != 2 || total != 3 {
			t.Fatalf("page 1 = %d (total %d), want 2/3", len(first), total)
		}
		second, total := list(t, s, conv, db.ListOpts{Limit: 2, Offset: 2})
		if len(second) != 1 || total != 3 {
			t.Fatalf("page 2 = %d (total %d), want 1/3", len(second), total)
		}
		for i, a := range append(first, second...) {
			if a.ObjectID != all[i].ObjectID {
				t.Errorf("paged item %d = %q, unwindowed %q", i, a.ObjectID, all[i].ObjectID)
			}
		}
		count, total := list(t, s, conv, db.ListOpts{CountOnly: true})
		if len(count) != 0 || total != 3 {
			t.Errorf("count-only read = %d items (total %d), want 0/3", len(count), total)
		}
		if _, _, err := s.f.Stores.Artifacts.ListByConversation(ctx, s.f.OrgID, conv, db.ListOpts{Limit: 2, After: []string{"x"}}); err == nil {
			t.Error("a keyset window was accepted, want it refused")
		}
	})

	// scopeList reads one scope: the team when teamID is set, else the org.
	scopeList := func(t *testing.T, s seed, teamID string, opts db.ArtifactListOpts) ([]domain.ObjectActivity, int) {
		t.Helper()
		var (
			got   []domain.ObjectActivity
			total int
			err   error
		)
		if teamID != "" {
			got, total, err = s.f.Stores.Artifacts.ListByTeam(ctx, s.f.OrgID, teamID, opts)
		} else {
			got, total, err = s.f.Stores.Artifacts.ListByOrgSystem(ctx, s.f.OrgID, opts)
		}
		if err != nil {
			t.Fatalf("scope read (team %q, %+v): %v", teamID, opts, err)
		}
		return got, total
	}
	pageOf := func(limit, offset int) db.ArtifactListOpts {
		return db.ArtifactListOpts{ListOpts: db.ListOpts{Limit: limit, Offset: offset}}
	}

	t.Run("Scope_reads_group_an_object_across_runs_and_teams", func(t *testing.T) {
		s := setup(t)
		one, two, three := s.f.NewConversation(t), s.f.NewConversation(t), s.f.NewConversation(t)
		record(t, s, one, branch(s, domain.ActionBranchPushed, "octo/old", ""), at(1))
		record(t, s, one, branch(s, domain.ActionBranchPushed, "octo/old", ""), at(2))
		record(t, s, two, branch(s, domain.ActionBranchPushed, "octo/old", ""), at(3))
		other := branch(s, domain.ActionBranchPushed, "octo/api", TestGitHubHost+"/octo/api/tree/fix")
		other.TeamID = s.f.OtherTeamID
		latest := record(t, s, three, other, at(4))
		// A row whose run was purged is the object's too, and counts no run.
		record(t, s, "", branch(s, domain.ActionBranchDeleted, "octo/old", ""), at(0))

		got, total := scopeList(t, s, "", pageOf(50, 0))
		if len(got) != 1 || total != 1 {
			t.Fatalf("org read = %d objects (total %d), want 1: %+v", len(got), total, got)
		}
		b := got[0]
		if b.ObjectID != objectRef(t, latest) || b.ObjectKind != domain.ObjectKindBranch || b.RepositoryID != s.repo.ID {
			t.Errorf("org object = %+v, want the branch %q", b, objectID(t, latest))
		}
		if b.RunCount != 3 || b.ActionCount != 5 || b.LastAction != domain.ActionBranchPushed {
			t.Errorf("org counts = runs %d, actions %d, last %q; want 3, 5, %q", b.RunCount, b.ActionCount, b.LastAction, domain.ActionBranchPushed)
		}
		if b.Target != "octo/api" || b.URL != latest.URL || !b.FirstAt.Equal(at(0)) || !b.LastAt.Equal(at(4)) {
			t.Errorf("org display = (%q, %q, %v, %v), want the latest row's from %v to %v", b.Target, b.URL, b.FirstAt, b.LastAt, at(0), at(4))
		}
		wantTeams := []string{s.f.TeamID, s.f.OtherTeamID}
		slices.Sort(wantTeams)
		if !slices.Equal(b.TeamIDs, wantTeams) {
			t.Errorf("org TeamIDs = %v, want %v sorted", b.TeamIDs, wantTeams)
		}

		// The team read keeps the team's rows alone.
		mine, total := scopeList(t, s, s.f.TeamID, pageOf(50, 0))
		if len(mine) != 1 || total != 1 || mine[0].RunCount != 2 || mine[0].ActionCount != 4 ||
			!slices.Equal(mine[0].TeamIDs, []string{s.f.TeamID}) || mine[0].Target != "octo/old" {
			t.Errorf("team read = %+v (total %d), want the branch from 2 runs and 4 rows on the team, displayed as its latest row", mine, total)
		}
		theirs, _ := scopeList(t, s, s.f.OtherTeamID, pageOf(50, 0))
		if len(theirs) != 1 || theirs[0].RunCount != 1 || theirs[0].ActionCount != 1 ||
			!slices.Equal(theirs[0].TeamIDs, []string{s.f.OtherTeamID}) {
			t.Errorf("other team read = %+v, want the branch from its one run", theirs)
		}
		if none, total := scopeList(t, s, uuid.NewString(), pageOf(50, 0)); len(none) != 0 || total != 0 {
			t.Errorf("a team with no rows read %d objects (total %d)", len(none), total)
		}
	})

	t.Run("Scope_reads_skip_system_and_refusal_rows", func(t *testing.T) {
		s := setup(t)
		conv := s.f.NewConversation(t)
		system := pullRequest(s, domain.ActionPRClosed)
		system.PerformedBy = domain.PerformedBySystem
		record(t, s, conv, system, at(2))
		record(t, s, conv, branch(s, domain.ActionBranchPushFailed, "octo/api", ""), at(3))
		record(t, s, conv, pullRequest(s, domain.ActionGHChannelWrite), at(4))
		for _, teamID := range []string{"", s.f.TeamID} {
			if got, total := scopeList(t, s, teamID, pageOf(50, 0)); len(got) != 0 || total != 0 {
				t.Errorf("scope %q read %d objects (total %d), want none: %+v", teamID, len(got), total, got)
			}
		}
	})

	t.Run("Scope_reads_window_and_filter_rows", func(t *testing.T) {
		s := setup(t)
		conv := s.f.NewConversation(t)
		record(t, s, conv, branch(s, domain.ActionBranchPushed, "octo/api", ""), at(1))
		record(t, s, conv, branch(s, domain.ActionBranchPushed, "octo/api", ""), at(3))
		pr := record(t, s, conv, pullRequest(s, domain.ActionPRCreated), at(2))
		issue := record(t, s, conv, domain.ExternalAction{
			Provider: domain.ProviderJira, Scope: "https://acme.atlassian.net", Action: domain.ActionIssueTransitioned,
			Target: "SKY-1", Credential: domain.CredentialJiraOrg,
			ObjectKind: domain.ObjectKindIssue, ObjectKey: "jira:issue:acme-10001",
		}, at(4))

		// The window is half-open: a row at Since is in, a row at Until out.
		got, total := scopeList(t, s, "", db.ArtifactListOpts{Since: at(2), Until: at(4), ListOpts: db.ListOpts{Limit: 50}})
		if len(got) != 2 || total != 2 {
			t.Fatalf("windowed read = %d objects (total %d), want the branch and the PR: %+v", len(got), total, got)
		}
		if got[0].ObjectKind != domain.ObjectKindBranch || got[0].ActionCount != 1 || !got[0].FirstAt.Equal(at(3)) {
			t.Errorf("windowed branch = %+v, want its one row at %v", got[0], at(3))
		}
		if got[1].ObjectID != objectRef(t, pr) {
			t.Errorf("windowed second = %q, want the PR %q", got[1].ObjectID, objectID(t, pr))
		}
		for name, tc := range map[string]struct {
			opts db.ArtifactListOpts
			want domain.ObjectRef
		}{
			"provider":    {db.ArtifactListOpts{Provider: domain.ProviderJira}, objectRef(t, issue)},
			"object_kind": {db.ArtifactListOpts{ObjectKind: domain.ObjectKindPullRequest}, objectRef(t, pr)},
			"both":        {db.ArtifactListOpts{Provider: domain.ProviderGitHub, ObjectKind: domain.ObjectKindPullRequest}, objectRef(t, pr)},
		} {
			tc.opts.Limit = 50
			for _, teamID := range []string{"", s.f.TeamID} {
				got, total := scopeList(t, s, teamID, tc.opts)
				if len(got) != 1 || total != 1 || got[0].ObjectID != tc.want {
					t.Errorf("%s filter (team %q) = %+v (total %d), want %q alone", name, teamID, got, total, tc.want)
				}
			}
		}
		if got, total := scopeList(t, s, "", db.ArtifactListOpts{Provider: domain.ProviderSlack, ListOpts: db.ListOpts{Limit: 50}}); len(got) != 0 || total != 0 {
			t.Errorf("a provider no row has = %d objects (total %d), want none", len(got), total)
		}
	})

	t.Run("Scope_reads_order_and_page_objects", func(t *testing.T) {
		s := setup(t)
		conv := s.f.NewConversation(t)
		record(t, s, conv, branch(s, domain.ActionBranchPushed, "octo/api", ""), at(1))
		record(t, s, conv, branch(s, domain.ActionBranchPushed, "octo/api", ""), at(5))
		record(t, s, conv, pullRequest(s, domain.ActionPRCreated), at(3))
		// Two comments last changed at one instant: ObjectID breaks the tie.
		record(t, s, conv, comment(1), at(2))
		record(t, s, conv, comment(2), at(2))

		all, total := scopeList(t, s, "", pageOf(50, 0))
		if len(all) != 4 || total != 4 {
			t.Fatalf("org read = %d objects (total %d), want 4", len(all), total)
		}
		if all[0].ObjectKind != domain.ObjectKindBranch || all[1].ObjectKind != domain.ObjectKindPullRequest {
			t.Errorf("order = %q, %q first, want the branch (5) then the PR (3)", all[0].ObjectKind, all[1].ObjectKind)
		}
		if string(all[2].ObjectID) >= string(all[3].ObjectID) {
			t.Errorf("tied comments = %q, %q, want ascending object id", all[2].ObjectID, all[3].ObjectID)
		}
		first, total := scopeList(t, s, "", pageOf(3, 0))
		second, total2 := scopeList(t, s, "", pageOf(3, 3))
		if len(first) != 3 || len(second) != 1 || total != 4 || total2 != 4 {
			t.Fatalf("pages = %d + %d (totals %d, %d), want 3 + 1 of 4", len(first), len(second), total, total2)
		}
		for i, a := range append(first, second...) {
			if a.ObjectID != all[i].ObjectID {
				t.Errorf("paged item %d = %q, unwindowed %q", i, a.ObjectID, all[i].ObjectID)
			}
		}
		count, total := scopeList(t, s, s.f.TeamID, db.ArtifactListOpts{ListOpts: db.ListOpts{CountOnly: true}})
		if len(count) != 0 || total != 4 {
			t.Errorf("count-only team read = %d items (total %d), want 0/4", len(count), total)
		}
		if _, _, err := s.f.Stores.Artifacts.ListByOrgSystem(ctx, s.f.OrgID, db.ArtifactListOpts{ListOpts: db.ListOpts{Limit: 2, After: []string{"x"}}}); err == nil {
			t.Error("a keyset window was accepted, want it refused")
		}
	})

	t.Run("Deliverable_match_reads_return_the_candidates_newest_first", func(t *testing.T) {
		s := setup(t)
		other, _, err := s.f.Stores.Entities.FindOrCreateSystem(ctx, s.f.OrgID, "github", TestGitHubHost,
			"octo/api#9", "PR_art9", domain.EntityKindPullRequest, "Another PR", TestGitHubHost+"/octo/api/pull/9")
		if err != nil {
			t.Fatalf("seed second entity: %v", err)
		}
		n := 0
		store := func(teamID string, d domain.Deliverable) string {
			t.Helper()
			n++
			d.TeamID, d.Provider, d.Scope = teamID, domain.ProviderGitHub, TestGitHubHost
			d.DedupKey = uuid.NewString()
			if d.State == "" {
				d.State = domain.DeliverableStatePROpen
			}
			stored, err := s.f.Stores.Deliverables.UpsertSystem(ctx, s.f.OrgID, d)
			if err != nil {
				t.Fatalf("seed deliverable %d: %v", n, err)
			}
			s.f.SetDeliverableCreatedAt(t, stored.ID, at(n))
			return stored.ID
		}
		pr := func(entityID string) domain.Deliverable {
			return domain.Deliverable{Kind: domain.DeliverableKindPullRequest, EntityID: entityID, Target: "octo/api#7"}
		}
		review := func(externalID string) domain.Deliverable {
			return domain.Deliverable{Kind: domain.DeliverableKindReview, ExternalID: externalID, Target: "octo/api#7",
				State: domain.DeliverableStateReviewSubmitted}
		}
		olderPR := store(s.f.TeamID, pr(s.pr.ID))
		posted := store(s.f.TeamID, review("55"))
		newerPR := store(s.f.TeamID, pr(s.pr.ID))
		theirPR := store(s.f.OtherTeamID, pr(s.pr.ID))
		store(s.f.TeamID, pr(other.ID))       // another entity
		store(s.f.TeamID, review("56"))       // another review
		store(s.f.TeamID, domain.Deliverable{ // a branch on the entity matches nothing
			Kind: domain.DeliverableKindBranch, EntityID: s.pr.ID, Target: "octo/api", ExternalID: "refs/heads/fix",
			State: domain.DeliverableStateBranchPushed,
		})

		ids := func(ds []domain.Deliverable) []string {
			out := make([]string, len(ds))
			for i, d := range ds {
				out[i] = d.ID
			}
			return out
		}
		team, err := s.f.Stores.Deliverables.ListArtifactMatchesByTeamSystem(ctx, s.f.OrgID, s.f.TeamID, []string{s.pr.ID}, []string{"55"})
		if err != nil {
			t.Fatalf("ListArtifactMatchesByTeamSystem: %v", err)
		}
		if got, want := ids(team), []string{newerPR, posted, olderPR}; !slices.Equal(got, want) {
			t.Errorf("team candidates = %v, want %v (newest first, the team's alone)", got, want)
		}
		org, err := s.f.Stores.Deliverables.ListArtifactMatchesByOrgSystem(ctx, s.f.OrgID, []string{s.pr.ID}, []string{"55"})
		if err != nil {
			t.Fatalf("ListArtifactMatchesByOrgSystem: %v", err)
		}
		if got, want := ids(org), []string{theirPR, newerPR, posted, olderPR}; !slices.Equal(got, want) {
			t.Errorf("org candidates = %v, want %v", got, want)
		}
		reviewsOnly, err := s.f.Stores.Deliverables.ListArtifactMatchesByOrgSystem(ctx, s.f.OrgID, nil, []string{"55"})
		if err != nil || !slices.Equal(ids(reviewsOnly), []string{posted}) {
			t.Errorf("review-only candidates = %v, %v; want %s", ids(reviewsOnly), err, posted)
		}
		prsOnly, err := s.f.Stores.Deliverables.ListArtifactMatchesByTeamSystem(ctx, s.f.OrgID, s.f.TeamID, []string{s.pr.ID}, nil)
		if err != nil || !slices.Equal(ids(prsOnly), []string{newerPR, olderPR}) {
			t.Errorf("PR-only candidates = %v, %v; want %s, %s", ids(prsOnly), err, newerPR, olderPR)
		}
		if none, err := s.f.Stores.Deliverables.ListArtifactMatchesByOrgSystem(ctx, s.f.OrgID, nil, nil); err != nil || len(none) != 0 {
			t.Errorf("no keys = %v, %v; want nothing", none, err)
		}

		// The artifact's match is the newest candidate.
		match, ok := domain.ArtifactDeliverable(domain.Artifact{ObjectKind: domain.ObjectKindPullRequest, EntityID: s.pr.ID}, team)
		if !ok || match.ID != newerPR {
			t.Errorf("match = %s (%v), want the newest pull request %s", match.ID, ok, newerPR)
		}
	})
}

// sameArtifact compares two artifacts field by field, the times by instant.
func sameArtifact(a, b domain.Artifact) bool {
	ta, tb := a, b
	ta.FirstAt, ta.LastAt, tb.FirstAt, tb.LastAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}
	return ta == tb && a.FirstAt.Equal(b.FirstAt) && a.LastAt.Equal(b.LastAt)
}
