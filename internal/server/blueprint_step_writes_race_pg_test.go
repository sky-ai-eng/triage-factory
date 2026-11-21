package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/blueprints"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/prompts"
)

// pausingBlueprintsTx pauses a write transaction right after it takes a hold
// on blueprints, GetForUpdate or GetAllForUpdate, with the hold taken, or
// right after ReplaceSteps, with the step rows written and not committed.
type pausingBlueprintsTx struct {
	db.TxRunner
	afterHold, afterReplace *pausePoint
}

func (w pausingBlueprintsTx) WithTx(ctx context.Context, orgID, userID string, fn func(db.TxStores) error) error {
	return w.TxRunner.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		tx.Blueprints = pausingBlueprints{BlueprintStore: tx.Blueprints, afterHold: w.afterHold, afterReplace: w.afterReplace}
		return fn(tx)
	})
}

type pausingBlueprints struct {
	db.BlueprintStore
	afterHold, afterReplace *pausePoint
}

func (b pausingBlueprints) GetForUpdate(ctx context.Context, orgID, id string) (*domain.Blueprint, error) {
	out, err := b.BlueprintStore.GetForUpdate(ctx, orgID, id)
	b.afterHold.hold()
	return out, err
}

func (b pausingBlueprints) GetAllForUpdate(ctx context.Context, orgID string, ids []string) (map[string]*domain.Blueprint, error) {
	out, err := b.BlueprintStore.GetAllForUpdate(ctx, orgID, ids)
	b.afterHold.hold()
	return out, err
}

func (b pausingBlueprints) ReplaceSteps(ctx context.Context, orgID, blueprintID string, stepPromptIDs, briefs []string) (domain.Blueprint, error) {
	out, err := b.BlueprintStore.ReplaceSteps(ctx, orgID, blueprintID, stepPromptIDs, briefs)
	b.afterReplace.hold()
	return out, err
}

// stepWriteRig is a Postgres server whose write transactions can be held
// right after their blueprint hold or right after ReplaceSteps, with the
// blueprint and prompt routes on one mux, sent as the rig's admin.
type stepWriteRig struct {
	*viewerRig
	mux                     *http.ServeMux
	afterHold, afterReplace *pausePoint
}

func newStepWriteRig(t *testing.T) stepWriteRig {
	t.Helper()
	afterHold, afterReplace := newPausePoint(t), newPausePoint(t)
	r := newViewerRigWithTx(t, func(tx db.TxRunner) db.TxRunner {
		return pausingBlueprintsTx{TxRunner: tx, afterHold: afterHold, afterReplace: afterReplace}
	})
	mux := http.NewServeMux()
	mount := func(pattern string, h http.HandlerFunc) { mux.HandleFunc(pattern, h) }
	m := httpx.Mounts{API: mount, APIMutating: mount}
	blueprints.New(r.s.tx, r.s.az).Register(m)
	prompts.New(r.s.db, r.s.tx, r.s.az).Register(m)
	return stepWriteRig{viewerRig: r, mux: mux, afterHold: afterHold, afterReplace: afterReplace}
}

func (r stepWriteRig) send(method, path string, body any) <-chan *httptest.ResponseRecorder {
	return serveAsync(r.mux.ServeHTTP, r.req(method, path, r.admin, body))
}

func (r stepWriteRig) merge(host, source string) <-chan *httptest.ResponseRecorder {
	return r.send(http.MethodPost, "/api/blueprints/"+host+"/merge", map[string]any{"source_blueprint_id": source})
}

// putSteps sends a steps PUT naming version, with brief on every step.
func (r stepWriteRig) putSteps(id string, version int64, brief string, prompts []string) <-chan *httptest.ResponseRecorder {
	steps := make([]map[string]any, len(prompts))
	for i, p := range prompts {
		steps[i] = map[string]any{"step_prompt_id": p, "brief": brief}
	}
	return r.send(http.MethodPut, "/api/blueprints/"+id+"/steps", map[string]any{"steps": steps, "version": version})
}

