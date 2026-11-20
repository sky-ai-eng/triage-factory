package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// retireAfterRead merges one blueprint into another, once, in a transaction of
// its own, right after the first read-only transaction that reads the absorbed
// blueprint ends: the merge a concurrent request would commit between a
// handler write's read of the blueprint and its write. Unarmed, it does
// nothing.
type retireAfterRead struct {
	hostID, absorbedID string
	armed              bool
	ran                bool
	err                error
}

func (r *retireAfterRead) arm(hostID, absorbedID string) {
	r.hostID, r.absorbedID, r.armed = hostID, absorbedID, true
}

// check fails the test unless the merge ran and committed.
func (r *retireAfterRead) check(t *testing.T) {
	t.Helper()
	if !r.ran {
		t.Fatal("the write never read the blueprint the merge retires")
	}
	if r.err != nil {
		t.Fatalf("the merge between the read and the write: %v", r.err)
	}
}

type retireAfterReadTx struct {
	db.TxRunner
	race *retireAfterRead
}

func (w retireAfterReadTx) WithReadTx(ctx context.Context, orgID, userID string, fn func(db.TxStores) error) error {
	read := false
	err := w.TxRunner.WithReadTx(ctx, orgID, userID, func(tx db.TxStores) error {
		if w.race.armed {
			tx.Blueprints = readWatchBlueprints{BlueprintStore: tx.Blueprints, id: w.race.absorbedID, read: &read}
		}
		return fn(tx)
	})
	if err == nil && read {
		w.race.armed, w.race.ran = false, true
		w.race.err = w.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
			return tx.Blueprints.MergeInto(ctx, orgID, w.race.hostID, w.race.absorbedID)
		})
	}
	return err
}

type readWatchBlueprints struct {
	db.BlueprintStore
	id   string
	read *bool
}

func (b readWatchBlueprints) Get(ctx context.Context, orgID, id string) (*domain.Blueprint, error) {
	got, err := b.BlueprintStore.Get(ctx, orgID, id)
	if id == b.id && got != nil {
		*b.read = true
	}
	return got, err
}

// TestEventHandlers_BlueprintRetiredAfterTheRouteReadIt: a create or PATCH
// naming a blueprint that a merge retires after the route read it answers the
// 404 a missing blueprint gets, and leaves no handler on the retired
// blueprint.
func TestEventHandlers_BlueprintRetiredAfterTheRouteReadIt(t *testing.T) {
	for _, write := range []string{"create", "patch"} {
		t.Run(write, func(t *testing.T) {
			race := &retireAfterRead{}
			s := newTestServerWithTx(t, func(tx db.TxRunner) db.TxRunner { return retireAfterReadTx{TxRunner: tx, race: race} })
			configureEventSources(t, s)
			host, absorbed := fixtureUUID("retire-host"), fixtureUUID("retire-absorbed")
			seedHandlerBlueprint(t, s, host)
			seedHandlerBlueprint(t, s, absorbed)

			path, method := "/api/event-handlers", http.MethodPost
			body := map[string]any{"event_type": domain.EventGitHubPROpened, "blueprint_id": absorbed}
			if write == "patch" {
				path, method = "/api/event-handlers/"+createWatchHandler(t, s), http.MethodPatch
				body = map[string]any{"blueprint_id": absorbed}
			}
			race.arm(host, absorbed)
			rec := doJSON(t, s, method, path, body)
			race.check(t)
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s naming a blueprint retired after the route read it = %d %s, want 404", write, rec.Code, rec.Body.String())
			}
			on, err := s.allStores.EventHandlers.ListForBlueprint(t.Context(), runmode.LocalDefaultOrgID, absorbed)
			if err != nil {
				t.Fatalf("ListForBlueprint: %v", err)
			}
			if len(on) != 0 {
				t.Errorf("handlers on the retired blueprint = %d, want 0", len(on))
			}
		})
	}
}
