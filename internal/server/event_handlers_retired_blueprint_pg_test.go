package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/blueprints"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// seedBlueprint creates a bare blueprint on the rig's team, as its admin.
func (r *viewerRig) seedBlueprint(t *testing.T, name string) string {
	t.Helper()
	id := uuid.NewString()
	if err := r.s.tx.WithTx(t.Context(), r.orgID, r.admin, func(tx db.TxStores) error {
		_, err := tx.Blueprints.Create(t.Context(), r.orgID, r.teamID, domain.Blueprint{ID: id, Name: name, Source: "user", TeamID: r.teamID})
		return err
	}); err != nil {
		t.Fatalf("seed blueprint %s: %v", name, err)
	}
	return id
}

func (r *viewerRig) createHandler(caller string, body map[string]any) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r.eh.handleEventHandlerCreate(rec, r.req(http.MethodPost, "/api/event-handlers", caller, body))
	return rec
}

func (r *viewerRig) patchHandler(caller, id string, body map[string]any) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := r.req(http.MethodPatch, "/api/event-handlers/"+id, caller, body)
	req.SetPathValue("id", id)
	r.eh.handleEventHandlerUpdate(rec, req)
	return rec
}

// handlersOn counts the live handlers that name blueprintID, read past RLS.
func (r *viewerRig) handlersOn(t *testing.T, blueprintID string) int {
	t.Helper()
	var n int
	if err := r.h.AdminDB.QueryRow(
		`SELECT count(*) FROM event_handlers WHERE blueprint_id = $1 AND deleted_at IS NULL`, blueprintID,
	).Scan(&n); err != nil {
		t.Fatalf("count handlers on %s: %v", blueprintID, err)
	}
	return n
}

// TestEventHandlers_BlueprintRetiredAfterTheRouteReadIt_Postgres is the
// SQLite test of the same name on the RLS pool, under the caller's claims.
func TestEventHandlers_BlueprintRetiredAfterTheRouteReadIt_Postgres(t *testing.T) {
	for _, write := range []string{"create", "patch"} {
		t.Run(write, func(t *testing.T) {
			race := &retireAfterRead{}
			r := newViewerRigWithTx(t, func(tx db.TxRunner) db.TxRunner { return retireAfterReadTx{TxRunner: tx, race: race} })
			r.bindGitHubPAT(t)
			host, absorbed := r.seedBlueprint(t, "host"), r.seedBlueprint(t, "absorbed")

			var rec *httptest.ResponseRecorder
			if write == "create" {
				race.arm(host, absorbed)
				rec = r.createHandler(r.member, map[string]any{"event_type": domain.EventGitHubPROpened, "blueprint_id": absorbed})
			} else {
				created := r.createHandler(r.member, map[string]any{"event_type": domain.EventGitHubPROpened})
				var h domain.EventHandler
				if created.Code != http.StatusCreated || json.Unmarshal(created.Body.Bytes(), &h) != nil {
					t.Fatalf("create the handler to patch: %d %s", created.Code, created.Body.String())
				}
				race.arm(host, absorbed)
				rec = r.patchHandler(r.member, h.ID, map[string]any{"blueprint_id": absorbed})
			}
			race.check(t)
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s naming a blueprint retired after the route read it = %d %s, want 404", write, rec.Code, rec.Body.String())
			}
			if n := r.handlersOn(t, absorbed); n != 0 {
				t.Errorf("handlers on the retired blueprint = %d, want 0", n)
			}
		})
	}
}

// pausePoint holds the first transaction that reaches it, once armed, open at
// that point until the test frees it.
type pausePoint struct {
	armed   atomic.Bool
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func newPausePoint(t *testing.T) *pausePoint {
	p := &pausePoint{reached: make(chan struct{}), release: make(chan struct{})}
	// A failed check must still end the held transaction, or its locks
	// outlive the test.
	t.Cleanup(p.free)
	return p
}

func (p *pausePoint) hold() {
	if !p.armed.CompareAndSwap(true, false) {
		return
	}
	close(p.reached)
	<-p.release
}

func (p *pausePoint) free() { p.once.Do(func() { close(p.release) }) }

func (p *pausePoint) waitReached(t *testing.T, what string) {
	t.Helper()
	select {
	case <-p.reached:
	case <-time.After(15 * time.Second):
		t.Fatalf("%s never reached its pause", what)
	}
}

// pausingHandlersTx pauses a write transaction right after it lists a
// blueprint's handlers, which is the merge's trigger check, or right before it
// inserts a handler, which is after the create's blueprint lock.
type pausingHandlersTx struct {
	db.TxRunner
	afterList, beforeCreate *pausePoint
}

func (w pausingHandlersTx) WithTx(ctx context.Context, orgID, userID string, fn func(db.TxStores) error) error {
	return w.TxRunner.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		tx.EventHandlers = pausingHandlers{EventHandlerStore: tx.EventHandlers, w: w}
		return fn(tx)
	})
}

type pausingHandlers struct {
	db.EventHandlerStore
	w pausingHandlersTx
}

func (h pausingHandlers) ListForBlueprint(ctx context.Context, orgID, blueprintID string) ([]domain.EventHandler, error) {
	out, err := h.EventHandlerStore.ListForBlueprint(ctx, orgID, blueprintID)
	h.w.afterList.hold()
	return out, err
}

func (h pausingHandlers) Create(ctx context.Context, orgID, teamID string, eh domain.EventHandler) (domain.EventHandler, error) {
	h.w.beforeCreate.hold()
	return h.EventHandlerStore.Create(ctx, orgID, teamID, eh)
}

