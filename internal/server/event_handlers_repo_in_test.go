package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// createRepoRule creates a github:pr:opened rule scoped by repo_in and returns
// the response.
func createRepoRule(t *testing.T, s *Server, repoIn ...string) (int, map[string]any) {
	t.Helper()
	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/rules", map[string]any{
		"event_type":      "github:pr:opened",
		"name":            "scoped",
		"scope_predicate": map[string]any{"repo_in": repoIn},
	})
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	return rec.Code, got
}

// repoInFaultMessages returns the scope_predicate faults of a 422 answer,
// failing the test on any other status, or on a fault whose field is not the
// predicate key its message names.
func repoInFaultMessages(t *testing.T, code int, body map[string]any) []string {
	t.Helper()
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %v", code, body)
	}
	items, _ := body["errors"].([]any)
	var messages []string
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		msg, _ := item["message"].(string)
		field, _ := item["field"].(string)
		key, keyed := strings.CutPrefix(field, fieldScopePredicate+".")
		if item["reason"] != "INVALID_FIELD" || !keyed || !strings.HasPrefix(msg, key) {
			t.Errorf("fault = %v, want INVALID_FIELD on %s.<the key the message names>", item, fieldScopePredicate)
		}
		messages = append(messages, msg)
	}
	return messages
}

// untrackAllBut leaves the local team tracking only the named repositories.
func untrackAllBut(t *testing.T, s *Server, keep ...domain.TeamGitHubRepo) {
	t.Helper()
	if err := dbtest.TrackRepos(t.Context(), s.allStores.Repos, s.allStores.TeamGitHubRepos, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, seedRepoHost(t, s), keep); err != nil {
		t.Fatalf("replace tracked repos: %v", err)
	}
}

func TestEventHandlerRepoIn_CreateAcceptsTrackedRepos(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	apiID := seedConfiguredRepo(t, s, "acme", "api")
	webID := seedConfiguredRepo(t, s, "acme", "web")

	code, got := createRepoRule(t, s, apiID, webID)
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", code, got)
	}
	want := fmt.Sprintf(`{"repo_in":[%q,%q]}`, apiID, webID)
	if got["scope_predicate_json"] != want {
		t.Errorf("stored predicate = %v, want %s", got["scope_predicate_json"], want)
	}
}

// A registry row the team does not track, an owner/name, and an id nothing
// answers to are each refused, every one reported, and nothing is written.
func TestEventHandlerRepoIn_CreateRefusesWhatTheTeamDoesNotTrack(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	apiID := seedConfiguredRepo(t, s, "acme", "api")
	otherID := seedUntrackedRepo(t, s, "acme", "other")
	const unknownID = "00000000-0000-0000-0000-0000000000ff"
	before := len(listEventHandlers(t, s, "rule"))

	code, got := createRepoRule(t, s, apiID, otherID, "acme/api", unknownID)
	messages := repoInFaultMessages(t, code, got)
	if len(messages) != 3 {
		t.Fatalf("faults = %q, want one each for the untracked row, the name and the unknown id", messages)
	}
	for i, want := range []string{otherID, "acme/api", unknownID} {
		if !strings.Contains(messages[i], fmt.Sprintf("%q", want)) {
			t.Errorf("fault %d = %q, want it to name %q", i, messages[i], want)
		}
	}
	if after := len(listEventHandlers(t, s, "rule")); after != before {
		t.Errorf("a refused create changed the rule count from %d to %d", before, after)
	}
}

func TestEventHandlerRepoIn_CreateTriggerRefusesUntrackedRepos(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	seedBlueprintForTrigger(t, s, "p-repo-in")
	otherID := seedUntrackedRepo(t, s, "acme", "other")

	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/triggers", map[string]any{
		"event_type":      "github:pr:opened",
		"blueprint_id":    "p-repo-in",
		"scope_predicate": map[string]any{"repo_in": []string{otherID}},
	})
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if messages := repoInFaultMessages(t, rec.Code, got); len(messages) != 1 {
		t.Errorf("faults = %q, want one", messages)
	}
}

