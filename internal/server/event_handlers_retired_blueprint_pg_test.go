package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
// blueprint's handlers, or right before it writes a handler.
type pausingHandlersTx struct {
	db.TxRunner
	afterList, beforeWrite *pausePoint
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
	h.w.beforeWrite.hold()
	return h.EventHandlerStore.Create(ctx, orgID, teamID, eh)
}

func (h pausingHandlers) Update(ctx context.Context, orgID string, eh domain.EventHandler) (domain.EventHandler, error) {
	h.w.beforeWrite.hold()
	return h.EventHandlerStore.Update(ctx, orgID, eh)
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

// seedChain creates a blueprint on the rig's team whose steps are n new
// prompts, as its admin.
func (r *viewerRig) seedChain(t *testing.T, name string, n int) string {
	t.Helper()
	id := r.seedBlueprint(t, name)
	if err := r.s.tx.WithTx(t.Context(), r.orgID, r.admin, func(tx db.TxStores) error {
		prompts, briefs := make([]string, n), make([]string, n)
		for i := range prompts {
			p, err := tx.Prompts.Create(t.Context(), r.orgID, r.teamID, domain.Prompt{
				ID: uuid.NewString(), Name: name + " step", Body: "body", Source: "user",
			})
			if err != nil {
				return err
			}
			prompts[i] = p.ID
		}
		_, err := tx.Blueprints.ReplaceSteps(t.Context(), r.orgID, id, prompts, briefs)
		return err
	}); err != nil {
		t.Fatalf("seed the steps of %s: %v", name, err)
	}
	return id
}

// stepsOf counts a blueprint's steps and reports whether it is live, read
// past RLS.
func (r *viewerRig) stepsOf(t *testing.T, blueprintID string) (steps int, live bool) {
	t.Helper()
	if err := r.h.AdminDB.QueryRow(`
		SELECT (SELECT count(*) FROM blueprint_steps WHERE blueprint_id = b.id), b.deleted_at IS NULL
		  FROM blueprints b WHERE b.id = $1
	`, blueprintID).Scan(&steps, &live); err != nil {
		t.Fatalf("read blueprint %s: %v", blueprintID, err)
	}
	return steps, live
}

// absorbRig is a Postgres server with its handler writes pausable, a host of
// two steps, and a one-step blueprint for a merge or a reconnect to absorb
// into it.
type absorbRig struct {
	*viewerRig
	mux                    *http.ServeMux
	afterList, beforeWrite *pausePoint
	host, absorbed         string
	// hostVersion is host's version as seeded. The handler writes these tests
	// race leave it, so a reconnect sends it.
	hostVersion int64
}

func newAbsorbRig(t *testing.T, wrap func(db.TxRunner) db.TxRunner) absorbRig {
	t.Helper()
	afterList, beforeWrite := newPausePoint(t), newPausePoint(t)
	r := newViewerRigWithTx(t, func(tx db.TxRunner) db.TxRunner {
		var out db.TxRunner = pausingHandlersTx{TxRunner: tx, afterList: afterList, beforeWrite: beforeWrite}
		if wrap != nil {
			out = wrap(out)
		}
		return out
	})
	r.bindGitHubPAT(t)
	mux := http.NewServeMux()
	mount := func(pattern string, h http.HandlerFunc) { mux.HandleFunc(pattern, h) }
	blueprints.New(r.s.tx, r.s.az).Register(httpx.Mounts{API: mount, APIMutating: mount, Op: func(op httpx.Operation, h http.HandlerFunc) { mount(op.Spec().Pattern, h) }})
	host := r.seedChain(t, "host", 2)
	return absorbRig{
		viewerRig: r, mux: mux, afterList: afterList, beforeWrite: beforeWrite,
		host: host, absorbed: r.seedChain(t, "absorbed", 1), hostVersion: r.version(t, host),
	}
}

// absorb sends one of the two routes that retire the rig's absorbed
// blueprint, as the rig's admin.
func (r absorbRig) absorb(route string) <-chan *httptest.ResponseRecorder {
	body := map[string]any{"source_blueprint_id": r.absorbed}
	if route == "reconnect" {
		body = map[string]any{"at_step_index": 1, "target_blueprint_id": r.absorbed, "version": r.hostVersion}
	}
	return serveAsync(r.mux.ServeHTTP, r.req(http.MethodPost, "/api/blueprints/"+r.host+"/"+route, r.admin, body))
}

// TestEventHandlers_HandlerWritesRaceBlueprintAbsorb_Postgres runs a handler
// write naming a blueprint against a merge or a reconnect that absorbs that
// blueprint, in both orders the blueprint's row lock allows. The absorb
// holds the blueprint and then checks that no handler names it, so the
// property under test is that the write either commits before the hold, and
// the absorb refuses, or waits for the absorb and answers 404: never a
// handler on a retired blueprint.
func TestEventHandlers_HandlerWritesRaceBlueprintAbsorb_Postgres(t *testing.T) {
	// write prepares a handler write naming the absorbed blueprint, then
	// returns what sends it. The preparing happens before any pause is armed.
	writes := map[string]func(t *testing.T, r absorbRig) func() <-chan *httptest.ResponseRecorder{
		"create": func(t *testing.T, r absorbRig) func() <-chan *httptest.ResponseRecorder {
			return func() <-chan *httptest.ResponseRecorder {
				return serveAsync(r.eh.handleEventHandlerCreate, r.req(http.MethodPost, "/api/event-handlers", r.member,
					map[string]any{"event_type": domain.EventGitHubPROpened, "blueprint_id": r.absorbed}))
			}
		},
		"patch": func(t *testing.T, r absorbRig) func() <-chan *httptest.ResponseRecorder {
			created := r.createHandler(r.member, map[string]any{"event_type": domain.EventGitHubPROpened})
			var h domain.EventHandler
			if created.Code != http.StatusCreated || json.Unmarshal(created.Body.Bytes(), &h) != nil {
				t.Fatalf("create the handler to patch: %d %s", created.Code, created.Body.String())
			}
			return func() <-chan *httptest.ResponseRecorder {
				req := r.req(http.MethodPatch, "/api/event-handlers/"+h.ID, r.member, map[string]any{"blueprint_id": r.absorbed})
				req.SetPathValue("id", h.ID)
				return serveAsync(r.eh.handleEventHandlerUpdate, req)
			}
		},
	}
	wrote := map[string]int{"create": http.StatusCreated, "patch": http.StatusOK}

	for _, route := range []string{"merge", "reconnect"} {
		for name, prepare := range writes {
			t.Run(route+"_retires_first/"+name, func(t *testing.T) {
				r := newAbsorbRig(t, nil)
				write := prepare(t, r)

				// The absorb, held after it held the blueprint and found no
				// handler on it.
				r.afterList.armed.Store(true)
				absorbed := r.absorb(route)
				r.afterList.waitReached(t, "the "+route+"'s check")

				written := write()
				waitForLockWait(t, r.h, "the "+name)

				r.afterList.free()
				if rec := receiveResponse(t, "the "+route, absorbed); rec.Code != http.StatusOK {
					t.Fatalf("%s = %d %s, want 200", route, rec.Code, rec.Body.String())
				}
				if rec := receiveResponse(t, "the "+name, written); rec.Code != http.StatusNotFound {
					t.Errorf("%s behind the %s = %d %s, want 404", name, route, rec.Code, rec.Body.String())
				}
				if n := r.handlersOn(t, r.absorbed); n != 0 {
					t.Errorf("handlers on the retired blueprint = %d, want 0", n)
				}
			})

			t.Run(name+"_locks_first/"+route, func(t *testing.T) {
				r := newAbsorbRig(t, nil)
				write := prepare(t, r)

				// The write, held after its blueprint lock and before it
				// writes the handler.
				r.beforeWrite.armed.Store(true)
				written := write()
				r.beforeWrite.waitReached(t, "the "+name+"'s write")

				absorbed := r.absorb(route)
				waitForLockWait(t, r.h, "the "+route)

				r.beforeWrite.free()
				if rec := receiveResponse(t, "the "+name, written); rec.Code != wrote[name] {
					t.Fatalf("%s = %d %s, want %d", name, rec.Code, rec.Body.String(), wrote[name])
				}
				rec := receiveResponse(t, "the "+route, absorbed)
				if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), httpx.ReasonConflict) {
					t.Errorf("%s behind the %s = %d %s, want 422 %s", route, name, rec.Code, rec.Body.String(), httpx.ReasonConflict)
				}
				// The refused absorb left both blueprints as they were.
				if steps, live := r.stepsOf(t, r.absorbed); steps != 1 || !live {
					t.Errorf("absorbed blueprint = (%d steps, live %v), want its 1 step, live", steps, live)
				}
				if steps, live := r.stepsOf(t, r.host); steps != 2 || !live {
					t.Errorf("host = (%d steps, live %v), want its 2 steps, live", steps, live)
				}
				if n := r.handlersOn(t, r.absorbed); n != 1 {
					t.Errorf("handlers on the blueprint = %d, want the %s's", n, name)
				}
			})
		}
	}
}

