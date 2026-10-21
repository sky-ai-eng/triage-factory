package agenthost

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// startMovedIssueJira is a Jira on which issue 10007 moved from OLD-7 to
// NEW-7: a read by either key or by the id answers under NEW-7, and the
// mutating verbs accept either.
func startMovedIssueJira(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/transitions"):
			if r.Method == http.MethodGet {
				_, _ = io.WriteString(w, `{"transitions":[{"id":"31","name":"Done","to":{"id":"5","name":"Done"}}]}`)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(path, "/comment"):
			_, _ = io.WriteString(w, `{"id":"20001"}`)
		case r.Method == http.MethodGet && strings.Contains(path, "/issue/"):
			ref := path[strings.LastIndex(path, "/")+1:]
			if ref != "OLD-7" && ref != "NEW-7" && ref != "10007" {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"errorMessages":["Issue does not exist"]}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":"10007","key":"NEW-7","fields":{"summary":"Moved"}}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestLocalClient_JiraWriteOnAnOldKey_ResolvesTheExistingEntity: an agent
// writing to the key an issue has left records against the issue's current
// key and its existing entity — artifacts keyed on its id, the touch on the
// entity that already carries it — and creates nothing keyed by the old key.
func TestLocalClient_JiraWriteOnAnOldKey_ResolvesTheExistingEntity(t *testing.T) {
	jira := startMovedIssueJira(t)
	conn, stores, info := newJiraRecordingStoresConn(t, jira.URL, true)
	ctx := context.Background()
	site := jira.URL
	existing, _, err := stores.Entities.FindOrCreateJiraSystem(ctx, runmode.LocalDefaultOrgID, site, "NEW-7", "10007", dbseed.JiraIssueProject("NEW-7"), "Moved", site+"/browse/NEW-7")
	if err != nil {
		t.Fatal(err)
	}
	client := NewLocal(stores, info)

	if err := client.JiraTransitionTo(ctx, "OLD-7", "Done"); err != nil {
		t.Fatalf("JiraTransitionTo: %v", err)
	}
	if err := client.JiraAddComment(ctx, "old-7", "still here"); err != nil {
		t.Fatalf("JiraAddComment: %v", err)
	}

	resource := domain.JiraIssueResource(site, "10007")
	for _, a := range listConversationArtifacts(t, stores, info.ConversationID) {
		if a.Target != "NEW-7" {
			t.Errorf("%s artifact target = %q, want the issue's current key", a.Kind, a.Target)
		}
		if !strings.HasPrefix(a.DedupKey, domain.ArtifactDedupKey(domain.ArtifactProviderJira, a.Kind, resource, "")) {
			t.Errorf("%s artifact dedup key = %q, want it keyed on %s", a.Kind, a.DedupKey, resource)
		}
		if a.EntityID != existing.ID {
			t.Errorf("%s artifact entity = %q, want the existing entity %s", a.Kind, a.EntityID, existing.ID)
		}
	}
	if role := touchRole(t, conn, info.ConversationID, existing.ID); role != domain.MemoryRoleTouched {
		t.Errorf("touch role on the existing entity = %q, want touched", role)
	}
	var n int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM entities WHERE source = 'jira'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d jira entities, want only the existing one", n)
	}
}

// TestLocalClient_JiraReadOnAnOldKey_RenamesTheEntity: a read by the key an
// issue has left answers under its current key, and the touch renames the
// entity, stored under the old one, onto it.
func TestLocalClient_JiraReadOnAnOldKey_RenamesTheEntity(t *testing.T) {
	jira := startMovedIssueJira(t)
	_, stores, info := newJiraRecordingStoresConn(t, jira.URL, true)
	ctx := context.Background()
	site := jira.URL
	stale, _, err := stores.Entities.FindOrCreateJiraSystem(ctx, runmode.LocalDefaultOrgID, site, "OLD-7", "10007", dbseed.JiraIssueProject("OLD-7"), "Moved", site+"/browse/OLD-7")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewLocal(stores, info).JiraGetIssue(ctx, "OLD-7"); err != nil {
		t.Fatalf("JiraGetIssue: %v", err)
	}
	got, _ := stores.Entities.GetSystem(ctx, runmode.LocalDefaultOrgID, stale.ID)
	if got.SourceID != "NEW-7" || got.URL != site+"/browse/NEW-7" {
		t.Errorf("entity = %+v, want it renamed onto NEW-7", got)
	}
}

// TestLocalClient_MemoryLoad_OldJiraKeyFindsTheEntity: a memory load by the key
// an issue has left misses the key lookup, resolves through Jira — which
// follows the move — and finds the entity by the issue's id.
func TestLocalClient_MemoryLoad_OldJiraKeyFindsTheEntity(t *testing.T) {
	jira := startMovedIssueJira(t)
	conn, stores, info := newJiraRecordingStoresConn(t, jira.URL, true)
	ctx := context.Background()
	site := jira.URL
	ent, _, err := stores.Entities.FindOrCreateJiraSystem(ctx, runmode.LocalDefaultOrgID, site, "NEW-7", "10007", dbseed.JiraIssueProject("NEW-7"), "Moved", "")
	if err != nil {
		t.Fatal(err)
	}
	seedAuthoringMemory(t, conn, runmode.LocalDefaultOrgID, ent.ID, uuid.New().String(), "what I learned", memBase, domain.MemoryRolePrimary)

	res, err := NewLocal(stores, info).MemoryLoad(ctx, "jira", "OLD-7", 20)
	if err != nil {
		t.Fatalf("MemoryLoad: %v", err)
	}
	if res.EntityID != ent.ID || len(res.Memories) != 1 || res.Memories[0].Content != "what I learned" {
		t.Errorf("result = %+v, want the moved issue's memory", res)
	}

	miss, err := NewLocal(stores, info).MemoryLoad(ctx, "jira", "GONE-1", 20)
	if err != nil || miss.EntityID != "" {
		t.Errorf("load of an unknown key = %+v err=%v, want a miss", miss, err)
	}
}

// TestServer_SandboxUpsertArtifact_RecordsBranchesOnly pins the sandbox's
// artifact door: over the socket it records a branch and refuses every other
// kind. An artifact is linked to the entity its key names, and that
// resolution follows the target onto the entity, so a Jira issue artifact the
// sandbox composed, keyed on an existing issue's id under a key of its own
// choosing, would rename that issue's entity.
func TestServer_SandboxUpsertArtifact_RecordsBranchesOnly(t *testing.T) {
	const site = "https://jira.example.test"
	_, stores, info := newJiraRecordingStoresConn(t, site, true)
	ctx := context.Background()
	if _, _, err := stores.Entities.FindOrCreateJiraSystem(ctx, info.OrgID, site, "ABC-1", "10001", dbseed.JiraIssueProject("ABC-1"), "Issue", site+"/browse/ABC-1"); err != nil {
		t.Fatal(err)
	}
	registerRepo(t, stores, "octo", "repo", "1")

	sockPath := tempSocket(t)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := NewServer(stores, info, nil)
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() {
		_ = listener.Close()
		_ = srv.Shutdown(context.Background())
	})
	sandbox := Dial(sockPath)
	t.Cleanup(func() { _ = sandbox.Close() })

	forged := domain.Artifact{
		Provider: domain.ArtifactProviderJira,
		Kind:     domain.ArtifactKindIssue,
		Target:   "EVIL-9",
		State:    domain.ArtifactStateIssueUpdated,
		DedupKey: domain.ArtifactDedupKey(domain.ArtifactProviderJira, domain.ArtifactKindIssue, domain.JiraIssueResource(site, "10001"), ""),
	}
	if _, err := sandbox.UpsertArtifact(ctx, forged); err == nil {
		t.Fatal("the sandbox recorded a Jira issue artifact; want it refused")
	}
	issue, err := stores.Entities.GetByExternalIDSystem(ctx, info.OrgID, domain.ArtifactProviderJira, site, "10001")
	if err != nil || issue == nil || issue.SourceID != "ABC-1" {
		t.Fatalf("issue entity after the refused write = %+v, %v; want it still keyed ABC-1", issue, err)
	}
	if arts := listConversationArtifacts(t, stores, info.ConversationID); len(arts) != 0 {
		t.Fatalf("artifacts after the refused write = %+v, want none", arts)
	}

	branch, ok := domain.NewBranchArtifact("octo/repo", "refs/heads/feature/x", "abc123", true)
	if !ok {
		t.Fatal("build branch artifact")
	}
	if _, err := sandbox.UpsertArtifact(ctx, branch); err != nil {
		t.Fatalf("UpsertArtifact (branch): %v", err)
	}
	if arts := listConversationArtifacts(t, stores, info.ConversationID); len(arts) != 1 || arts[0].Kind != domain.ArtifactKindBranch {
		t.Fatalf("artifacts after the branch = %+v, want the one branch", arts)
	}
}