// Untracking a repository must not block an edit that leaves it in place; an
// entry the edit adds is still checked, and a refused edit changes nothing.
func TestEventHandlerRepoIn_PatchChecksOnlyAddedRepos(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	apiID := seedConfiguredRepo(t, s, "acme", "api")
	webID := seedConfiguredRepo(t, s, "acme", "web")
	code, created := createRepoRule(t, s, apiID)
	if code != http.StatusCreated {
		t.Fatalf("seed rule: %d: %v", code, created)
	}
	id := created["id"].(string)
	untrackAllBut(t, s, domain.TeamGitHubRepo{Owner: "acme", Repo: "web"})

	if rec := doJSON(t, s, http.MethodPatch, "/api/event-handlers/"+id, map[string]any{"name": "renamed"}); rec.Code != http.StatusOK {
		t.Fatalf("name-only PATCH: %d: %s", rec.Code, rec.Body.String())
	}
	kept := fmt.Sprintf(`{"repo_in":[%q,%q]}`, apiID, webID)
	if rec := doJSON(t, s, http.MethodPatch, "/api/event-handlers/"+id, map[string]any{
		"scope_predicate": map[string]any{"repo_in": []string{apiID, webID}},
	}); rec.Code != http.StatusOK {
		t.Fatalf("PATCH keeping an untracked repo and adding a tracked one: %d: %s", rec.Code, rec.Body.String())
	}

	otherID := seedUntrackedRepo(t, s, "acme", "other")
	rec := doJSON(t, s, http.MethodPatch, "/api/event-handlers/"+id, map[string]any{
		"scope_predicate": map[string]any{"repo_in": []string{apiID, otherID}},
	})
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if messages := repoInFaultMessages(t, rec.Code, got); len(messages) != 1 || !strings.Contains(messages[0], otherID) {
		t.Errorf("faults = %q, want one naming the added untracked row", messages)
	}
	stored := doJSON(t, s, http.MethodGet, "/api/event-handlers/"+id, nil)
	var row map[string]any
	_ = json.Unmarshal(stored.Body.Bytes(), &row)
	if row["scope_predicate_json"] != kept {
		t.Errorf("refused PATCH changed the predicate: %v, want %s", row["scope_predicate_json"], kept)
	}
}

func TestEventHandlerRepoIn_PromoteRefusesUntrackedRepos(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	seedBlueprintForTrigger(t, s, "p-promote-repo-in")
	id := createUserRule(t, s)
	otherID := seedUntrackedRepo(t, s, "acme", "other")

	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/"+id+"/promote", map[string]any{
		"blueprint_id":    "p-promote-repo-in",
		"scope_predicate": map[string]any{"repo_in": []string{otherID}},
	})
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if messages := repoInFaultMessages(t, rec.Code, got); len(messages) != 1 {
		t.Errorf("faults = %q, want one", messages)
	}
}

// The predicate schema has no single-valued repo key, so a caller sending one
// is told so rather than having it dropped.
func TestEventHandlerRepoIn_RefusesRepoKey(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	seedConfiguredRepo(t, s, "acme", "api")

	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/rules", map[string]any{
		"event_type":      "github:pr:opened",
		"name":            "old shape",
		"scope_predicate": map[string]any{"repo": "acme/api"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// The check runs under the caller's claims, so a plain team member must be able
// to read the org's GitHub host and the team's tracked set through RLS, or every
// repo_in entry that member writes would be refused.
func TestRepoInFaults_PlainMemberUnderRLS_Postgres(t *testing.T) {
	rig := newRepoScopeRig(t)
	rig.setGitHubHost(t, dbtest.TestGitHubHost)
	webID := rig.repoID(t, "acme", "web")
	apiID := rig.repoID(t, "acme", "api")

	faultsFor := func(repoIn ...string) []PredicateReferenceFault {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"repo_in": repoIn})
		predicate := string(b)
		var faults []PredicateReferenceFault
		if err := rig.s.tx.WithTx(t.Context(), rig.orgID, rig.plainB, func(tx db.TxStores) error {
			var e error
			faults, e = repoInFaults(t.Context(), tx, rig.orgID, rig.teamB, &predicate, nil)
			return e
		}); err != nil {
			t.Fatalf("repoInFaults(%v): %v", repoIn, err)
		}
		return faults
	}

	if faults := faultsFor(webID); len(faults) != 0 {
		t.Errorf("teamB's own repository: faults = %v, want none", faults)
	}
	if faults := faultsFor(apiID); len(faults) != 1 {
		t.Errorf("teamA's repository: faults = %v, want one", faults)
	}
}

// Every GitHub event type whose predicate has repo_in registers the check, so
// none of them stores a reference the team cannot match on.
func TestRepoInFaults_RegisteredForEveryGitHubRepoInType(t *testing.T) {
	predicateReferenceMu.RLock()
	defer predicateReferenceMu.RUnlock()
	n := 0
	for eventType := range predicateReferenceChecks {
		if strings.HasPrefix(eventType, "github:") {
			n++
		}
	}
	if n != 21 {
		t.Errorf("registered checks for %d github event types, want the 21 with a repo_in field", n)
	}
	if _, ok := predicateReferenceChecks[domain.EventGitHubPRCICheckFailed]; !ok {
		t.Errorf("no check registered for %s", domain.EventGitHubPRCICheckFailed)
	}
}
