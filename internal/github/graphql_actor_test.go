package github

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestBuildSnapshot_BotLoginsSpelledREST pins that a Bot actor's login lands
// in the snapshot in REST's "<slug>[bot]" form, on both the PR author and a
// review's author, while a User's login is unchanged. GraphQL returns a bot's
// login as the bare slug; handler predicates and TF's own App identity use the
// suffixed form.
func TestBuildSnapshot_BotLoginsSpelledREST(t *testing.T) {
	const body = `{
	  "number": 9,
	  "repository": {"nameWithOwner": "octo/repo"},
	  "author": {"__typename": "Bot", "login": "dependabot"},
	  "latestReviews": {"nodes": [
	    {"id": "PRR_1", "author": {"__typename": "Bot", "login": "copilot-pull-request-reviewer"}, "state": "COMMENTED", "submittedAt": "2026-10-01T10:00:00Z"},
	    {"id": "PRR_2", "author": {"__typename": "User", "login": "alice"}, "state": "APPROVED", "submittedAt": "2026-10-01T11:00:00Z"},
	    {"id": "PRR_3", "author": {"__typename": "Bot", "login": "renovate[bot]"}, "state": "COMMENTED", "submittedAt": "2026-10-01T12:00:00Z"}
	  ]}
	}`
	var pr gqlPR
	if err := json.Unmarshal([]byte(body), &pr); err != nil {
		t.Fatalf("decode: %v", err)
	}

	wantReviews := []string{"copilot-pull-request-reviewer[bot]", "alice", "renovate[bot]"}
	for name, snap := range map[string]domain.PRSnapshot{
		"full":      pr.toSnapshot(),
		"discovery": pr.toDiscoverySnapshot(),
	} {
		if snap.Author != "dependabot[bot]" {
			t.Errorf("%s: Author = %q, want dependabot[bot]", name, snap.Author)
		}
		if len(snap.Reviews) != len(wantReviews) {
			t.Fatalf("%s: got %d reviews, want %d", name, len(snap.Reviews), len(wantReviews))
		}
		for i, want := range wantReviews {
			if got := snap.Reviews[i].Author; got != want {
				t.Errorf("%s: review[%d] author = %q, want %q", name, i, got, want)
			}
		}
	}
}

// TestBuildSnapshot_UserLoginUnchanged pins that a person's PR keeps its login
// as GraphQL returns it, including when __typename is absent.
func TestBuildSnapshot_UserLoginUnchanged(t *testing.T) {
	for _, author := range []string{
		`{"__typename": "User", "login": "octocat"}`,
		`{"login": "octocat"}`,
	} {
		var pr gqlPR
		if err := json.Unmarshal([]byte(`{"number": 1, "author": `+author+`}`), &pr); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got := pr.toSnapshot().Author; got != "octocat" {
			t.Errorf("author %s: Author = %q, want octocat", author, got)
		}
	}
}

// TestPRBaseFields_SelectActorTypename pins that every actor the snapshot
// records a login from selects __typename, without which a bot decodes as a
// person and keeps GraphQL's bare slug, and the account's id.
func TestPRBaseFields_SelectActorTypename(t *testing.T) {
	const sel = "author { __typename login ... on User { databaseId } ... on Bot { databaseId } }"
	if n := strings.Count(prBaseFields, sel); n != 2 {
		t.Errorf("prBaseFields selects %q %d times, want 2 (PR author, latestReviews author)", sel, n)
	}
	if n := strings.Count(prBaseFields, "author { login }"); n != 0 {
		t.Errorf("prBaseFields has %d author selections without __typename", n)
	}
}
