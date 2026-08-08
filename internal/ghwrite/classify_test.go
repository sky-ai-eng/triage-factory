package ghwrite

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestClassify_TableShapes walks the shapes an agent actually reaches for
// through the real-`gh` channel and pins what each one is recorded as. The
// dotcom and GHES path forms are both exercised, since the injector forwards
// whichever the org's upstream uses.
func TestClassify_TableShapes(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		path    string
		want    Shape
		wantErr bool // want no classification at all
	}{
		{
			name:   "review thread reply",
			method: "POST",
			path:   "/repos/acme/widgets/pulls/841/comments/555/replies",
			want: Shape{
				Action: domain.ActionCommentPosted, Owner: "acme", Repo: "widgets",
				Number: 841, InReplyTo: 555, CreatesObject: true,
			},
		},
		{
			name:   "review thread reply on GHES",
			method: "POST",
			path:   "/api/v3/repos/acme/widgets/pulls/841/comments/555/replies",
			want: Shape{
				Action: domain.ActionCommentPosted, Owner: "acme", Repo: "widgets",
				Number: 841, InReplyTo: 555, CreatesObject: true,
			},
		},
		{
			name:   "issue comment create",
			method: "POST",
			path:   "/repos/acme/widgets/issues/7/comments",
			want: Shape{
				Action: domain.ActionCommentPosted, Owner: "acme", Repo: "widgets",
				Number: 7, CreatesObject: true,
			},
		},
		{
			name:   "issue comment edit",
			method: "PATCH",
			path:   "/repos/acme/widgets/issues/comments/5",
			want:   Shape{Action: domain.ActionCommentEdited, Owner: "acme", Repo: "widgets", ExternalID: "5"},
		},
		{
			name:   "issue comment delete",
			method: "DELETE",
			path:   "/repos/acme/widgets/issues/comments/5",
			want:   Shape{Action: domain.ActionCommentDeleted, Owner: "acme", Repo: "widgets", ExternalID: "5"},
		},
		{
			name:   "review comment edit",
			method: "PATCH",
			path:   "/repos/acme/widgets/pulls/comments/5",
			want:   Shape{Action: domain.ActionReviewCommentEdited, Owner: "acme", Repo: "widgets", ExternalID: "5"},
		},
		{
			name:   "review comment delete",
			method: "DELETE",
			path:   "/repos/acme/widgets/pulls/comments/5",
			want:   Shape{Action: domain.ActionReviewCommentDeleted, Owner: "acme", Repo: "widgets", ExternalID: "5"},
		},
		{
			name:   "pull request edit",
			method: "PATCH",
			path:   "/repos/acme/widgets/pulls/841",
			want:   Shape{Action: domain.ActionPREdited, Owner: "acme", Repo: "widgets", Number: 841},
		},
		{
			name:   "merge",
			method: "PUT",
			path:   "/repos/acme/widgets/pulls/841/merge",
			want:   Shape{Action: domain.ActionPRMerged, Owner: "acme", Repo: "widgets", Number: 841},
		},
		{
			name:   "reaction on an issue",
			method: "POST",
			path:   "/repos/acme/widgets/issues/7/reactions",
			want:   Shape{Action: domain.ActionReactionAdded, Owner: "acme", Repo: "widgets", Number: 7},
		},
		{
			name:   "reaction on a review comment",
			method: "POST",
			path:   "/repos/acme/widgets/pulls/comments/5/reactions",
			want:   Shape{Action: domain.ActionReactionAdded, Owner: "acme", Repo: "widgets", ExternalID: "5"},
		},
		{
			name:   "reaction removed from a comment",
			method: "DELETE",
			path:   "/repos/acme/widgets/issues/comments/5/reactions/99",
			want:   Shape{Action: domain.ActionReactionRemoved, Owner: "acme", Repo: "widgets", ExternalID: "5"},
		},
		{
			// The issue-level removal addresses the object by number, so it is
			// the one reaction shape whose target carries a '#N'.
			name:   "reaction removed from an issue",
			method: "DELETE",
			path:   "/repos/acme/widgets/issues/7/reactions/99",
			want:   Shape{Action: domain.ActionReactionRemoved, Owner: "acme", Repo: "widgets", Number: 7},
		},
		{
			name:   "workflow dispatch",
			method: "POST",
			path:   "/repos/acme/widgets/actions/workflows/ci.yml/dispatches",
			want:   Shape{Action: domain.ActionWorkflowDispatched, Owner: "acme", Repo: "widgets", ExternalID: "ci.yml"},
		},
		{
			name:   "workflow run cancel",
			method: "POST",
			path:   "/repos/acme/widgets/actions/runs/12345/cancel",
			want:   Shape{Action: domain.ActionWorkflowRunCancelled, Owner: "acme", Repo: "widgets", ExternalID: "12345"},
		},

		{
			// The number is GitHub's to assign, so the path cannot carry it —
			// Resolve fills it from the created PR's url (see the test below).
			name:   "pull request create",
			method: "POST",
			path:   "/repos/acme/widgets/pulls",
			want: Shape{
				Action: domain.ActionPRCreated, Owner: "acme", Repo: "widgets",
				CreatesObject: true, NumberFromObjectURL: true, CarriesProvenance: true,
			},
		},
		{
			name:   "review post",
			method: "POST",
			path:   "/repos/acme/widgets/pulls/841/reviews",
			want: Shape{
				Action: domain.ActionReviewSubmitted, Owner: "acme", Repo: "widgets",
				Number: 841, CreatesObject: true, CarriesProvenance: true,
			},
		},

		// Deliberately unclassified.
		{name: "org-level endpoint", method: "POST", path: "/orgs/acme/repos", wantErr: true},
		{name: "unmodeled repo endpoint", method: "PUT", path: "/repos/acme/widgets/topics", wantErr: true},
		{name: "wrong method for the shape", method: "POST", path: "/repos/acme/widgets/pulls/841/merge", wantErr: true},
		{name: "truncated path", method: "PATCH", path: "/repos/acme", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Classify(tc.method, tc.path)
			if tc.wantErr {
				if ok {
					t.Fatalf("Classify(%s %s) = %+v, want no classification", tc.method, tc.path, got)
				}
				return
			}
			if !ok {
				t.Fatalf("Classify(%s %s) found no shape", tc.method, tc.path)
			}
			if got != tc.want {
				t.Errorf("Classify(%s %s) =\n  %+v\nwant\n  %+v", tc.method, tc.path, got, tc.want)
			}
		})
	}
}

