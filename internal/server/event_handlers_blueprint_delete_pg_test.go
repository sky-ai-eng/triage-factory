package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/blueprints"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/prompts"
)

// blueprintRaceRig is a viewerRig whose member drives the handler, blueprint
// and prompt routes through one mux, with the claims and active org
// withSession would supply.
type blueprintRaceRig struct {
	*viewerRig
	mux *http.ServeMux
}

func newBlueprintRaceRig(t *testing.T) *blueprintRaceRig {
	t.Helper()
	r := newViewerRig(t)
	r.bindGitHubPAT(t)
	mux := http.NewServeMux()
	mount := func(pattern string, h http.HandlerFunc) { mux.HandleFunc(pattern, h) }
	m := httpx.Mounts{API: mount, APIMutating: mount}
	blueprints.New(r.s.tx, r.s.az).Register(m)
	prompts.New(r.s.db, r.s.tx, r.s.az).Register(m)
	mux.HandleFunc("POST /api/event-handlers", r.eh.handleEventHandlerCreate)
	mux.HandleFunc("PATCH /api/event-handlers/{id}", r.eh.handleEventHandlerUpdate)
	return &blueprintRaceRig{viewerRig: r, mux: mux}
}

// do sends one request as the member.
func (r *blueprintRaceRig) do(method, path string, body any) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, r.req(method, path, r.member, body))
	return rec
}