// firstHolds sends first, holds it right after its blueprint hold, sends
// second, and frees first once second waits on a lock. It hands back both
// responses.
func (r stepWriteRig) firstHolds(t *testing.T, what [2]string, first, second func() <-chan *httptest.ResponseRecorder) (*httptest.ResponseRecorder, *httptest.ResponseRecorder) {
	t.Helper()
	return r.firstHoldsAt(t, r.afterHold, what, first, second)
}

// firstHoldsAt is firstHolds with first held at pause.
func (r stepWriteRig) firstHoldsAt(t *testing.T, pause *pausePoint, what [2]string, first, second func() <-chan *httptest.ResponseRecorder) (*httptest.ResponseRecorder, *httptest.ResponseRecorder) {
	t.Helper()
	pause.armed.Store(true)
	held := first()
	pause.waitReached(t, "the "+what[0])
	waiting := second()
	waitForLockWait(t, r.h, "the "+what[1])
	pause.free()
	return receiveResponse(t, "the "+what[0], held), receiveResponse(t, "the "+what[1], waiting)
}

// stepPrompts reads a blueprint's step prompts in step order, past RLS.
func (r *viewerRig) stepPrompts(t *testing.T, blueprintID string) []string {
	t.Helper()
	rows, err := r.h.AdminDB.Query(
		`SELECT step_prompt_id FROM blueprint_steps WHERE blueprint_id = $1 ORDER BY step_index`, blueprintID)
	if err != nil {
		t.Fatalf("read the steps of %s: %v", blueprintID, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan a step of %s: %v", blueprintID, err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the steps of %s: %v", blueprintID, err)
	}
	return out
}

// version reads a blueprint's version, past RLS.
func (r *viewerRig) version(t *testing.T, blueprintID string) int64 {
	t.Helper()
	var v int64
	if err := r.h.AdminDB.QueryRow(`SELECT version FROM blueprints WHERE id = $1`, blueprintID).Scan(&v); err != nil {
		t.Fatalf("read the version of %s: %v", blueprintID, err)
	}
	return v
}

