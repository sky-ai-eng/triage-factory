package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// ---------- fakes ----------
//
// Each fake embeds the full core interface (nil) so it satisfies the type
// without implementing every method — only the methods resolveSender
// actually calls are overridden. Mirrors internal/jira/resolver_test.go's
// fakeSecrets pattern.

type fakeSecrets struct {
	db.SecretStore
	token string
	err   error
	calls int
}

func (f *fakeSecrets) GetSystem(context.Context, string, string) (string, error) {
	f.calls++
	return f.token, f.err
}

type fakeUsers struct {
	db.UsersStore
	ids   []string
	err   error
	calls int
}

func (f *fakeUsers) UserIDsForVerifiedEmailSystem(context.Context, string) ([]string, error) {
	f.calls++
	return f.ids, f.err
}

type fakeIdentityStore struct {
	mu   sync.Mutex
	rows map[string]*slackstore.SlackIdentity

	lookupErr error
	markErr   error
	upsertErr error

	markAttemptCalls int
	upsertCalls      int
}

func newFakeIdentityStore() *fakeIdentityStore {
	return &fakeIdentityStore{rows: map[string]*slackstore.SlackIdentity{}}
}

func identityKey(ws, su string) string { return ws + "/" + su }

func (f *fakeIdentityStore) LookupSystem(_ context.Context, ws, su string) (*slackstore.SlackIdentity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lookupErr != nil {
		return nil, f.lookupErr
	}
	row := f.rows[identityKey(ws, su)]
	if row == nil {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

func (f *fakeIdentityStore) UpsertResolvedSystem(_ context.Context, ws, su, userID, displayName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upsertCalls++
	if f.upsertErr != nil {
		return f.upsertErr
	}
	uid := userID
	f.rows[identityKey(ws, su)] = &slackstore.SlackIdentity{
		WorkspaceID: ws, SlackUserID: su, UserID: &uid, SlackDisplayName: displayName,
		Source: "auto_email", LastAttemptAt: time.Now(),
	}
	return nil
}

// MarkAttemptSystem mirrors the real store's ON CONFLICT ... WHERE user_id
// IS NULL guard: a resolved row is left completely untouched.
func (f *fakeIdentityStore) MarkAttemptSystem(_ context.Context, ws, su, displayName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markAttemptCalls++
	if f.markErr != nil {
		return f.markErr
	}
	if existing := f.rows[identityKey(ws, su)]; existing != nil && existing.UserID != nil {
		return nil
	}
	f.rows[identityKey(ws, su)] = &slackstore.SlackIdentity{
		WorkspaceID: ws, SlackUserID: su, UserID: nil, SlackDisplayName: displayName,
		Source: "auto_email", LastAttemptAt: time.Now(),
	}
	return nil
}

// ---------- test scaffolding ----------

func testWorkspaceForResolver() slackstore.Workspace {
	return slackstore.Workspace{WorkspaceID: "T0RESOLVE1", OrgID: "org-1", BotTokenRef: "slack_ws_T0RESOLVE1_bot_token"}
}

// newUsersInfoServer serves a fixed users.info JSON body and counts hits.
func newUsersInfoServer(t *testing.T, body map[string]any) (*http.Client, *int) {
	t.Helper()
	hits := 0
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		_ = json.NewEncoder(w).Encode(body)
	})
	return srv.Client(), &hits
}

func captureSlackLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := slackLog
	slackLog = slog.New(slog.NewTextHandler(&buf, nil))
	t.Cleanup(func() { slackLog = orig })
	return &buf
}

// ---------- resolveSender ----------

func TestResolveSender_SingleMatch_BindsWithDisplayName(t *testing.T) {
	client, hits := newUsersInfoServer(t, map[string]any{
		"ok": true,
		"user": map[string]any{
			"is_bot": false, "deleted": false,
			"profile": map[string]any{"email": "ada@example.com", "real_name": "Ada L.", "display_name": "ada"},
		},
	})
	identities := newFakeIdentityStore()
	users := &fakeUsers{ids: []string{"user-1"}}
	r := &IdentityResolver{secrets: &fakeSecrets{token: "xoxb-test"}, users: users, identities: identities, client: client}
	ws := testWorkspaceForResolver()

	r.resolveSender(context.Background(), ws, "U0SENDER01")

	if *hits != 1 {
		t.Fatalf("users.info hits = %d; want 1", *hits)
	}
	row := identities.rows[identityKey(ws.WorkspaceID, "U0SENDER01")]
	if row == nil || row.UserID == nil || *row.UserID != "user-1" {
		t.Fatalf("row = %+v; want resolved to user-1", row)
	}
	if row.SlackDisplayName != "ada" {
		t.Errorf("SlackDisplayName = %q; want display_name (\"ada\") preferred over real_name", row.SlackDisplayName)
	}
	if identities.markAttemptCalls != 0 {
		t.Errorf("markAttemptCalls = %d; want 0 (a single match never writes the negative cache)", identities.markAttemptCalls)
	}
}

