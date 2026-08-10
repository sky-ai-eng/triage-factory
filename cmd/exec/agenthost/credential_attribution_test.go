package agenthost

import (
	"context"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/ghwrite"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// These attribution tests pin what the audit log says about WHICH credential
// made a write. Every GitHub row used to claim the org App, so a PAT
// org's log of record named a service account that does not exist there. These
// prove the two tiers now land distinctly, on every path a run can write
// through: the exec verbs, the branch push, and the real-`gh` channel.

// TestExecVerb_RecordsTheTierTheResolverPicked: a comment posted through the
// verb path is stamped with the credential the resolver actually selected for
// the repo, not a fixed one. Both tiers, same call, same fake backend — the only
// difference is what the resolver reports.
func TestExecVerb_RecordsTheTierTheResolverPicked(t *testing.T) {
	for _, tc := range []struct {
		name     string
		identity ghclient.Identity
		want     string
	}{
		{"app", ghclient.IdentityApp, domain.CredentialGitHubApp},
		{"pat", ghclient.IdentityPAT, domain.CredentialGitHubPAT},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh := startFakeGitHubComments(t)
			stores, _, client := newGithubRecordingClient(t, gh.URL, true)
			client.SetGitHubResolver(fakeGitHubResolver{baseURL: gh.URL, token: "tok", identity: tc.identity})

			if _, err := client.GithubAddComment(context.Background(), "octo", "repo", 1, "hi"); err != nil {
				t.Fatalf("GithubAddComment: %v", err)
			}

			acts := listExternalActions(t, stores)
			if len(acts) != 1 {
				t.Fatalf("want 1 action, got %d: %+v", len(acts), acts)
			}
			if acts[0].Action != domain.ActionCommentPosted || acts[0].Credential != tc.want {
				t.Errorf("action = %+v, want comment_posted under %q", acts[0], tc.want)
			}
		})
	}
}

// TestUnclassifiedResolverKeepsTheApp: a resolver that reports no tier leaves
// the row reading as it always has. The fallback is the point — an unknown tier
// must not be published as a PAT, which would invent a finding out of a resolver
// that simply doesn't classify.
func TestUnclassifiedResolverKeepsTheApp(t *testing.T) {
	gh := startFakeGitHubComments(t)
	stores, _, client := newGithubRecordingClient(t, gh.URL, true)
	client.SetGitHubResolver(baseOnlyResolver{baseURL: gh.URL, token: "tok"})

	if _, err := client.GithubAddComment(context.Background(), "octo", "repo", 1, "hi"); err != nil {
		t.Fatalf("GithubAddComment: %v", err)
	}
	acts := listExternalActions(t, stores)
	if len(acts) != 1 || acts[0].Credential != domain.CredentialGitHubApp {
		t.Errorf("actions = %+v, want one row still attributed to the app", acts)
	}
}

// TestBranchPush_ResolvesItsOwnTier covers the one recorder that runs in a
// process which never resolved a gh client: the local-mode pre-push hook builds
// a branch artifact and nothing has classified the credential by then. The row
// still names the tier, resolved from the same resolver that minted the token
// the push authenticated with.
func TestBranchPush_ResolvesItsOwnTier(t *testing.T) {
	gh := startFakeGitHubComments(t)
	stores, _, client := newGithubRecordingClient(t, gh.URL, true)
	client.SetGitHubResolver(fakeGitHubResolver{baseURL: gh.URL, token: "tok", identity: ghclient.IdentityPAT})

	branch, ok := domain.NewBranchArtifact("octo/repo", "refs/heads/feat", "abc123", true)
	if !ok {
		t.Fatal("NewBranchArtifact returned ok=false")
	}
	if _, err := client.UpsertArtifact(context.Background(), branch); err != nil {
		t.Fatalf("UpsertArtifact: %v", err)
	}

	acts := listExternalActions(t, stores)
	if len(acts) != 1 {
		t.Fatalf("want 1 action, got %d: %+v", len(acts), acts)
	}
	if acts[0].Action != domain.ActionBranchPushed || acts[0].Credential != domain.CredentialGitHubPAT {
		t.Errorf("branch action = %+v, want branch_pushed under github_pat", acts[0])
	}
}

// TestSetGitHubCredential_ReachesTheBranchRow: on an executor nothing here can
// classify the tier — the secret store is disabled and the sealed bundle opens
// only in the sidecar — so the sidecar's report is set on the client and must
// reach the row the runtime beneath it builds, without any resolver being asked.
func TestSetGitHubCredential_ReachesTheBranchRow(t *testing.T) {
	stores, info := newCaptureStores(t, true)
	client := NewLocal(stores, info)
	client.SetGitHubCredential(domain.CredentialGitHubPAT)

	branch, ok := domain.NewBranchArtifact("octo/repo", "refs/heads/feat", "abc123", true)
	if !ok {
		t.Fatal("NewBranchArtifact returned ok=false")
	}
	if _, err := client.UpsertArtifact(context.Background(), branch); err != nil {
		t.Fatalf("UpsertArtifact: %v", err)
	}

	acts := listExternalActions(t, stores)
	if len(acts) != 1 || acts[0].Credential != domain.CredentialGitHubPAT {
		t.Errorf("actions = %+v, want the branch row under the reported credential", acts)
	}
}

