package agenthost

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/convident"
	"github.com/sky-ai-eng/triage-factory/internal/commitsigner"
	"github.com/sky-ai-eng/triage-factory/internal/commitsigner/gitsig"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// keySecrets is the secret store's system read over one org's signing key.
type keySecrets struct {
	db.SecretStore
	key string
}

func (k keySecrets) GetSystem(_ context.Context, _, name string) (string, error) {
	if name == integrations.KeyGitHubCommitSigningKey {
		return k.key, nil
	}
	return "", nil
}

// With the local sandbox off, the exec CLI serves a run's verbs in-process,
// and the spawner hands it what it signs with through the environment.
func TestNewLocalFromEnv_SignsWithTheHandedOffSigning(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	stores, conn := newTestDB(t)
	key, err := gitsig.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	public, _ := gitsig.PublicKey(key)
	private, _ := gitsig.MarshalPrivateKey(key)
	stores.Secrets = keySecrets{key: string(private)}
	ctx := context.Background()
	if _, err := stores.OrgCommitSigners.Create(ctx, domain.OrgCommitSigner{
		OrgID: runmode.LocalDefaultOrgID, Host: "https://github.com", GitHubUserID: 583231, Login: "octocat", PublicKey: public,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := stores.OrgCommitSigners.Activate(ctx, runmode.LocalDefaultOrgID, public); err != nil {
		t.Fatal(err)
	}
	conversationID := uuid.New().String()
	dbtest.SeedConversation(t, conn, domain.Conversation{ID: conversationID})
	claimID := dbtest.SeedActiveClaim(t, conn, conversationID, "executor-1", 1)
	signer, err := commitsigner.Resolver{Signers: stores.OrgCommitSigners}.Resolve(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\n" +
		"author octocat <583231+octocat@users.noreply.github.com> 1700000000 +0000\n" +
		"committer octocat <583231+octocat@users.noreply.github.com> 1700000000 +0000\n\nsubject\n")
	t.Setenv(convident.ConversationIDEnvVar, conversationID)

	client := func(t *testing.T) Client {
		t.Helper()
		c, err := NewLocalFromEnv(ctx, stores)
		if err != nil {
			t.Fatalf("NewLocalFromEnv: %v", err)
		}
		return c
	}

	t.Run("handed off", func(t *testing.T) {
		entry, err := LocalCommitSigning{
			ClaimID: claimID, BootEpoch: 1, Signer: signer,
			Budgets: commitsigner.Budgets{PerOrgMinute: 10, PerClaim: 10},
		}.EnvEntry()
		if err != nil {
			t.Fatal(err)
		}
		name, value, _ := strings.Cut(entry, "=")
		t.Setenv(name, value)
		res, err := client(t).SignCommit(ctx, payload)
		if err != nil || res.Refusal != "" {
			t.Fatalf("SignCommit = %+v, %v; want a signature", res, err)
		}
		if want, _ := gitsig.Sign(key, payload); res.Signature != string(want) {
			t.Errorf("signature is not the org key's")
		}
	})
	t.Run("not handed off", func(t *testing.T) {
		t.Setenv(CommitSigningEnvVar, "")
		res, err := client(t).SignCommit(ctx, payload)
		if err != nil || res.Signature != "" || !strings.Contains(res.Refusal, "this run does not sign commits") {
			t.Fatalf("SignCommit = %+v, %v; want the not-signing line", res, err)
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		t.Setenv(CommitSigningEnvVar, "{not json")
		if res, err := client(t).SignCommit(ctx, payload); err == nil {
			t.Fatalf("SignCommit = %+v with an unreadable hand-off; want an error, which the verb answers as unanswered", res)
		}
	})
}