func TestResolveSender_SingleMatch_FallsBackToRealName(t *testing.T) {
	client, _ := newUsersInfoServer(t, map[string]any{
		"ok": true,
		"user": map[string]any{
			"is_bot": false, "deleted": false,
			"profile": map[string]any{"email": "ada@example.com", "real_name": "Ada L.", "display_name": ""},
		},
	})
	identities := newFakeIdentityStore()
	r := &IdentityResolver{secrets: &fakeSecrets{token: "xoxb-test"}, users: &fakeUsers{ids: []string{"user-1"}}, identities: identities, client: client}
	ws := testWorkspaceForResolver()

	r.resolveSender(context.Background(), ws, "U0SENDER02")

	row := identities.rows[identityKey(ws.WorkspaceID, "U0SENDER02")]
	if row == nil || row.SlackDisplayName != "Ada L." {
		t.Fatalf("row = %+v; want SlackDisplayName falling back to real_name", row)
	}
}

func TestResolveSender_ZeroMatch_WritesNegativeCache(t *testing.T) {
	client, _ := newUsersInfoServer(t, map[string]any{
		"ok": true,
		"user": map[string]any{
			"is_bot": false, "deleted": false,
			"profile": map[string]any{"email": "nobody@example.com", "real_name": "Nobody"},
		},
	})
	identities := newFakeIdentityStore()
	r := &IdentityResolver{secrets: &fakeSecrets{token: "xoxb-test"}, users: &fakeUsers{ids: nil}, identities: identities, client: client}
	ws := testWorkspaceForResolver()

	r.resolveSender(context.Background(), ws, "U0SENDER03")

	row := identities.rows[identityKey(ws.WorkspaceID, "U0SENDER03")]
	if row == nil || row.UserID != nil {
		t.Fatalf("row = %+v; want an unresolved negative-cache row", row)
	}
	if identities.markAttemptCalls != 1 || identities.upsertCalls != 0 {
		t.Errorf("markAttemptCalls=%d upsertCalls=%d; want 1/0", identities.markAttemptCalls, identities.upsertCalls)
	}
}

func TestResolveSender_AmbiguousMatch_WritesNegativeCacheAndWarnsWithoutEmail(t *testing.T) {
	client, _ := newUsersInfoServer(t, map[string]any{
		"ok": true,
		"user": map[string]any{
			"is_bot": false, "deleted": false,
			"profile": map[string]any{"email": "shared@example.com", "real_name": "Shared"},
		},
	})
	identities := newFakeIdentityStore()
	r := &IdentityResolver{secrets: &fakeSecrets{token: "xoxb-test"}, users: &fakeUsers{ids: []string{"user-1", "user-2"}}, identities: identities, client: client}
	ws := testWorkspaceForResolver()
	logs := captureSlackLog(t)

	r.resolveSender(context.Background(), ws, "U0SENDER04")

	row := identities.rows[identityKey(ws.WorkspaceID, "U0SENDER04")]
	if row == nil || row.UserID != nil {
		t.Fatalf("row = %+v; want unresolved (never guess on ambiguity)", row)
	}
	logged := logs.String()
	if !bytes.Contains(logs.Bytes(), []byte(ws.WorkspaceID)) || !bytes.Contains(logs.Bytes(), []byte("U0SENDER04")) {
		t.Errorf("log output = %q; want it to name the workspace + slack user", logged)
	}
	if bytes.Contains(logs.Bytes(), []byte("shared@example.com")) {
		t.Errorf("log output = %q; must never name the ambiguous email", logged)
	}
}

