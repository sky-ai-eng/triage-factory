package github

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
)

type recordingManagedGitResolver struct {
	permissions map[string]string
}

func (r *recordingManagedGitResolver) TokenForRepoScoped(_ context.Context, _, _, _ string, permissions map[string]string) (githubapp.Token, error) {
	r.permissions = permissions
	return githubapp.Token{Value: "installation-grant"}, nil
}

func (*recordingManagedGitResolver) TokenForReposScoped(context.Context, string, string, []string, map[string]string) (githubapp.Token, error) {
	return githubapp.Token{}, nil
}

func (*recordingManagedGitResolver) HasAnyCredential(context.Context, string) (bool, error) {
	return true, nil
}

func TestTokenForManagedGit_InheritsInstallationPermissions(t *testing.T) {
	resolver := &recordingManagedGitResolver{}
	tok, err := TokenForManagedGit(context.Background(), resolver, "org-1", "acme", "widgets")
	if err != nil {
		t.Fatalf("TokenForManagedGit: %v", err)
	}
	if tok.Value != "installation-grant" {
		t.Errorf("token = %q, want installation-grant", tok.Value)
	}
	if resolver.permissions != nil {
		t.Errorf("permissions = %v, want nil so the App token inherits its installation grant", resolver.permissions)
	}
}