// staleHandlerReadTx runs then, once, right after the first read-only
// transaction that reads the event handler id ends: what concurrent requests
// would do between a PATCH's read of the handler and its write.
type staleHandlerReadTx struct {
	db.TxRunner
	id   string
	then *func(ctx context.Context) error
	ran  *atomic.Bool
	err  *error
}

func (w staleHandlerReadTx) WithReadTx(ctx context.Context, orgID, userID string, fn func(db.TxStores) error) error {
	var read bool
	err := w.TxRunner.WithReadTx(ctx, orgID, userID, func(tx db.TxStores) error {
		tx.EventHandlers = readWatchHandlers{EventHandlerStore: tx.EventHandlers, id: w.id, read: &read}
		return fn(tx)
	})
	if err == nil && read && *w.then != nil && w.ran.CompareAndSwap(false, true) {
		*w.err = (*w.then)(ctx)
	}
	return err
}

type readWatchHandlers struct {
	db.EventHandlerStore
	id   string
	read *bool
}

func (h readWatchHandlers) Get(ctx context.Context, orgID, id string) (*domain.EventHandler, error) {
	got, err := h.EventHandlerStore.Get(ctx, orgID, id)
	if id == h.id && got != nil {
		*h.read = true
	}
	return got, err
}

// TestEventHandlers_StalePatchRacesBlueprintMerge_Postgres: a PATCH whose
// read of the handler still shows a blueprint, after another request took
// the handler off it and while a merge retiring that blueprint is in flight,
// waits for the merge and is refused, rather than putting the handler back
// on the blueprint the merge retires.
func TestEventHandlers_StalePatchRacesBlueprintMerge_Postgres(t *testing.T) {
	handlerID := uuid.NewString()
	var (
		then func(ctx context.Context) error
		ran  atomic.Bool
		err  error
	)
	r := newAbsorbRig(t, func(tx db.TxRunner) db.TxRunner {
		return staleHandlerReadTx{TxRunner: tx, id: handlerID, then: &then, ran: &ran, err: &err}
	})
	if e := r.s.tx.WithTx(t.Context(), r.orgID, r.member, func(tx db.TxStores) error {
		_, e := tx.EventHandlers.Create(t.Context(), r.orgID, r.teamID, domain.EventHandler{
			ID: handlerID, Mode: domain.EventHandlerModeWatch, EventType: domain.EventGitHubPROpened, Name: "on the absorbed",
			DefaultPriority: 0.5, BreakerThreshold: 4, BlueprintID: r.absorbed,
		})
		return e
	}); e != nil {
		t.Fatalf("seed the handler: %v", e)
	}

	var merged <-chan *httptest.ResponseRecorder
	then = func(ctx context.Context) error {
		// Another request takes the handler off the blueprint...
		if e := r.s.tx.WithTx(ctx, r.orgID, r.member, func(tx db.TxStores) error {
			h, e := tx.EventHandlers.Get(ctx, r.orgID, handlerID)
			if e != nil || h == nil {
				return fmt.Errorf("read the handler: %v", e)
			}
			h.BlueprintID = ""
			_, e = tx.EventHandlers.Update(ctx, r.orgID, *h)
			return e
		}); e != nil {
			return fmt.Errorf("detach the handler: %w", e)
		}
		// ...and a merge holding it is held after its check found no
		// handler.
		r.afterList.armed.Store(true)
		merged = r.absorb("merge")
		select {
		case <-r.afterList.reached:
			return nil
		case <-time.After(15 * time.Second):
			return errors.New("the merge never reached its check")
		}
	}

	req := r.req(http.MethodPatch, "/api/event-handlers/"+handlerID, r.member, map[string]any{"name": "renamed"})
	req.SetPathValue("id", handlerID)
	patched := serveAsync(r.eh.handleEventHandlerUpdate, req)
	waitForLockWait(t, r.h, "the patch")
	if !ran.Load() || err != nil {
		t.Fatalf("the requests between the patch's read and its write: ran %v, %v", ran.Load(), err)
	}

	r.afterList.free()
	if rec := receiveResponse(t, "the merge", merged); rec.Code != http.StatusOK {
		t.Fatalf("merge = %d %s, want 200", rec.Code, rec.Body.String())
	}
	rec := receiveResponse(t, "the patch", patched)
	if rec.Code != http.StatusConflict {
		t.Errorf("stale patch behind the merge = %d %s, want 409", rec.Code, rec.Body.String())
	}
	if n := r.handlersOn(t, r.absorbed); n != 0 {
		t.Errorf("handlers on the retired blueprint = %d, want 0", n)
	}
}
