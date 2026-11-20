package server

import (
	"net/http"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// A handler write that names a blueprint answers 404 when the blueprint is
// deleted after the route's first read of it, and leaves no handler on it,
// because the write reads the blueprint again in its own transaction. The
// delete lands while the route waits on Jira's catalog, between that first
// read and the write.
func TestEventHandlerWrite_BlueprintDeletedAfterTheFirstRead(t *testing.T) {
	predicate := map[string]any{"status_id_in": []string{statusInProgressID}}
	for _, tc := range []struct {
		name string
		// write sends the handler write naming blueprintID, setting up what it
		// needs before the catalog hook is armed.
		write func(t *testing.T, r *jiraCatalogRig, arm func(), blueprintID string) (int, map[string]any)
	}{
		{"create", func(t *testing.T, r *jiraCatalogRig, arm func(), blueprintID string) (int, map[string]any) {
			arm()
			return r.do(t, http.MethodPost, "/api/event-handlers", map[string]any{
				"event_type": domain.EventJiraIssueAssigned, "blueprint_id": blueprintID, "scope_predicate": predicate,
			})
		}},
		{"patch_attaching_it", func(t *testing.T, r *jiraCatalogRig, arm func(), blueprintID string) (int, map[string]any) {
			id := createHandlerID(t, r.s, map[string]any{"event_type": domain.EventJiraIssueAssigned})
			arm()
			code, got := r.patch(t, id, map[string]any{"blueprint_id": blueprintID, "scope_predicate": predicate})
			if h := getEventHandler(t, r.s, id); h["blueprint_id"] != nil {
				t.Errorf("handler after the refused PATCH = %v; want it still without a blueprint", h)
			}
			return code, got
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newJiraCatalogRig(t)
			bp, _ := createWrappedBlueprint(t, r.s, "DeletedMidWrite")

			var (
				once      sync.Once
				deleteHit int
			)
			arm := func() {
				r.fake.OnCall(func() {
					once.Do(func() {
						deleteHit = doJSON(t, r.s, http.MethodDelete, "/api/blueprints/"+bp, nil).Code
					})
				})
			}
			code, got := tc.write(t, r, arm, bp)
			if deleteHit != http.StatusOK {
				t.Fatalf("the blueprint delete during the catalog check = %d; want 200", deleteHit)
			}
			if code != http.StatusNotFound {
				t.Fatalf("handler write = %d %v; want 404 for the deleted blueprint", code, got)
			}
			if hs := handlersForBlueprint(t, r.s, bp); len(hs) != 0 {
				t.Errorf("handlers on the deleted blueprint = %v; want none", hs)
			}
		})
	}
}