// waitForLockWait polls until a backend in the test database is blocked on a
// lock. The race tests hold one request open and wait for the other to block,
// on whichever statement it blocks at, so that the outcome is what they judge.
func waitForLockWait(t *testing.T, h *pgtest.Harness, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := h.AdminDB.QueryRow(`
			SELECT count(*) FROM pg_stat_activity
			 WHERE wait_event_type = 'Lock' AND datname = current_database()
		`).Scan(&waiting); err != nil {
			t.Fatalf("read waiting backends: %v", err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never blocked", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// serveAsync runs one request on its own goroutine and hands back its
// recorder once the handler returns.
func serveAsync(handler http.HandlerFunc, req *http.Request) <-chan *httptest.ResponseRecorder {
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		handler(rec, req)
		done <- rec
	}()
	return done
}

func receiveResponse(t *testing.T, what string, ch <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case rec := <-ch:
		return rec
	case <-time.After(15 * time.Second):
		t.Fatalf("%s never returned", what)
		return nil
	}
}

// TestEventHandlers_CreateRacesBlueprintMerge_Postgres runs a handler create
// naming a blueprint and a merge that absorbs that blueprint against each
// other, in both orders the blueprint's row locks allow. The merge checks that
// no handler names the blueprint and then retires it, so the property under
// test is that the create either commits before the check, and the merge
// refuses, or waits for the merge and answers 404: never a handler on a
// retired blueprint.
func TestEventHandlers_CreateRacesBlueprintMerge_Postgres(t *testing.T) {
	type rig struct {
		*viewerRig
		mux                     *http.ServeMux
		afterList, beforeCreate *pausePoint
		host, absorbed          string
	}
	setup := func(t *testing.T) rig {
		t.Helper()
		afterList, beforeCreate := newPausePoint(t), newPausePoint(t)
		r := newViewerRigWithTx(t, func(tx db.TxRunner) db.TxRunner {
			return pausingHandlersTx{TxRunner: tx, afterList: afterList, beforeCreate: beforeCreate}
		})
		r.bindGitHubPAT(t)
		mux := http.NewServeMux()
		mount := func(pattern string, h http.HandlerFunc) { mux.HandleFunc(pattern, h) }
		blueprints.New(r.s.tx, r.s.az).Register(httpx.Mounts{API: mount, APIMutating: mount})
		return rig{
			viewerRig: r, mux: mux, afterList: afterList, beforeCreate: beforeCreate,
			host: r.seedBlueprint(t, "host"), absorbed: r.seedBlueprint(t, "absorbed"),
		}
	}
	merge := func(r rig) <-chan *httptest.ResponseRecorder {
		return serveAsync(r.mux.ServeHTTP, r.req(http.MethodPost, "/api/blueprints/"+r.host+"/merge", r.admin,
			map[string]any{"source_blueprint_id": r.absorbed}))
	}
	create := func(r rig) <-chan *httptest.ResponseRecorder {
		return serveAsync(r.eh.handleEventHandlerCreate, r.req(http.MethodPost, "/api/event-handlers", r.member,
			map[string]any{"event_type": domain.EventGitHubPROpened, "blueprint_id": r.absorbed}))
	}

	t.Run("the_merge_checks_first", func(t *testing.T) {
		r := setup(t)

		// The merge, held after it found no handler on the absorbed blueprint.
		r.afterList.armed.Store(true)
		merged := merge(r)
		r.afterList.waitReached(t, "the merge's trigger check")

		created := create(r)
		waitForLockWait(t, r.h, "the create")

		r.afterList.free()
		if rec := receiveResponse(t, "the merge", merged); rec.Code != http.StatusOK {
			t.Fatalf("merge = %d %s, want 200", rec.Code, rec.Body.String())
		}
		if rec := receiveResponse(t, "the create", created); rec.Code != http.StatusNotFound {
			t.Errorf("create behind the merge = %d %s, want 404", rec.Code, rec.Body.String())
		}
		if n := r.handlersOn(t, r.absorbed); n != 0 {
			t.Errorf("handlers on the retired blueprint = %d, want 0", n)
		}
	})

	t.Run("the_create_locks_first", func(t *testing.T) {
		r := setup(t)

		// The create, held after its blueprint lock and before its insert.
		r.beforeCreate.armed.Store(true)
		created := create(r)
		r.beforeCreate.waitReached(t, "the create's insert")

		merged := merge(r)
		waitForLockWait(t, r.h, "the merge")

		r.beforeCreate.free()
		if rec := receiveResponse(t, "the create", created); rec.Code != http.StatusCreated {
			t.Fatalf("create = %d %s, want 201", rec.Code, rec.Body.String())
		}
		rec := receiveResponse(t, "the merge", merged)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), httpx.ReasonConflict) {
			t.Errorf("merge behind the create = %d %s, want 422 %s", rec.Code, rec.Body.String(), httpx.ReasonConflict)
		}
		var live bool
		if err := r.h.AdminDB.QueryRow(`SELECT deleted_at IS NULL FROM blueprints WHERE id = $1`, r.absorbed).Scan(&live); err != nil || !live {
			t.Errorf("absorbed blueprint live = %v (%v), want the refused merge to leave it live", live, err)
		}
		if n := r.handlersOn(t, r.absorbed); n != 1 {
			t.Errorf("handlers on the blueprint = %d, want the create's", n)
		}
	})
}
