package delegate

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/commitsigner"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/gitproxy"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	_ "modernc.org/sqlite"
)

// newRecorderStores opens an in-memory SQLite, migrates it, and seeds one
// conversations row the artifacts FK (conversation_id) can point at, and the
// registry row of octo/repo, the repository every push here goes to —
// mirroring the pre-push hook's own test fixture so the two writers are
// exercised against the same store.
func newRecorderStores(t *testing.T) (db.Stores, string) {
	t.Helper()
	stores, _, conversationID := newRecorderStoresConn(t)
	return stores, conversationID
}

// newRecorderStoresConn is newRecorderStores with the connection, for a test
// that seeds rows no store writes.
func newRecorderStoresConn(t *testing.T) (db.Stores, *sql.DB, string) {
	t.Helper()
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := db.Migrate(conn, "sqlite3"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.SeedLocalTenantRows(context.Background(), conn); err != nil {
		t.Fatalf("seed local tenant: %v", err)
	}
	if err := db.SetLocalGitHubURLForTest(conn, "https://github.com"); err != nil {
		t.Fatalf("set github url: %v", err)
	}
	if _, err := conn.Exec(`INSERT INTO conversations (id, trigger_type, creator_user_id, origin, status) VALUES ('run-1', 'event', NULL, 'interactive', 'running')`); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	stores := sqlitestore.New(conn)
	registerOctoRepo(t, stores)
	return stores, conn, "run-1"
}

// registerOctoRepo gives the registry a row for octo/repo on the org's GitHub
// host, carrying GitHub's id for it, as the repository of a run's checkout has
// one: the branch sink keys a branch on the row. Returns the row's id, the
// same one on every call.
func registerOctoRepo(t *testing.T, stores db.Stores) string {
	t.Helper()
	ctx := context.Background()
	ref := domain.RepoRef{Source: domain.RepoSourceGitHub, Host: "https://github.com", Owner: "octo", Repo: "repo", ExternalID: "1"}
	row, err := stores.Repos.FindOrCreateByRefSystem(ctx, runmode.LocalDefaultOrgID, ref)
	if err != nil {
		t.Fatalf("register octo/repo: %v", err)
	}
	if _, err := stores.Repos.FillMissingExternalIDsSystem(ctx, runmode.LocalDefaultOrgID, []domain.RepoRef{ref}); err != nil {
		t.Fatalf("record octo/repo's id: %v", err)
	}
	return row.ID
}

// recorderInfo is the run identity a delegated run carries — event-triggered,
// so UpsertArtifact takes the admin-pool branch (the common case for the proxy:
// an auto-delegated run has no user identity).
func recorderInfo(conversationID string) agenthost.ConversationInfo {
	return agenthost.ConversationInfo{
		OrgID:            runmode.LocalDefaultOrgID,
		UserID:           runmode.LocalDefaultUserID,
		TeamID:           runmode.LocalDefaultTeamID,
		ConversationID:   conversationID,
		IsEventTriggered: true,
	}
}

func TestGitPushRecorder_RecordsBranch(t *testing.T) {
	stores, conversationID := newRecorderStores(t)
	info := recorderInfo(conversationID)
	rec := gitPushRecorder(agenthost.NewLocal(stores, info), info)

	rec(context.Background(), gitproxy.PushedRef{
		Repo:    "octo/repo",
		Ref:     "refs/heads/feature/x",
		NewSHA:  "abc123",
		Created: true,
		Status:  200,
	})

	arts, err := stores.Artifacts.ListByConversation(context.Background(), runmode.LocalDefaultOrgID, conversationID)
	if err != nil {
		t.Fatalf("ListByConversation: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("got %d artifacts, want 1", len(arts))
	}
	a := arts[0]
	if a.Provider != domain.ArtifactProviderGit || a.Kind != domain.ArtifactKindBranch {
		t.Errorf("provider/kind = %q/%q, want git/branch", a.Provider, a.Kind)
	}
	if a.Target != "octo/repo" || a.ExternalID != "refs/heads/feature/x" {
		t.Errorf("target/external_id = %q/%q, want octo/repo, refs/heads/feature/x", a.Target, a.ExternalID)
	}
	if want := domain.BranchDedupKey(registerOctoRepo(t, stores), "refs/heads/feature/x"); a.DedupKey != want {
		t.Errorf("dedup_key = %q, want %q", a.DedupKey, want)
	}
	if a.ConversationID != conversationID || a.OrgID != runmode.LocalDefaultOrgID || a.TeamID != runmode.LocalDefaultTeamID {
		t.Errorf("identity not stamped: run=%q org=%q team=%q", a.ConversationID, a.OrgID, a.TeamID)
	}
	var d struct {
		SHA string `json:"sha"`
		New bool   `json:"new"`
	}
	if err := json.Unmarshal([]byte(a.DetailsJSON), &d); err != nil {
		t.Fatalf("details_json %q: %v", a.DetailsJSON, err)
	}
	if d.SHA != "abc123" || !d.New {
		t.Errorf("details = %+v, want sha=abc123 new=true", d)
	}
}

func TestGitPushRecorder_SkipsNonBranchAndMalformed(t *testing.T) {
	stores, conversationID := newRecorderStores(t)
	info := recorderInfo(conversationID)
	rec := gitPushRecorder(agenthost.NewLocal(stores, info), info)

	// A tag isn't a branch; a 3-segment repo path isn't owner/repo. Both make
	// NewBranchArtifact return ok=false, so the recorder skips them silently.
	rec(context.Background(), gitproxy.PushedRef{Repo: "octo/repo", Ref: "refs/tags/v1", NewSHA: "abc", Created: false, Status: 200})
	rec(context.Background(), gitproxy.PushedRef{Repo: "octo/repo/extra", Ref: "refs/heads/main", NewSHA: "abc", Created: false, Status: 200})

	arts, err := stores.Artifacts.ListByConversation(context.Background(), runmode.LocalDefaultOrgID, conversationID)
	if err != nil {
		t.Fatalf("ListByConversation: %v", err)
	}
	if len(arts) != 0 {
		t.Fatalf("got %d artifacts, want 0 (tag + malformed repo are skipped)", len(arts))
	}
}

// TestGitPushRecorder_RefusedPushRecordsAuditOnly pins the outcome split: a
// push the upstream refused (non-2xx) must NOT mint a branch artifact —
// artifacts track only work that landed — but must leave a branch_push_failed
// external-action audit row carrying the attempt's sha and HTTP status, so the
// audit log never omits an attempted external write. A subsequent successful
// push of the same ref then lands the artifact and the branch_pushed row as
// usual (the failure row, keyed for that one act, can't swallow it).
func TestGitPushRecorder_RefusedPushRecordsAuditOnly(t *testing.T) {
	stores, conversationID := newRecorderStores(t)
	info := recorderInfo(conversationID)
	rec := gitPushRecorder(agenthost.NewLocal(stores, info), info)
	ctx := context.Background()

	rec(ctx, gitproxy.PushedRef{Repo: "octo/repo", Ref: "refs/heads/feature/x", NewSHA: "abc123", Created: true, Status: 403})

	arts, err := stores.Artifacts.ListByConversation(ctx, runmode.LocalDefaultOrgID, conversationID)
	if err != nil {
		t.Fatalf("ListByConversation: %v", err)
	}
	if len(arts) != 0 {
		t.Fatalf("got %d artifacts for a refused push, want 0", len(arts))
	}
	acts, _, err := stores.ExternalActions.ListByOrgSystem(ctx, runmode.LocalDefaultOrgID, domain.ExternalActionListOpts{Action: domain.ActionBranchPushFailed})
	if err != nil {
		t.Fatalf("ListByOrgSystem: %v", err)
	}
	if len(acts) != 1 {
		t.Fatalf("got %d branch_push_failed actions, want 1", len(acts))
	}
	a := acts[0]
	if a.Target != "octo/repo" || a.ExternalID != "refs/heads/feature/x" || a.ConversationID != conversationID {
		t.Errorf("failure row = target %q external_id %q run %q; want octo/repo, refs/heads/feature/x, %s", a.Target, a.ExternalID, a.ConversationID, conversationID)
	}
	var d struct {
		SHA        string `json:"sha"`
		New        bool   `json:"new"`
		HTTPStatus int    `json:"http_status"`
	}
	if err := json.Unmarshal([]byte(a.DetailJSON), &d); err != nil {
		t.Fatalf("detail_json %q: %v", a.DetailJSON, err)
	}
	if d.SHA != "abc123" || !d.New || d.HTTPStatus != 403 {
		t.Errorf("detail = %+v, want sha=abc123 new=true http_status=403", d)
	}

	// The retry that lands: same (run, ref, sha), now a 2xx — the artifact and
	// the success audit row both appear despite the earlier failure.
	rec(ctx, gitproxy.PushedRef{Repo: "octo/repo", Ref: "refs/heads/feature/x", NewSHA: "abc123", Created: true, Status: 200})
	arts, err = stores.Artifacts.ListByConversation(ctx, runmode.LocalDefaultOrgID, conversationID)
	if err != nil {
		t.Fatalf("ListByConversation after retry: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("got %d artifacts after the successful retry, want 1", len(arts))
	}
	pushed, _, err := stores.ExternalActions.ListByOrgSystem(ctx, runmode.LocalDefaultOrgID, domain.ExternalActionListOpts{Action: domain.ActionBranchPushed})
	if err != nil {
		t.Fatalf("ListByOrgSystem (branch_pushed): %v", err)
	}
	if len(pushed) != 1 {
		t.Fatalf("got %d branch_pushed actions after the retry, want 1 (the failure row must not swallow it)", len(pushed))
	}
}

// TestGitPushRecorder_DedupConvergesWithHook proves the proxy backstop and the
// pre-push hook converge on one row: recording the same branch twice (as the
// hook then the proxy would for a single push) upserts in place — updating the
// volatile head — rather than minting a second artifact.
func TestGitPushRecorder_DedupConvergesWithHook(t *testing.T) {
	stores, conversationID := newRecorderStores(t)
	info := recorderInfo(conversationID)
	rec := gitPushRecorder(agenthost.NewLocal(stores, info), info)

	rec(context.Background(), gitproxy.PushedRef{Repo: "octo/repo", Ref: "refs/heads/main", NewSHA: "aaa", Created: true, Status: 200})
	rec(context.Background(), gitproxy.PushedRef{Repo: "octo/repo", Ref: "refs/heads/main", NewSHA: "bbb", Created: false, Status: 200})

	arts, err := stores.Artifacts.ListByConversation(context.Background(), runmode.LocalDefaultOrgID, conversationID)
	if err != nil {
		t.Fatalf("ListByConversation: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("got %d artifacts, want 1 (same dedup_key must upsert)", len(arts))
	}
	var d struct {
		SHA string `json:"sha"`
	}
	_ = json.Unmarshal([]byte(arts[0].DetailsJSON), &d)
	if d.SHA != "bbb" {
		t.Errorf("details sha after re-record = %q, want bbb", d.SHA)
	}
}

// TestGitPushRecorder_RejectedRefInsideA200 is the refusal GitHub reports in
// the body of a successful POST: the ref's report-status line is "ng". The
// HTTP status says 200, and the recorder must still leave no artifact and a
// branch_push_failed row carrying GitHub's reason and message.
func TestGitPushRecorder_RejectedRefInsideA200(t *testing.T) {
	stores, conversationID := newRecorderStores(t)
	info := recorderInfo(conversationID)
	rec := gitPushRecorder(agenthost.NewLocal(stores, info), info)
	ctx := context.Background()

	rec(ctx, gitproxy.PushedRef{
		Repo:          "octo/repo",
		Ref:           "refs/heads/feature/x",
		NewSHA:        "abc123",
		Created:       true,
		Status:        200,
		Rejected:      true,
		Reason:        "push declined due to repository rule violations",
		RemoteMessage: "error: GH013: Repository rule violations found for refs/heads/feature/x.",
	})

	arts, err := stores.Artifacts.ListByConversation(ctx, runmode.LocalDefaultOrgID, conversationID)
	if err != nil {
		t.Fatalf("ListByConversation: %v", err)
	}
	if len(arts) != 0 {
		t.Fatalf("got %d artifacts for a rejected ref, want 0", len(arts))
	}
	pushed, _, err := stores.ExternalActions.ListByOrgSystem(ctx, runmode.LocalDefaultOrgID, domain.ExternalActionListOpts{Action: domain.ActionBranchPushed})
	if err != nil {
		t.Fatalf("ListByOrgSystem (branch_pushed): %v", err)
	}
	if len(pushed) != 0 {
		t.Fatalf("got %d branch_pushed actions for a rejected ref, want 0", len(pushed))
	}
	acts, _, err := stores.ExternalActions.ListByOrgSystem(ctx, runmode.LocalDefaultOrgID, domain.ExternalActionListOpts{Action: domain.ActionBranchPushFailed})
	if err != nil {
		t.Fatalf("ListByOrgSystem: %v", err)
	}
	if len(acts) != 1 {
		t.Fatalf("got %d branch_push_failed actions, want 1", len(acts))
	}
	var d map[string]any
	if err := json.Unmarshal([]byte(acts[0].DetailJSON), &d); err != nil {
		t.Fatalf("detail_json %q: %v", acts[0].DetailJSON, err)
	}
	want := map[string]any{
		"sha":            "abc123",
		"new":            true,
		"http_status":    float64(200),
		"reason":         "push declined due to repository rule violations",
		"remote_message": "error: GH013: Repository rule violations found for refs/heads/feature/x.",
		// Beside a ruleset's refusal, an unsigned tip names the cause.
		"signer": "unsigned",
	}
	if !reflect.DeepEqual(d, want) {
		t.Errorf("detail = %v, want %v", d, want)
	}
}

// TestGitPushRecorder_FailureDetailOmitsEmpty pins that a refusal with no
// report (a bare 403) leaves reason and remote_message out of the detail
// rather than recording them empty.
func TestGitPushRecorder_FailureDetailOmitsEmpty(t *testing.T) {
	stores, conversationID := newRecorderStores(t)
	info := recorderInfo(conversationID)
	rec := gitPushRecorder(agenthost.NewLocal(stores, info), info)
	ctx := context.Background()

	rec(ctx, gitproxy.PushedRef{Repo: "octo/repo", Ref: "refs/heads/feature/x", NewSHA: "abc123", Status: 403})

	acts, _, err := stores.ExternalActions.ListByOrgSystem(ctx, runmode.LocalDefaultOrgID, domain.ExternalActionListOpts{Action: domain.ActionBranchPushFailed})
	if err != nil {
		t.Fatalf("ListByOrgSystem: %v", err)
	}
	if len(acts) != 1 {
		t.Fatalf("got %d branch_push_failed actions, want 1", len(acts))
	}
	if want := `{"sha":"abc123","new":false,"http_status":403,"signer":"unsigned"}`; acts[0].DetailJSON != want {
		t.Errorf("detail_json = %s, want %s", acts[0].DetailJSON, want)
	}
}

// A push's audit row names the account that signed its tip, from the
// signature whose commit id is the tip, and "unsigned" when none is; landed
// and refused alike. A push from an org whose signer signs is counted by
// whether its tip was signed.
func TestGitPushRecorder_NamesTheTipsSigner(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	stores, conn, conversationID := newRecorderStoresConn(t)
	ctx := context.Background()
	const signedTip, unsignedTip = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"

	if _, err := stores.OrgCommitSigners.Create(ctx, domain.OrgCommitSigner{
		OrgID: runmode.LocalDefaultOrgID, Host: "https://github.com", GitHubUserID: 583231,
		Login: "octocat", PublicKey: "ssh-ed25519 AAAAkey",
	}); err != nil {
		t.Fatalf("Create signer: %v", err)
	}
	if _, err := stores.OrgCommitSigners.Activate(ctx, runmode.LocalDefaultOrgID, "ssh-ed25519 AAAAkey"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	claimID := dbtest.SeedActiveClaim(t, conn, conversationID, "executor-1", 1)
	at := time.Now().UTC()
	if _, err := stores.CommitSignatures.InsertSystem(ctx, domain.CommitSignature{
		OrgID: runmode.LocalDefaultOrgID, ConversationID: conversationID, ClaimID: claimID, BootEpoch: 1,
		Signer: domain.CommitSigningOrg, SignerHost: "https://github.com", SignerUserID: 583231,
		PublicKey: "ssh-ed25519 AAAAkey", Digest: "ab", State: domain.CommitSignatureSigned,
		Signature: "sig", SignedAt: &at, CommitSHA: signedTip,
	}); err != nil {
		t.Fatalf("InsertSystem: %v", err)
	}

	reader := sdkmetric.NewManualReader()
	commitsigner.SetMeterProviderForTest(t, sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))

	info := recorderInfo(conversationID)
	rec := gitPushRecorder(agenthost.NewLocal(stores, info), info)
	rec(ctx, gitproxy.PushedRef{Repo: "octo/repo", Ref: "refs/heads/signed", NewSHA: signedTip, Created: true, Status: 200})
	rec(ctx, gitproxy.PushedRef{Repo: "octo/repo", Ref: "refs/heads/unsigned", NewSHA: unsignedTip, Created: true, Status: 200})
	rec(ctx, gitproxy.PushedRef{Repo: "octo/repo", Ref: "refs/heads/refused", NewSHA: unsignedTip, Created: true, Status: 403})

	signers := map[string]string{}
	for _, action := range []string{domain.ActionBranchPushed, domain.ActionBranchPushFailed} {
		acts, _, err := stores.ExternalActions.ListByOrgSystem(ctx, runmode.LocalDefaultOrgID, domain.ExternalActionListOpts{Action: action})
		if err != nil {
			t.Fatalf("ListByOrgSystem: %v", err)
		}
		for _, a := range acts {
			var d struct {
				SHA    string `json:"sha"`
				Signer string `json:"signer"`
			}
			if err := json.Unmarshal([]byte(a.DetailJSON), &d); err != nil {
				t.Fatalf("detail_json %q: %v", a.DetailJSON, err)
			}
			if d.SHA == "" {
				t.Errorf("%s detail lost its sha: %s", action, a.DetailJSON)
			}
			signers[action+" "+a.ExternalID] = d.Signer
		}
	}
	want := map[string]string{
		"branch_pushed refs/heads/signed":       "octocat",
		"branch_pushed refs/heads/unsigned":     "unsigned",
		"branch_push_failed refs/heads/refused": "unsigned",
	}
	if !reflect.DeepEqual(signers, want) {
		t.Errorf("signers = %v, want %v", signers, want)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	counted := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "commit_signing.pushes" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				signed, _ := dp.Attributes.Value("signed")
				counted[signed.AsString()] += dp.Value
			}
		}
	}
	if counted["true"] != 1 || counted["false"] != 2 {
		t.Errorf("tf_commit_signing_pushes_total = %v, want 1 signed and 2 unsigned", counted)
	}
}