// TestClassify_ReadsAreNeverWrites guards the charter boundary: a read is not
// an act the audit log records, and no GET may ever pick up a verb — the
// injector filters on method too, so this is belt and braces on the shape a
// read shares with a write.
func TestClassify_ReadsAreNeverWrites(t *testing.T) {
	for _, path := range []string{
		"/repos/acme/widgets/pulls/841",
		"/repos/acme/widgets/issues/7/comments",
		"/repos/acme/widgets/issues/comments/5",
	} {
		if got, ok := Classify("GET", path); ok {
			t.Errorf("Classify(GET %s) = %+v, want no classification", path, got)
		}
	}
}

// TestShapeTarget_PrefersTheEntityForm pins the target rule the entity-touch
// path depends on: owner/repo#N whenever the path carries a number, owner/repo
// when it addresses an object by its own id.
func TestShapeTarget_PrefersTheEntityForm(t *testing.T) {
	numbered, _ := Classify("PUT", "/repos/acme/widgets/pulls/841/merge")
	if got := numbered.Target(); got != "acme/widgets#841" {
		t.Errorf("target = %q, want the entity-shaped acme/widgets#841", got)
	}
	byID, _ := Classify("DELETE", "/repos/acme/widgets/issues/comments/5")
	if got := byID.Target(); got != "acme/widgets" {
		t.Errorf("target = %q, want the repo — the path names a comment, not a PR", got)
	}
	if got := (Shape{}).Target(); got != "" {
		t.Errorf("empty shape target = %q, want empty", got)
	}
}