func TestResolveSender_BotSender_WritesNegativeCacheWithoutUsersLookup(t *testing.T) {
	client, _ := newUsersInfoServer(t, map[string]any{
		"ok": true,
		"user": map[string]any{
			"is_bot": true, "deleted": false,
			"profile": map[string]any{"email": "bot@example.com", "real_name": "Some Bot"},
		},
	})
	identities := newFakeIdentityStore()
	users := &fakeUsers{ids: []string{"user-1"}}
	r := &IdentityResolver{secrets: &fakeSecrets{token: "xoxb-test"}, users: users, identities: identities, client: client}
	ws := testWorkspaceForResolver()

	r.resolveSender(context.Background(), ws, "U0BOTSEND1")

	row := identities.rows[identityKey(ws.WorkspaceID, "U0BOTSEND1")]
	if row == nil || row.UserID != nil {
		t.Fatalf("row = %+v; want an unresolved negative-cache row for a bot sender", row)
	}
	if users.calls != 0 {
		t.Errorf("verified-email lookup calls = %d; want 0 (bot short-circuits before the email lookup)", users.calls)
	}
}

func TestResolveSender_DeletedSender_WritesNegativeCache(t *testing.T) {
	client, _ := newUsersInfoServer(t, map[string]any{
		"ok": true,
		"user": map[string]any{
			"is_bot": false, "deleted": true,
			"profile": map[string]any{"email": "gone@example.com", "real_name": "Gone"},
		},
	})
	identities := newFakeIdentityStore()
	r := &IdentityResolver{secrets: &fakeSecrets{token: "xoxb-test"}, users: &fakeUsers{ids: []string{"user-1"}}, identities: identities, client: client}
	ws := testWorkspaceForResolver()

	r.resolveSender(context.Background(), ws, "U0DELETED1")

	row := identities.rows[identityKey(ws.WorkspaceID, "U0DELETED1")]
	if row == nil || row.UserID != nil {
		t.Fatalf("row = %+v; want an unresolved negative-cache row for a deleted sender", row)
	}
}

func TestResolveSender_EmptyEmail_WritesNegativeCache(t *testing.T) {
	client, _ := newUsersInfoServer(t, map[string]any{
		"ok": true,
		"user": map[string]any{
			"is_bot": false, "deleted": false,
			"profile": map[string]any{"email": "", "real_name": "No Email"},
		},
	})
	identities := newFakeIdentityStore()
	r := &IdentityResolver{secrets: &fakeSecrets{token: "xoxb-test"}, users: &fakeUsers{ids: []string{"user-1"}}, identities: identities, client: client}
	ws := testWorkspaceForResolver()

	r.resolveSender(context.Background(), ws, "U0NOEMAIL1")

	row := identities.rows[identityKey(ws.WorkspaceID, "U0NOEMAIL1")]
	if row == nil || row.UserID != nil {
		t.Fatalf("row = %+v; want an unresolved negative-cache row when profile.email is empty", row)
	}
}

func TestResolveSender_AlreadyResolved_SkipsEverything(t *testing.T) {
	client, hits := newUsersInfoServer(t, map[string]any{"ok": true, "user": map[string]any{"profile": map[string]any{}}})
	identities := newFakeIdentityStore()
	ws := testWorkspaceForResolver()
	resolved := "user-1"
	identities.rows[identityKey(ws.WorkspaceID, "U0ALREADY1")] = &slackstore.SlackIdentity{
		WorkspaceID: ws.WorkspaceID, SlackUserID: "U0ALREADY1", UserID: &resolved, LastAttemptAt: time.Now(),
	}
	secrets := &fakeSecrets{token: "xoxb-test"}
	r := &IdentityResolver{secrets: secrets, users: &fakeUsers{}, identities: identities, client: client}

	r.resolveSender(context.Background(), ws, "U0ALREADY1")

	if *hits != 0 {
		t.Errorf("users.info hits = %d; want 0 (already resolved)", *hits)
	}
	if secrets.calls != 0 {
		t.Errorf("secrets calls = %d; want 0 (already resolved)", secrets.calls)
	}
}

