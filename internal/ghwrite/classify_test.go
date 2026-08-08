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

		// Deliberately unclassified.
		{name: "PR create keeps the fallback", method: "POST", path: "/repos/acme/widgets/pulls", wantErr: true},
		{name: "review post keeps the fallback", method: "POST", path: "/repos/acme/widgets/pulls/841/reviews", wantErr: true},
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