// TestResolve_CompletesACreateFromItsResponse pins the half of a create's
// identity that the request cannot carry. A pull request is the sharp case: its
// response id is the REST database id, while the product addresses a PR by its
// number everywhere else, so the number comes off the object's url and the
// database id is dropped rather than filed under a field that means something
// different.
func TestResolve_CompletesACreateFromItsResponse(t *testing.T) {
	created := Observation{
		Method:     "POST",
		Path:       "/repos/acme/widgets/pulls",
		Status:     201,
		ExternalID: "2314567890", // GitHub's database id, not the number
		URL:        "https://github.com/acme/widgets/pull/42",
	}
	got, ok := Resolve(created)
	if !ok {
		t.Fatal("a PR create resolved to no shape")
	}
	if got.Number != 42 || got.ExternalID != "42" || got.Target() != "acme/widgets#42" {
		t.Errorf("resolved = %+v (target %q), want the PR's number everywhere", got, got.Target())
	}

	// A url that never arrived (a body past the cap) must not leave the
	// database id standing in for a number.
	noURL := created
	noURL.URL = ""
	got, ok = Resolve(noURL)
	if !ok {
		t.Fatal("a PR create with no url resolved to no shape")
	}
	if got.ExternalID != "" || got.Number != 0 || got.Target() != "acme/widgets" {
		t.Errorf("resolved = %+v (target %q), want the act on its repo and no invented id", got, got.Target())
	}

	// A review's response id IS its addressable id, so it rides through.
	review, ok := Resolve(Observation{
		Method: "POST", Path: "/repos/acme/widgets/pulls/841/reviews", Status: 200,
		ExternalID: "999", URL: "https://github.com/acme/widgets/pull/841#pullrequestreview-999",
	})
	if !ok || review.ExternalID != "999" || review.Target() != "acme/widgets#841" {
		t.Errorf("resolved review = %+v, want the review id on acme/widgets#841", review)
	}

	if _, ok := Resolve(Observation{Method: "POST", Path: "/user/repos", Status: 201}); ok {
		t.Error("an unclassified shape resolved to a shape")
	}
}

// TestParsePullRequestURL_TakesTheRealSegment pins the parser both the write
// classifier and the injector's GraphQL observation read PR identity through.
func TestParsePullRequestURL_TakesTheRealSegment(t *testing.T) {
	owner, repo, number, ok := ParsePullRequestURL("https://ghe.corp/acme/widgets/pull/7")
	if !ok || owner != "acme" || repo != "widgets" || number != 7 {
		t.Errorf("GHES url = (%q, %q, %d, %v), want acme/widgets#7", owner, repo, number, ok)
	}
	// A repository literally named "pull" still resolves — the scan needs two
	// segments before the marker and a positive integer after it.
	owner, repo, number, ok = ParsePullRequestURL("https://github.com/acme/pull/pull/9")
	if !ok || owner != "acme" || repo != "pull" || number != 9 {
		t.Errorf("repo named pull = (%q, %q, %d, %v), want acme/pull#9", owner, repo, number, ok)
	}
	for _, raw := range []string{"", "https://github.com/acme/widgets", "https://github.com/acme/widgets/pull/x", "://"} {
		if _, _, _, ok := ParsePullRequestURL(raw); ok {
			t.Errorf("ParsePullRequestURL(%q) parsed, want a decline", raw)
		}
	}
}

// TestObservation_SucceededSplitsTheOutcome pins the rule the audit builder
// hangs on: only a 2xx earns the verb.
func TestObservation_SucceededSplitsTheOutcome(t *testing.T) {
	for status, want := range map[int]bool{200: true, 201: true, 204: true, 301: false, 404: false, 500: false} {
		if got := (Observation{Status: status}).Succeeded(); got != want {
			t.Errorf("Succeeded(%d) = %v, want %v", status, got, want)
		}
	}
}

// TestRepoPath_ExtractsOrDeclines pins the fallback row's target source: an
// unclassified write still files under its repo, and a path naming none gets
// nothing invented for it.
func TestRepoPath_ExtractsOrDeclines(t *testing.T) {
	cases := map[string]string{
		"/repos/acme/widgets/topics":        "acme/widgets",
		"/api/v3/repos/acme/widgets/topics": "acme/widgets",
		"/repos/acme/widgets":               "acme/widgets",
		"/user/repos":                       "",
		"/repos/acme":                       "",
		"/orgs/acme/repos":                  "",
	}
	for path, want := range cases {
		if got := RepoPath(path); got != want {
			t.Errorf("RepoPath(%q) = %q, want %q", path, got, want)
		}
	}
}