func TestResolveSender_TTL_SuppressesInsideWindowAllowsAfter(t *testing.T) {
	client, hits := newUsersInfoServer(t, map[string]any{
		"ok": true,
		"user": map[string]any{
			"is_bot": false, "deleted": false,
			"profile": map[string]any{"email": "later@example.com", "real_name": "Later"},
		},
	})
	identities := newFakeIdentityStore()
	ws := testWorkspaceForResolver()
	identities.rows[identityKey(ws.WorkspaceID, "U0TTLTEST1")] = &slackstore.SlackIdentity{
		WorkspaceID: ws.WorkspaceID, SlackUserID: "U0TTLTEST1", UserID: nil, LastAttemptAt: time.Now(),
	}
	r := &IdentityResolver{secrets: &fakeSecrets{token: "xoxb-test"}, users: &fakeUsers{ids: []string{"user-1"}}, identities: identities, client: client}

	// Inside the TTL: no users.info call, no write.
	r.resolveSender(context.Background(), ws, "U0TTLTEST1")
	if *hits != 0 {
		t.Fatalf("users.info hits inside TTL = %d; want 0", *hits)
	}

	// Past the TTL: the resolver retries and this time resolves.
	identities.rows[identityKey(ws.WorkspaceID, "U0TTLTEST1")].LastAttemptAt = time.Now().Add(-slackIdentityRetryTTL - time.Minute)
	r.resolveSender(context.Background(), ws, "U0TTLTEST1")
	if *hits != 1 {
		t.Fatalf("users.info hits after TTL = %d; want 1", *hits)
	}
	row := identities.rows[identityKey(ws.WorkspaceID, "U0TTLTEST1")]
	if row.UserID == nil || *row.UserID != "user-1" {
		t.Errorf("row after TTL retry = %+v; want resolved to user-1", row)
	}
}

func TestResolveSender_UsersInfoError_WritesNothing(t *testing.T) {
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	identities := newFakeIdentityStore()
	r := &IdentityResolver{secrets: &fakeSecrets{token: "xoxb-test"}, users: &fakeUsers{ids: []string{"user-1"}}, identities: identities, client: srv.Client()}
	ws := testWorkspaceForResolver()

	r.resolveSender(context.Background(), ws, "U0APIERROR")

	if len(identities.rows) != 0 {
		t.Errorf("rows = %v; want none written on a transient users.info error", identities.rows)
	}
	if identities.markAttemptCalls != 0 || identities.upsertCalls != 0 {
		t.Errorf("markAttemptCalls=%d upsertCalls=%d; want 0/0 (transient error must not consume the retry TTL)",
			identities.markAttemptCalls, identities.upsertCalls)
	}
}

func TestResolveSender_SecretsError_WritesNothing(t *testing.T) {
	identities := newFakeIdentityStore()
	r := &IdentityResolver{secrets: &fakeSecrets{err: errors.New("boom")}, users: &fakeUsers{}, identities: identities, client: http.DefaultClient}
	ws := testWorkspaceForResolver()

	r.resolveSender(context.Background(), ws, "U0SECRETERR")

	if len(identities.rows) != 0 {
		t.Errorf("rows = %v; want none written when the bot token read fails", identities.rows)
	}
}

func TestResolveSender_VerifiedEmailLookupError_WritesNothing(t *testing.T) {
	client, _ := newUsersInfoServer(t, map[string]any{
		"ok": true,
		"user": map[string]any{
			"is_bot": false, "deleted": false,
			"profile": map[string]any{"email": "ada@example.com", "real_name": "Ada"},
		},
	})
	identities := newFakeIdentityStore()
	r := &IdentityResolver{secrets: &fakeSecrets{token: "xoxb-test"}, users: &fakeUsers{err: errors.New("db down")}, identities: identities, client: client}
	ws := testWorkspaceForResolver()

	r.resolveSender(context.Background(), ws, "U0USERSERR1")

	if len(identities.rows) != 0 {
		t.Errorf("rows = %v; want none written when the verified-email lookup errors", identities.rows)
	}
}

func TestResolveSender_EmptySlackUserID_NoOp(t *testing.T) {
	identities := newFakeIdentityStore()
	secrets := &fakeSecrets{token: "xoxb-test"}
	r := &IdentityResolver{secrets: secrets, users: &fakeUsers{}, identities: identities, client: http.DefaultClient}

	r.resolveSender(context.Background(), testWorkspaceForResolver(), "")

	if secrets.calls != 0 || len(identities.rows) != 0 {
		t.Error("resolveSender with an empty slackUserID must be a complete no-op")
	}
}

func TestResolveSender_LookupError_WritesNothing(t *testing.T) {
	identities := newFakeIdentityStore()
	identities.lookupErr = errors.New("db down")
	secrets := &fakeSecrets{token: "xoxb-test"}
	r := &IdentityResolver{secrets: secrets, users: &fakeUsers{}, identities: identities, client: http.DefaultClient}

	r.resolveSender(context.Background(), testWorkspaceForResolver(), "U0LOOKUPERR")

	if secrets.calls != 0 {
		t.Error("a failed lookup must short-circuit before reading the bot token")
	}
}
