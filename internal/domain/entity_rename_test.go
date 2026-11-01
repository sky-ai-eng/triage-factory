package domain

import (
	"reflect"
	"strings"
	"testing"
)

func TestDetectEntityRenames(t *testing.T) {
	stored := []EntityRef{
		{Source: "linear", Scope: "ws-a", SourceID: "ENG-4", ExternalID: "uuid-4"},
		{Source: "linear", Scope: "ws-a", SourceID: "ENG-5", ExternalID: "uuid-5"},
		{Source: "jira", Scope: "https://jira.example.com", SourceID: "SKY-1"},
	}

	tests := []struct {
		name     string
		observed []EntityRef
		want     []string // observed keys
	}{
		{
			name:     "same id under a different key is a rename",
			observed: []EntityRef{{Source: "linear", Scope: "ws-a", SourceID: "OPS-77", ExternalID: "uuid-4"}},
			want:     []string{"OPS-77"},
		},
		{
			name:     "the same key under a different id is not a rename",
			observed: []EntityRef{{Source: "linear", Scope: "ws-a", SourceID: "ENG-4", ExternalID: "uuid-99"}},
			want:     nil,
		},
		{
			name:     "the same id in another scope is another object",
			observed: []EntityRef{{Source: "linear", Scope: "ws-b", SourceID: "OPS-77", ExternalID: "uuid-4"}},
			want:     nil,
		},
		{
			name:     "the same id under another source is another object",
			observed: []EntityRef{{Source: "jira", Scope: "ws-a", SourceID: "OPS-77", ExternalID: "uuid-4"}},
			want:     nil,
		},
		{
			name:     "an unchanged key is not a rename",
			observed: []EntityRef{{Source: "linear", Scope: "ws-a", SourceID: "ENG-5", ExternalID: "uuid-5"}},
			want:     nil,
		},
		{
			name:     "an observation with no id is never a rename",
			observed: []EntityRef{{Source: "jira", Scope: "https://jira.example.com", SourceID: "SKY-2"}},
			want:     nil,
		},
		{
			name: "a team key rename renames every issue in the team",
			observed: []EntityRef{
				{Source: "linear", Scope: "ws-a", SourceID: "CORE-4", ExternalID: "uuid-4"},
				{Source: "linear", Scope: "ws-a", SourceID: "CORE-5", ExternalID: "uuid-5"},
			},
			want: []string{"CORE-4", "CORE-5"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, r := range DetectEntityRenames(stored, tc.observed) {
				got = append(got, r.SourceID)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("DetectEntityRenames = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEntityScope(t *testing.T) {
	s := OrgSettings{
		GitHubBaseURL:     "https://ghe.example.com/",
		JiraBaseURL:       "  https://jira.example.com/jira/ ",
		LinearWorkspaceID: "ws-a",
	}
	cases := map[string]string{
		"github": "https://ghe.example.com",
		"jira":   "https://jira.example.com/jira",
		"linear": "ws-a",
		"slack":  SlackScope,
	}
	for source, want := range cases {
		if got := EntityScope(source, s); got != want {
			t.Errorf("EntityScope(%s) = %q, want %q", source, got, want)
		}
	}
	empty := OrgSettings{}
	if got := EntityScope("github", empty); got != "" {
		t.Errorf("EntityScope(github, unset) = %q, want empty: an org with no GitHub URL is on no GitHub", got)
	}
	if got := EntityScope("jira", empty); got != "" {
		t.Errorf("EntityScope(jira, unset) = %q, want empty", got)
	}
	if got := EntityScope("jira", OrgSettings{JiraBaseURL: "jira.example.com"}); got != "" {
		t.Errorf("EntityScope(jira, no scheme) = %q, want empty", got)
	}
	if got := EntityScope("linear", empty); got != "" {
		t.Errorf("EntityScope(linear, unbound) = %q, want empty", got)
	}
}

func TestJiraIssueResource(t *testing.T) {
	for _, site := range []string{
		"https://acme.atlassian.net",
		"https://jira.example.com:8443/jira",
		"http://10.0.0.5/a%2Fb",
	} {
		resource := JiraIssueResource(site, "10042")
		if strings.ContainsAny(resource[:strings.LastIndex(resource, "/")], ":/") {
			t.Errorf("JiraIssueResource(%q) = %q: the escaped site holds a separator", site, resource)
		}
		key := ObjectKey(ProviderJira, ObjectKindComment, resource, "20001")
		gotSite, gotID, ok := ObjectKeyEntityIdentity(ProviderJira, key)
		if !ok || gotSite != site || gotID != "10042" {
			t.Errorf("ObjectKeyEntityIdentity(%q) = (%q, %q, %v), want (%q, 10042)", key, gotSite, gotID, ok, site)
		}
		if want := ObjectKey(ProviderJira, ObjectKindComment, EntityResource("jira", site, "10042"), "20001"); key != want {
			t.Errorf("%q is not keyed on the entity's resource (%q)", key, want)
		}
	}
	if r := JiraIssueResource("", "10042"); r != "" {
		t.Errorf("no site = %q, want empty", r)
	}
	if r := JiraIssueResource("https://acme.atlassian.net", ""); r != "" {
		t.Errorf("no id = %q, want empty", r)
	}
	// A key spelling the issue key names no identity.
	if _, _, ok := ObjectKeyEntityIdentity(ProviderJira, "jira:issue:SKY-1"); ok {
		t.Error("a key-shaped Jira object key read as an identity")
	}
	// A GitHub pull request deliverable's pull request is the node id its key
	// names, and a review's is its pull request's. A key that still spells the
	// repository's name names none, whatever the row's node_id column holds.
	pr := Deliverable{Provider: ProviderGitHub, Kind: DeliverableKindPullRequest, DedupKey: "github:pull_request:o/r#1", NodeID: "PR_kwDO1"}
	if _, ok := DeliverablePullRequestNodeID(pr); ok {
		t.Error("a GitHub pull request keyed on its repository's name read as an identity")
	}
	pr.DedupKey = PullRequestDedupKey("PR_kwDO1")
	if id, ok := DeliverablePullRequestNodeID(pr); !ok || id != "PR_kwDO1" {
		t.Errorf("github pull request identity = (%q, %v), want its node id", id, ok)
	}
	review := Deliverable{Provider: ProviderGitHub, Kind: DeliverableKindReview, DedupKey: ReviewDedupKey("PR_kwDO1", "conv-9"), NodeID: "PRR_kwDO9"}
	if id, ok := DeliverablePullRequestNodeID(review); !ok || id != "PR_kwDO1" {
		t.Errorf("github review identity = (%q, %v), want its pull request's node id", id, ok)
	}
	review.DedupKey = "github:review:o/r#1:conv-9"
	if _, ok := DeliverablePullRequestNodeID(review); ok {
		t.Error("a GitHub review keyed on its repository's name read as an identity")
	}
	if _, id, ok := ObjectKeyEntityIdentity(ProviderLinear, "linear:comment:uuid-1:c-1"); !ok || id != "uuid-1" {
		t.Errorf("linear identity = (%q, %v), want uuid-1", id, ok)
	}
	// Two sites' issue 10042 are two resources.
	if JiraIssueResource("https://a.example.com", "10042") == JiraIssueResource("https://b.example.com", "10042") {
		t.Error("one id on two sites collapsed to one resource")
	}
}

func TestEntityURL(t *testing.T) {
	if got := EntityURL("jira", "https://acme.atlassian.net", "SKY-1"); got != "https://acme.atlassian.net/browse/SKY-1" {
		t.Errorf("jira url = %q", got)
	}
	if got := EntityURL("github", "https://github.com", "o/r#1"); got != "" {
		t.Errorf("github url = %q, want empty: callers supply it", got)
	}
	if got := EntityURL("jira", "", "SKY-1"); got != "" {
		t.Errorf("jira url with no site = %q, want empty", got)
	}
}
