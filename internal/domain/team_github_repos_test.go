package domain

import "testing"

func TestNormalizeTeamGitHubRepos(t *testing.T) {
	t.Run("trims, dedups, drops blanks", func(t *testing.T) {
		got, err := NormalizeTeamGitHubRepos([]TeamGitHubRepo{
			{Owner: " acme ", Repo: " api "},
			{Owner: "acme", Repo: "api"}, // dup of the trimmed first
			{Owner: "", Repo: ""},        // blank → dropped
			{Owner: "acme", Repo: "web"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []TeamGitHubRepo{{Owner: "acme", Repo: "api"}, {Owner: "acme", Repo: "web"}}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("[%d] = %v, want %v", i, got[i], want[i])
			}
		}
	})

	t.Run("preserves case (repo slugs round-trip verbatim)", func(t *testing.T) {
		got, err := NormalizeTeamGitHubRepos([]TeamGitHubRepo{{Owner: "Acme", Repo: "API"}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 || got[0].Owner != "Acme" || got[0].Repo != "API" {
			t.Errorf("got %v, want [{Acme API}] (case preserved)", got)
		}
	})

	t.Run("half-specified entry is an error", func(t *testing.T) {
		if _, err := NormalizeTeamGitHubRepos([]TeamGitHubRepo{{Owner: "acme", Repo: ""}}); err == nil {
			t.Error("owner-without-repo should error")
		}
		if _, err := NormalizeTeamGitHubRepos([]TeamGitHubRepo{{Owner: "", Repo: "api"}}); err == nil {
			t.Error("repo-without-owner should error")
		}
	})
}

func TestTeamGitHubReposFromSlugs(t *testing.T) {
	t.Run("splits owner/repo and normalizes", func(t *testing.T) {
		got, err := TeamGitHubReposFromSlugs([]string{"acme/api", " acme/web ", "acme/api"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 2 || got[0].Slug() != "acme/api" || got[1].Slug() != "acme/web" {
			t.Errorf("got %v, want [acme/api acme/web]", got)
		}
	})

	t.Run("slug without slash is rejected", func(t *testing.T) {
		if _, err := TeamGitHubReposFromSlugs([]string{"no-slash"}); err == nil {
			t.Error("a slug with no slash should error (half-specified)")
		}
	})

	t.Run("empty input yields empty set", func(t *testing.T) {
		got, err := TeamGitHubReposFromSlugs(nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("got %v, want empty", got)
		}
	})
}