// briefs reads a blueprint's step briefs in step order, past RLS.
func (r *viewerRig) briefs(t *testing.T, blueprintID string) []string {
	t.Helper()
	rows, err := r.h.AdminDB.Query(
		`SELECT brief FROM blueprint_steps WHERE blueprint_id = $1 ORDER BY step_index`, blueprintID)
	if err != nil {
		t.Fatalf("read the briefs of %s: %v", blueprintID, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			t.Fatalf("scan a brief of %s: %v", blueprintID, err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the briefs of %s: %v", blueprintID, err)
	}
	return out
}

// wantRefusal fails unless rec is a 409 with one error of reason on field.
func wantRefusal(t *testing.T, what string, rec *httptest.ResponseRecorder, reason, field string) {
	t.Helper()
	if rec.Code != http.StatusConflict {
		t.Errorf("%s = %d %s, want 409", what, rec.Code, rec.Body.String())
		return
	}
	if items := decodeErrorItems(t, rec); len(items) != 1 || items[0].Reason != reason || items[0].Field != field {
		t.Errorf("%s: errors = %+v; want one %s on %s", what, items, reason, field)
	}
}

// wantSteps fails unless blueprintID is live as wanted and holds want, in
// order.
func (r *viewerRig) wantSteps(t *testing.T, what, blueprintID string, live bool, want []string) {
	t.Helper()
	if _, isLive := r.stepsOf(t, blueprintID); isLive != live {
		t.Errorf("%s live = %v, want %v", what, isLive, live)
	}
	if got := r.stepPrompts(t, blueprintID); !slices.Equal(got, want) {
		t.Errorf("%s steps = %v, want %v", what, got, want)
	}
}

func concat(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// TestBlueprints_StepWritesRaceAMergeThatRetiresTheirBlueprint_Postgres runs
// each write that changes the steps of a blueprint A against a merge of A
// into X, in both orders. Every such write holds A before it reads or moves a
// step, so the two run one after the other: a write behind the merge finds A
// retired and answers 404, and a merge behind the write absorbs what the
// write left on A. Steps never end up on the retired A: B's steps stay on B,
// or reach X through A.
func TestBlueprints_StepWritesRaceAMergeThatRetiresTheirBlueprint_Postgres(t *testing.T) {
	type write struct {
		// send sends the write on A. B is the blueprint a merge or a
		// reconnect absorbs into A.
		send func(r stepWriteRig, a, b string, aSteps []string, aVersion int64) <-chan *httptest.ResponseRecorder
		ok   int
		// after is A's steps once the write commits.
		after   func(aSteps, bSteps []string) []string
		absorbs bool
	}
	writes := map[string]write{
		"merge": {
			send: func(r stepWriteRig, a, b string, _ []string, _ int64) <-chan *httptest.ResponseRecorder {
				return r.merge(a, b)
			},
			ok:      http.StatusOK,
			after:   func(a, b []string) []string { return concat(a, b) },
			absorbs: true,
		},
		"reconnect": {
			send: func(r stepWriteRig, a, b string, _ []string, _ int64) <-chan *httptest.ResponseRecorder {
				return r.send(http.MethodPost, "/api/blueprints/"+a+"/reconnect", map[string]any{"at_step_index": 1, "target_blueprint_id": b})
			},
			ok:      http.StatusOK,
			after:   func(a, b []string) []string { return concat(a[:1], b) },
			absorbs: true,
		},
		"split": {
			send: func(r stepWriteRig, a, _ string, _ []string, _ int64) <-chan *httptest.ResponseRecorder {
				return r.send(http.MethodPost, "/api/blueprints/"+a+"/split", map[string]any{"at_step_index": 1})
			},
			ok:    http.StatusOK,
			after: func(a, _ []string) []string { return a[:1] },
		},
		"steps_put": {
			send: func(r stepWriteRig, a, _ string, aSteps []string, aVersion int64) <-chan *httptest.ResponseRecorder {
				return r.putSteps(a, aVersion, "edited", []string{aSteps[1], aSteps[0]})
			},
			ok:    http.StatusOK,
			after: func(a, _ []string) []string { return []string{a[1], a[0]} },
		},
	}

	for name, w := range writes {
		setup := func(t *testing.T) (r stepWriteRig, x, a, b string, xSteps, aSteps, bSteps []string) {
			r = newStepWriteRig(t)
			x, a, b = r.seedChain(t, "x", 1), r.seedChain(t, "a", 2), r.seedChain(t, "b", 1)
			return r, x, a, b, r.stepPrompts(t, x), r.stepPrompts(t, a), r.stepPrompts(t, b)
		}

		t.Run("merge_of_A_holds_first/"+name, func(t *testing.T) {
			r, x, a, b, xSteps, aSteps, bSteps := setup(t)
			merged, written := r.firstHolds(t, [2]string{"merge of A into X", name + " on A"},
				func() <-chan *httptest.ResponseRecorder { return r.merge(x, a) },
				func() <-chan *httptest.ResponseRecorder { return w.send(r, a, b, aSteps, r.version(t, a)) })
			if merged.Code != http.StatusOK {
				t.Fatalf("merge of A into X = %d %s, want 200", merged.Code, merged.Body.String())
			}
			if written.Code != http.StatusNotFound {
				t.Errorf("%s on A behind the merge that retires it = %d %s, want 404", name, written.Code, written.Body.String())
			}
			r.wantSteps(t, "X", x, true, concat(xSteps, aSteps))
			r.wantSteps(t, "A", a, false, nil)
			if w.absorbs {
				r.wantSteps(t, "B", b, true, bSteps)
			}
		})

		t.Run(name+"_holds_first", func(t *testing.T) {
			r, x, a, b, xSteps, aSteps, bSteps := setup(t)
			written, merged := r.firstHolds(t, [2]string{name + " on A", "merge of A into X"},
				func() <-chan *httptest.ResponseRecorder { return w.send(r, a, b, aSteps, r.version(t, a)) },
				func() <-chan *httptest.ResponseRecorder { return r.merge(x, a) })
			if written.Code != w.ok {
				t.Fatalf("%s on A = %d %s, want %d", name, written.Code, written.Body.String(), w.ok)
			}
			if merged.Code != http.StatusOK {
				t.Errorf("merge of A into X behind the %s = %d %s, want 200", name, merged.Code, merged.Body.String())
			}
			r.wantSteps(t, "X", x, true, concat(xSteps, w.after(aSteps, bSteps)))
			r.wantSteps(t, "A", a, false, nil)
			if w.absorbs {
				r.wantSteps(t, "B", b, false, nil)
			}
		})
	}
}

// TestBlueprints_StepsPutRacesAMergeIntoItsBlueprint_Postgres runs a steps
// PUT on X, sending the list and the version read before the merge, against a
// merge of A into X, in both orders. The merge holds X before it moves a step
// and the PUT holds X before it reads or writes one, so neither can hold a
// step row while it waits for X's row, and the two do not deadlock. A PUT
// ahead of the merge rewrites the list it was read from, and the merge
// appends A's steps to it. A PUT behind the merge finds X past the version
// its list was read at, and is refused, so the merged steps stay on X.
func TestBlueprints_StepsPutRacesAMergeIntoItsBlueprint_Postgres(t *testing.T) {
	setup := func(t *testing.T) (r stepWriteRig, x, a string, xSteps, aSteps []string, xVersion int64) {
		r = newStepWriteRig(t)
		x, a = r.seedChain(t, "x", 1), r.seedChain(t, "a", 2)
		return r, x, a, r.stepPrompts(t, x), r.stepPrompts(t, a), r.version(t, x)
	}

	t.Run("put_holds_first", func(t *testing.T) {
		r, x, a, xSteps, aSteps, xVersion := setup(t)
		put, merged := r.firstHolds(t, [2]string{"steps PUT on X", "merge of A into X"},
			func() <-chan *httptest.ResponseRecorder { return r.putSteps(x, xVersion, "edited", xSteps) },
			func() <-chan *httptest.ResponseRecorder { return r.merge(x, a) })
		if put.Code != http.StatusOK {
			t.Fatalf("steps PUT = %d %s, want 200", put.Code, put.Body.String())
		}
		if merged.Code != http.StatusOK {
			t.Errorf("merge behind the PUT = %d %s, want 200", merged.Code, merged.Body.String())
		}
		r.wantSteps(t, "X", x, true, concat(xSteps, aSteps))
		r.wantSteps(t, "A", a, false, nil)
	})

	t.Run("merge_holds_first", func(t *testing.T) {
		r, x, a, xSteps, aSteps, xVersion := setup(t)
		merged, put := r.firstHolds(t, [2]string{"merge of A into X", "steps PUT on X"},
			func() <-chan *httptest.ResponseRecorder { return r.merge(x, a) },
			func() <-chan *httptest.ResponseRecorder { return r.putSteps(x, xVersion, "edited", xSteps) })
		if merged.Code != http.StatusOK {
			t.Fatalf("merge = %d %s, want 200", merged.Code, merged.Body.String())
		}
		wantRefusal(t, "steps PUT sent X's steps from before the merge, behind it", put, httpx.ReasonVersionConflict, "version")
		r.wantSteps(t, "X", x, true, concat(xSteps, aSteps))
		r.wantSteps(t, "A", a, false, nil)
	})
}

// TestBlueprints_StepsPutRacesAPromptDelete_Postgres runs a steps PUT on a
// blueprint against a delete of one of its middle prompts, in both orders.
// The delete holds the blueprint before it moves a step and so does the PUT,
// so the two do not deadlock: a PUT behind the delete finds the blueprint
// past the version it read and answers 409, and a delete behind the PUT
// deletes from the list the PUT wrote.
func TestBlueprints_StepsPutRacesAPromptDelete_Postgres(t *testing.T) {
	setup := func(t *testing.T) (r stepWriteRig, a string, aSteps []string, aVersion int64) {
		r = newStepWriteRig(t)
		a = r.seedChain(t, "a", 3)
		return r, a, r.stepPrompts(t, a), r.version(t, a)
	}
	deletePrompt := func(r stepWriteRig, id string) func() <-chan *httptest.ResponseRecorder {
		return func() <-chan *httptest.ResponseRecorder {
			return r.send(http.MethodDelete, "/api/prompts/"+id, nil)
		}
	}

	t.Run("put_holds_first", func(t *testing.T) {
		r, a, aSteps, aVersion := setup(t)
		put, deleted := r.firstHolds(t, [2]string{"steps PUT", "prompt delete"},
			func() <-chan *httptest.ResponseRecorder { return r.putSteps(a, aVersion, "edited", aSteps) },
			deletePrompt(r, aSteps[1]))
		if put.Code != http.StatusOK {
			t.Fatalf("steps PUT = %d %s, want 200", put.Code, put.Body.String())
		}
		if deleted.Code != http.StatusOK {
			t.Errorf("prompt delete behind the PUT = %d %s, want 200", deleted.Code, deleted.Body.String())
		}
		r.wantSteps(t, "the blueprint", a, true, aSteps[:1])
	})

	t.Run("delete_holds_first", func(t *testing.T) {
		r, a, aSteps, aVersion := setup(t)
		deleted, put := r.firstHolds(t, [2]string{"prompt delete", "steps PUT"},
			deletePrompt(r, aSteps[1]),
			func() <-chan *httptest.ResponseRecorder { return r.putSteps(a, aVersion, "edited", aSteps) })
		if deleted.Code != http.StatusOK {
			t.Fatalf("prompt delete = %d %s, want 200", deleted.Code, deleted.Body.String())
		}
		wantRefusal(t, "steps PUT naming the deleted prompt behind the delete", put, httpx.ReasonVersionConflict, "version")
		r.wantSteps(t, "the blueprint", a, true, aSteps[:1])
	})
}

// TestBlueprints_StepsPutRacesAnotherStepsPut_Postgres runs two steps PUTs on
// one blueprint, both sent with the version and the steps read before either.
// They name the same steps, so only the version tells the second it was
// decided against steps the first changed: the second waits for the first's
// hold, finds the blueprint past the version it read, and is refused, so the
// first's order and briefs stay.
func TestBlueprints_StepsPutRacesAnotherStepsPut_Postgres(t *testing.T) {
	r := newStepWriteRig(t)
	a := r.seedChain(t, "a", 2)
	aSteps, aVersion := r.stepPrompts(t, a), r.version(t, a)
	reordered := []string{aSteps[1], aSteps[0]}

	first, second := r.firstHolds(t, [2]string{"first steps PUT", "second steps PUT"},
		func() <-chan *httptest.ResponseRecorder { return r.putSteps(a, aVersion, "first", reordered) },
		func() <-chan *httptest.ResponseRecorder { return r.putSteps(a, aVersion, "second", aSteps) })
	if first.Code != http.StatusOK {
		t.Fatalf("first steps PUT = %d %s, want 200", first.Code, first.Body.String())
	}
	wantRefusal(t, "second steps PUT, sent the same version, behind the first", second, httpx.ReasonVersionConflict, "version")
	r.wantSteps(t, "the blueprint", a, true, reordered)
	if got := r.briefs(t, a); !slices.Equal(got, []string{"first", "first"}) {
		t.Errorf("briefs = %v, want the first PUT's", got)
	}
	if v := r.version(t, a); v != aVersion+1 {
		t.Errorf("version = %d, want %d: the first PUT's raise alone", v, aVersion+1)
	}
}

// TestBlueprints_StepsPutsAddingOnePromptToTwoBlueprints_Postgres runs two
// steps PUTs, on blueprints X and Y, that each add a prompt P belonging to no
// blueprint. Each PUT holds only its own blueprint, so both read P as
// unowned. The PUT on X is held after it writes its step rows and before it
// commits, and the PUT on Y waits on X's row for P at the unique index. Once
// X's PUT commits, the index refuses Y's insert, and Y's PUT answers 422
// CONFLICT on steps, as the check answers a prompt another blueprint holds.
// P is a step of X alone.
func TestBlueprints_StepsPutsAddingOnePromptToTwoBlueprints_Postgres(t *testing.T) {
	r := newStepWriteRig(t)
	x, y := r.seedChain(t, "x", 1), r.seedChain(t, "y", 1)
	xSteps, ySteps := r.stepPrompts(t, x), r.stepPrompts(t, y)
	xVersion, yVersion := r.version(t, x), r.version(t, y)
	p := uuid.NewString()
	if err := r.s.tx.WithTx(t.Context(), r.orgID, r.admin, func(tx db.TxStores) error {
		_, err := tx.Prompts.Create(t.Context(), r.orgID, r.teamID, domain.Prompt{ID: p, Name: "p", Body: "body", Source: "user"})
		return err
	}); err != nil {
		t.Fatalf("seed a prompt that belongs to no blueprint: %v", err)
	}

	onX, onY := r.firstHoldsAt(t, r.afterReplace, [2]string{"steps PUT on X", "steps PUT on Y"},
		func() <-chan *httptest.ResponseRecorder {
			return r.putSteps(x, xVersion, "x", concat(xSteps, []string{p}))
		},
		func() <-chan *httptest.ResponseRecorder {
			return r.putSteps(y, yVersion, "y", concat(ySteps, []string{p}))
		})
	if onX.Code != http.StatusOK {
		t.Fatalf("steps PUT on X = %d %s, want 200", onX.Code, onX.Body.String())
	}
	if onY.Code != http.StatusUnprocessableEntity {
		t.Errorf("steps PUT on Y behind X's = %d %s, want 422", onY.Code, onY.Body.String())
	} else if items := decodeErrorItems(t, onY); len(items) != 1 || items[0].Reason != httpx.ReasonConflict || items[0].Field != "steps" {
		t.Errorf("steps PUT on Y: errors = %+v; want one CONFLICT on steps", items)
	}
	r.wantSteps(t, "X", x, true, concat(xSteps, []string{p}))
	r.wantSteps(t, "Y", y, true, ySteps)
	if v := r.version(t, y); v != yVersion {
		t.Errorf("Y's version = %d, want %d: the refused PUT wrote nothing", v, yVersion)
	}
}

// TestBlueprints_Postgres_AbsorbingAnotherTeamsBlueprintIs422: a merge or a
// reconnect naming a blueprint of a team the caller may only read is refused
// with 422 CROSS_TEAM_REF, and changes neither blueprint. The route holds its
// two blueprints only after its same-team guard: under the hold's row-level
// security that blueprint would read as absent, and the route would answer
// 404 for a blueprint the caller can see.
func TestBlueprints_Postgres_AbsorbingAnotherTeamsBlueprintIs422(t *testing.T) {
	r := newStepWriteRig(t)
	ctx := t.Context()
	other := pgtest.SeedTeam(t, r.h, r.orgID, "other")
	pgtest.MustExec(t, r.h.AdminDB, `INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'admin')`, r.admin, other)
	pgtest.MustExec(t, r.h.AdminDB, `INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'viewer')`, r.member, other)
	host := r.seedChain(t, "host", 2)
	theirs := uuid.NewString()
	if err := r.s.tx.WithTx(ctx, r.orgID, r.admin, func(tx db.TxStores) error {
		_, err := tx.Blueprints.Create(ctx, r.orgID, other, domain.Blueprint{ID: theirs, Name: "theirs", Source: "user"})
		return err
	}); err != nil {
		t.Fatalf("seed the other team's blueprint: %v", err)
	}
	hostSteps := r.stepPrompts(t, host)

	for route, c := range map[string]struct {
		body  map[string]any
		field string
	}{
		"merge":     {map[string]any{"source_blueprint_id": theirs}, "source_blueprint_id"},
		"reconnect": {map[string]any{"at_step_index": 1, "target_blueprint_id": theirs}, "target_blueprint_id"},
	} {
		rec := httptest.NewRecorder()
		r.mux.ServeHTTP(rec, r.req(http.MethodPost, "/api/blueprints/"+host+"/"+route, r.member, c.body))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s naming another team's blueprint = %d: %s; want 422", route, rec.Code, rec.Body.String())
			continue
		}
		if items := decodeErrorItems(t, rec); len(items) != 1 || items[0].Reason != httpx.ReasonCrossTeamRef || items[0].Field != c.field {
			t.Errorf("%s: errors = %+v; want one CROSS_TEAM_REF on %s", route, items, c.field)
		}
		r.wantSteps(t, route+"'s host", host, true, hostSteps)
		r.wantSteps(t, route+"'s other-team blueprint", theirs, true, nil)
	}
}
