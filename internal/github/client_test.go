package github

import "testing"

func TestGraphQLURL(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    string
	}{
		{
			name:    "github.com",
			baseURL: "https://api.github.com",
			want:    "https://api.github.com/graphql",
		},
		{
			name:    "GHES",
			baseURL: "https://git.corp.example.com/api/v3",
			want:    "https://git.corp.example.com/api/graphql",
		},
		{
			name:    "GHES trailing slash stripped",
			baseURL: "https://github.example.com/api/v3",
			want:    "https://github.example.com/api/graphql",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := graphqlURL(tt.baseURL)
			if got != tt.want {
				t.Errorf("graphqlURL(%q) = %q, want %q", tt.baseURL, got, tt.want)
			}
		})
	}
}
