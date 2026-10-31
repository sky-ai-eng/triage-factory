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
// store bundle, the org every call is for, a team the ledger rows hang off,
// a way to mint a conversation in that team, and a way to set a ledger row's
// occurred_at to a given instant in the shape the column's default writes.
type ArtifactFixture struct {
	Stores          db.Stores
	OrgID, TeamID   string
	NewConversation func(t *testing.T) string
	SetOccurredAt   func(t *testing.T, actionID string, at time.Time)
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
// object and refusal filters, which read the same identity; and paging
// totals that count groups.
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
		a.TeamID = s.f.TeamID
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

	t.Run("Person_system_refusal_and_object_less_rows_form_no_artifact", func(t *testing.T) {
		s := setup(t)
		conv := s.f.NewConversation(t)
		person := pullRequest(s, domain.ActionPREdited)
		person.PerformedBy = domain.PerformedByPerson
		record(t, s, conv, person, at(1))
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

		// The agent's own row on the same pull request is the artifact, and the
		// person's, TF's and the catch-all's rows do not count toward it.
		record(t, s, conv, pullRequest(s, domain.ActionPRCreated), at(8))
		got, _ := list(t, s, conv, db.Unwindowed)
		if len(got) != 1 || got[0].ActionCount != 1 || got[0].LastAction != domain.ActionPRCreated {
			t.Errorf("after the agent's row: %+v, want one artifact of one pr_created row", got)
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
}

// sameArtifact compares two artifacts field by field, the times by instant.
func sameArtifact(a, b domain.Artifact) bool {
	ta, tb := a, b
	ta.FirstAt, ta.LastAt, tb.FirstAt, tb.LastAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}
	return ta == tb && a.FirstAt.Equal(b.FirstAt) && a.LastAt.Equal(b.LastAt)
}