// mustCreate sends a create the fixture needs and returns the id it answers,
// under key.
func (r *blueprintRaceRig) mustCreate(t *testing.T, path string, body any, key string) string {
	t.Helper()
	rec := r.do(http.MethodPost, path, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST %s = %d: %s", path, rec.Code, rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	id, _ := got[key].(string)
	if id == "" {
		t.Fatalf("POST %s answered no %s: %s", path, key, rec.Body.String())
	}
	return id
}

// wrappedBlueprint creates a blueprint whose one step is a new prompt and
// returns both ids.
func (r *blueprintRaceRig) wrappedBlueprint(t *testing.T, name string) (blueprintID, promptID string) {
	t.Helper()
	rec := r.do(http.MethodPost, "/api/blueprints", map[string]any{
		"first_prompt": map[string]any{"name": name, "body": "do " + name},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create blueprint %s = %d: %s", name, rec.Code, rec.Body.String())
	}
	var created struct {
		ID            string `json:"id"`
		FirstPromptID string `json:"first_prompt_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	return created.ID, created.FirstPromptID
}

// liveHandlerBlueprint reads a live handler's blueprint and mode off the admin
// pool.
func (r *blueprintRaceRig) liveHandlerBlueprint(t *testing.T, id string) (blueprintID *string, mode string) {
	t.Helper()
	if err := r.h.AdminDB.QueryRow(
		`SELECT blueprint_id, mode FROM event_handlers WHERE id = $1 AND deleted_at IS NULL`, id,
	).Scan(&blueprintID, &mode); err != nil {
		t.Fatalf("read handler %s: %v", id, err)
	}
	return blueprintID, mode
}

// deref renders a nullable column for a failure message.
func deref(s *string) string {
	if s == nil {
		return "<null>"
	}
	return *s
}

// handlersOn counts the live handlers that reference blueprintID.
func (r *blueprintRaceRig) handlersOn(t *testing.T, blueprintID string) int {
	t.Helper()
	var n int
	if err := r.h.AdminDB.QueryRow(
		`SELECT count(*) FROM event_handlers WHERE blueprint_id = $1 AND deleted_at IS NULL`, blueprintID,
	).Scan(&n); err != nil {
		t.Fatalf("count handlers on %s: %v", blueprintID, err)
	}
	return n
}

// awaitAnswer waits for a request started in the background, which has to be
// waiting on a lock until release lets the holding transaction commit.
func (r *blueprintRaceRig) awaitAnswer(t *testing.T, answered <-chan *httptest.ResponseRecorder, release chan<- struct{}, holder <-chan error) *httptest.ResponseRecorder {
	t.Helper()
	waitForLockWaiter(t, r.h.AdminDB)
	select {
	case rec := <-answered:
		t.Fatalf("the request answered %d while the other transaction held the blueprint: %s", rec.Code, rec.Body.String())
	default:
	}
	close(release)
	if err := <-holder; err != nil {
		t.Fatalf("the holding transaction: %v", err)
	}
	select {
	case rec := <-answered:
		return rec
	case <-time.After(10 * time.Second):
		t.Fatal("the request never answered after the holding transaction committed")
		return nil
	}
}

// A handler write that names a blueprint whose delete has run its detach but
// not committed waits for the delete, and then answers 404: the route's first
// read sees the blueprint live, and the write's own read of it waits on the
// lock the delete took before its detach.
func TestEventHandlerWrite_Postgres_WaitsForABlueprintDelete(t *testing.T) {
	for _, tc := range []struct {
		name string
		// setup makes the handler a PATCH writes, if any, and write sends the
		// handler write naming blueprintID.
		setup func(t *testing.T, r *blueprintRaceRig) (handlerID string)
		write func(r *blueprintRaceRig, handlerID, blueprintID string) *httptest.ResponseRecorder
	}{
		{"create", func(*testing.T, *blueprintRaceRig) string { return "" },
			func(r *blueprintRaceRig, _, blueprintID string) *httptest.ResponseRecorder {
				return r.do(http.MethodPost, "/api/event-handlers", map[string]any{
					"event_type": domain.EventGitHubPROpened, "blueprint_id": blueprintID,
				})
			}},
		{"patch_attaching_it", func(t *testing.T, r *blueprintRaceRig) string {
			return r.mustCreate(t, "/api/event-handlers", map[string]any{"event_type": domain.EventGitHubPROpened}, "id")
		}, func(r *blueprintRaceRig, handlerID, blueprintID string) *httptest.ResponseRecorder {
			return r.do(http.MethodPatch, "/api/event-handlers/"+handlerID, map[string]any{"blueprint_id": blueprintID})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newBlueprintRaceRig(t)
			ctx := t.Context()
			handlerID := tc.setup(t, r)
			bp, _ := r.wrappedBlueprint(t, "DeletedWhileWritten")

			// The delete, held open after its detach.
			detached, release := make(chan struct{}), make(chan struct{})
			deleted := make(chan error, 1)
			go func() {
				deleted <- r.s.tx.WithTx(ctx, r.orgID, r.member, func(tx db.TxStores) error {
					if b, err := tx.Blueprints.GetForUpdate(ctx, r.orgID, bp); err != nil || b == nil {
						t.Errorf("the delete's hold = %+v, %v; want the live blueprint", b, err)
						close(detached)
						return err
					}
					if err := tx.EventHandlers.DetachBlueprint(ctx, r.orgID, bp); err != nil {
						return err
					}
					close(detached)
					<-release
					return tx.Blueprints.Delete(ctx, r.orgID, bp)
				})
			}()
			<-detached

			answered := make(chan *httptest.ResponseRecorder, 1)
			go func() { answered <- tc.write(r, handlerID, bp) }()
			rec := r.awaitAnswer(t, answered, release, deleted)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("handler write after the delete = %d: %s; want 404", rec.Code, rec.Body.String())
			}
			if n := r.handlersOn(t, bp); n != 0 {
				t.Errorf("%d live handlers on the deleted blueprint; want none", n)
			}
		})
	}
}

// Each blueprint delete path holds the blueprint before it detaches its
// handlers, so a handler written by a transaction that held the blueprint
// first is detached by the delete, which waits for it: it is left live, with
// no blueprint and out of automate.
func TestBlueprintDelete_Postgres_DetachesAHandlerWrittenWhileItWaited(t *testing.T) {
	for _, tc := range []struct {
		name string
		// setup makes the blueprint and returns the delete that retires it.
		setup func(t *testing.T, r *blueprintRaceRig) (blueprintID, deletePath string)
	}{
		{"blueprint_delete", func(t *testing.T, r *blueprintRaceRig) (string, string) {
			bp, _ := r.wrappedBlueprint(t, "Deleted")
			return bp, "/api/blueprints/" + bp
		}},
		{"sole_step_prompt_delete", func(t *testing.T, r *blueprintRaceRig) (string, string) {
			bp, p := r.wrappedBlueprint(t, "Wrapper")
			return bp, "/api/prompts/" + p
		}},
		{"head_prompt_delete", func(t *testing.T, r *blueprintRaceRig) (string, string) {
			bp := r.mustCreate(t, "/api/blueprints", map[string]any{"name": "Chain"}, "id")
			head := r.mustCreate(t, "/api/prompts", map[string]any{"name": "head", "body": "b"}, "id")
			tail := r.mustCreate(t, "/api/prompts", map[string]any{"name": "tail", "body": "b"}, "id")
			if rec := r.do(http.MethodPut, "/api/blueprints/"+bp+"/steps", map[string]any{
				"steps": []map[string]any{{"step_prompt_id": head}, {"step_prompt_id": tail}},
			}); rec.Code != http.StatusNoContent {
				t.Fatalf("PUT steps = %d: %s", rec.Code, rec.Body.String())
			}
			return bp, "/api/prompts/" + head
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newBlueprintRaceRig(t)
			ctx := t.Context()
			bp, deletePath := tc.setup(t, r)

			// A handler write that holds the blueprint, writing its handler
			// only once the delete is waiting.
			handlerID := uuid.NewString()
			held, release := make(chan struct{}), make(chan struct{})
			written := make(chan error, 1)
			go func() {
				written <- r.s.tx.WithTx(ctx, r.orgID, r.member, func(tx db.TxStores) error {
					if b, err := tx.Blueprints.GetForShare(ctx, r.orgID, bp); err != nil || b == nil {
						t.Errorf("the handler write's hold = %+v, %v; want the live blueprint", b, err)
						close(held)
						return err
					}
					close(held)
					<-release
					_, err := tx.EventHandlers.Create(ctx, r.orgID, r.teamID, domain.EventHandler{
						ID: handlerID, Mode: domain.EventHandlerModeAutomate, EventType: domain.EventGitHubPROpened,
						Source: domain.EventHandlerSourceUser, Name: "written while the delete waited",
						DefaultPriority: 0.5, BlueprintID: bp, BreakerThreshold: 4,
					})
					return err
				})
			}()
			<-held

			answered := make(chan *httptest.ResponseRecorder, 1)
			go func() { answered <- r.do(http.MethodDelete, deletePath, nil) }()
			rec := r.awaitAnswer(t, answered, release, written)
			if rec.Code != http.StatusOK {
				t.Fatalf("DELETE %s = %d: %s; want 200", deletePath, rec.Code, rec.Body.String())
			}
			if got, mode := r.liveHandlerBlueprint(t, handlerID); got != nil || mode != domain.EventHandlerModeWatch {
				t.Errorf("handler after the delete: blueprint %s, mode %s; want no blueprint, in watch", deref(got), mode)
			}
		})
	}
}

// A prompt delete whose blueprint is retired by another write while the delete
// waits to hold it answers 409 and changes nothing: it was composed against a
// chain that no longer exists.
func TestPromptDelete_Postgres_ConflictsWhenItsBlueprintIsRetiredWhileItWaits(t *testing.T) {
	r := newBlueprintRaceRig(t)
	ctx := t.Context()
	bp, p := r.wrappedBlueprint(t, "RetiredElsewhere")

	held, release := make(chan struct{}), make(chan struct{})
	retired := make(chan error, 1)
	go func() {
		retired <- r.s.tx.WithTx(ctx, r.orgID, r.member, func(tx db.TxStores) error {
			if b, err := tx.Blueprints.GetForUpdate(ctx, r.orgID, bp); err != nil || b == nil {
				t.Errorf("the retire's hold = %+v, %v; want the live blueprint", b, err)
				close(held)
				return err
			}
			if err := tx.Blueprints.Delete(ctx, r.orgID, bp); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- r.do(http.MethodDelete, "/api/prompts/"+p, nil) }()
	rec := r.awaitAnswer(t, answered, release, retired)
	if rec.Code != http.StatusConflict {
		t.Fatalf("DELETE prompt after its blueprint was retired = %d: %s; want 409", rec.Code, rec.Body.String())
	}
	var deletedAt *time.Time
	if err := r.h.AdminDB.QueryRow(`SELECT deleted_at FROM prompts WHERE id = $1`, p).Scan(&deletedAt); err != nil {
		t.Fatalf("read prompt: %v", err)
	}
	if deletedAt != nil {
		t.Errorf("prompt deleted_at = %v after the refused delete; want it untouched", deletedAt)
	}
}
