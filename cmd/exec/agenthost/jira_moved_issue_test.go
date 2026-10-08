package agenthost

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

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
	site := testScope("jira")
	existing, _, err := stores.Entities.FindOrCreateSystem(ctx, runmode.LocalDefaultOrgID, "jira", site, "NEW-7", "10007", "issue", "Moved", site+"/browse/NEW-7")
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
		if !domain.ArtifactKeyHasResource(a.DedupKey, domain.ArtifactProviderJira, resource) {
			t.Errorf("%s artifact dedup key = %q, want it keyed on %s", a.Kind, a.DedupKey, resource)
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
	site := testScope("jira")
	stale, _, err := stores.Entities.FindOrCreateSystem(ctx, runmode.LocalDefaultOrgID, "jira", site, "OLD-7", "10007", "issue", "Moved", site+"/browse/OLD-7")
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
	site := testScope("jira")
	ent, _, err := stores.Entities.FindOrCreateSystem(ctx, runmode.LocalDefaultOrgID, "jira", site, "NEW-7", "10007", "issue", "Moved", "")
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
