package domain

import (
	"reflect"
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
		"slack":  "",
	}
	for source, want := range cases {
		if got := EntityScope(source, s); got != want {
			t.Errorf("EntityScope(%s) = %q, want %q", source, got, want)
		}
	}
	empty := OrgSettings{}
	if got := EntityScope("github", empty); got != "https://github.com" {
		t.Errorf("EntityScope(github, unset) = %q, want the deployment default", got)
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