// TestGHChannelRows_CarryTheInjectedCredential: on the gh channel the agent's
// own token is a placeholder, so the tier can only come from the side that
// swapped it in. Both the write row and the refusal row take it from there.
func TestGHChannelRows_CarryTheInjectedCredential(t *testing.T) {
	write := GHChannelWriteAction(ghwrite.Observation{
		Method: "PATCH", Path: "/repos/octo/repo/pulls/1", Status: 200,
	}, domain.CredentialGitHubPAT)
	if write.Credential != domain.CredentialGitHubPAT {
		t.Errorf("gh-channel write credential = %q, want github_pat", write.Credential)
	}

	fallback := GHChannelWriteAction(ghwrite.Observation{
		Method: "POST", Path: "/user/repos", Status: 201,
	}, domain.CredentialGitHubPAT)
	if fallback.Credential != domain.CredentialGitHubPAT {
		t.Errorf("unclassified write credential = %q, want github_pat", fallback.Credential)
	}

	graphql := GHChannelWriteAction(ghwrite.Observation{
		Method: "POST", Path: "/graphql", Status: 200,
		GraphQL: &ghwrite.GraphQLFacts{Operation: "Thing", Fields: []string{"doThing"}},
	}, domain.CredentialGitHubPAT)
	if graphql.Credential != domain.CredentialGitHubPAT {
		t.Errorf("graphql fallback credential = %q, want github_pat", graphql.Credential)
	}

	req := ghwrite.Request{Method: "POST", Path: "/user/repos"}
	ref, gated := ghwrite.Gate(req)
	if !gated {
		t.Fatal("repo create is expected to be a gated shape")
	}
	denied := GHWriteDeniedAction(req, ref, domain.CredentialGitHubPAT)
	if denied.Credential != domain.CredentialGitHubPAT {
		t.Errorf("refusal credential = %q, want github_pat", denied.Credential)
	}
}

// TestProxyRepoClient_ReportsTheSidecarsTier pins the executor's side of the
// same fact: the client is built against a proxy holding a placeholder, so the
// identity it reports has to be the one the sidecar sent up with it.
func TestProxyRepoClient_ReportsTheSidecarsTier(t *testing.T) {
	for _, tc := range []struct {
		credential string
		want       ghclient.Identity
	}{
		{domain.CredentialGitHubPAT, ghclient.IdentityPAT},
		{domain.CredentialGitHubApp, ghclient.IdentityApp},
		{"", ghclient.IdentityApp},
	} {
		_, identity, err := proxyRepoClient(&ProxyCredentials{
			GitHubAPIURL:     "http://127.0.0.1:1",
			GitHubCredential: tc.credential,
		}, "octo", "repo")
		if err != nil {
			t.Fatalf("proxyRepoClient(%q): %v", tc.credential, err)
		}
		if identity != tc.want {
			t.Errorf("proxyRepoClient(%q) identity = %v, want %v", tc.credential, identity, tc.want)
		}
	}
}

// TestNoteGitHubCredential_LeavesAReportedCredentialAlone: on the executor the
// credential comes from the only process that can read it, and the identity the
// proxy client reports is derived from that same value. Re-deriving it back
// would round-trip through a three-valued enum — lossless only while the column
// carries exactly the two tiers that enum models — so a tier the round-trip
// could invent must not displace what was reported.
func TestNoteGitHubCredential_LeavesAReportedCredentialAlone(t *testing.T) {
	c := &LocalClient{proxyCreds: &ProxyCredentials{GitHubCredential: domain.CredentialGitHubPAT}}
	c.SetGitHubCredential(c.proxyCreds.GitHubCredential)

	c.noteGitHubCredential(ghclient.IdentityApp)

	if got := c.githubCredential(); got != domain.CredentialGitHubPAT {
		t.Errorf("credential = %q, want the reported credential to stand", got)
	}
}

// TestRelayServer_CredentialReachesBothRecorders covers the executor's assembly
// point, the one link the per-piece tests skip. A relay server builds audit rows
// through two halves — its runtime, for the branch push composed into an
// artifact upsert, and its audit client, for the relayed gh-channel writes and
// git denials — so a credential that reaches only one of them leaves half the
// run's log still claiming the App.
func TestRelayServer_CredentialReachesBothRecorders(t *testing.T) {
	stores, info := newCaptureStores(t, true)
	srv := NewRelayServer(stores, info, nil)
	srv.SetGitHubCredential(domain.CredentialGitHubPAT)
	ctx := context.Background()

	branch, ok := domain.NewBranchArtifact("octo/repo", "refs/heads/feat", "abc123", true)
	if !ok {
		t.Fatal("NewBranchArtifact returned ok=false")
	}
	if _, err := srv.rt.UpsertArtifact(ctx, branch); err != nil {
		t.Fatalf("runtime UpsertArtifact: %v", err)
	}
	srv.audit.RecordGHChannelWrite(ctx, ghwrite.Observation{
		Method: "PATCH", Path: "/repos/octo/repo/pulls/1", Status: 200,
	})

	acts := listExternalActions(t, stores)
	if len(acts) != 2 {
		t.Fatalf("want a row from each half, got %d: %+v", len(acts), acts)
	}
	for _, a := range acts {
		if a.Credential != domain.CredentialGitHubPAT {
			t.Errorf("%s row = %+v, want github_pat", a.Action, a)
		}
	}
}
