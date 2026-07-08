package server

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestProjectPatch_ConcurrentAutosave_NoLostUpdate is a full-handler
// sanity check alongside the deterministic lock-primitive proof in
// advisorylock_test.go (TestAcquireKeyedLock_Multi_SerializesSameKey,
// which directly demonstrates a second acquire blocks until the first
// releases). Two concurrent PATCHes against the SAME project, each
// touching a different field, are fired at handleProjectUpdate through
// httptest — a real Postgres advisory lock is live (multi mode) — and both
// fields must survive. This exercises the real read-merge-write handler
// end to end (acquireKeyedLock wired at the right scope, the happy path
// still works under concurrent load) but note it is NOT by itself a
// reliable adversarial reproduction of the pre-fix bug: two in-process
// goroutines racing a single local Postgres round-trip rarely land the
// exact read/write interleaving a lost update needs, with or without the
// lock, so removing the fix does not reliably fail this test. The
// lock-primitive test is the authoritative regression proof; this is the
// integration-level complement.
func TestProjectPatch_ConcurrentAutosave_NoLostUpdate(t *testing.T) {
	r := newProjectVisibilityRig(t)
	_, created := r.create(t, r.admin, map[string]any{"name": "original name"})

	const rounds = 10
	for i := 0; i < rounds; i++ {
		var wg sync.WaitGroup
		var recName, recDesc *httptestRecorderResult
		wg.Add(2)
		go func() {
			defer wg.Done()
			rec := r.patch(t, r.admin, created.ID, map[string]any{"name": "concurrent-name"})
			recName = &httptestRecorderResult{code: rec.Code, body: rec.Body.String()}
		}()
		go func() {
			defer wg.Done()
			rec := r.patch(t, r.admin, created.ID, map[string]any{"description": "concurrent-description"})
			recDesc = &httptestRecorderResult{code: rec.Code, body: rec.Body.String()}
		}()
		wg.Wait()

		if recName.code != http.StatusOK {
			t.Fatalf("round %d: name PATCH status = %d, body=%s", i, recName.code, recName.body)
		}
		if recDesc.code != http.StatusOK {
			t.Fatalf("round %d: description PATCH status = %d, body=%s", i, recDesc.code, recDesc.body)
		}

		var got domain.Project
		rec := r.patch(t, r.admin, created.ID, map[string]any{})
		if rec.Code != http.StatusOK {
			t.Fatalf("round %d: read-back PATCH status = %d, body=%s", i, rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("round %d: decode: %v", i, err)
		}
		if got.Name != "concurrent-name" {
			t.Errorf("round %d: name = %q, want %q (lost update)", i, got.Name, "concurrent-name")
		}
		if got.Description != "concurrent-description" {
			t.Errorf("round %d: description = %q, want %q (lost update)", i, got.Description, "concurrent-description")
		}
	}
}

type httptestRecorderResult struct {
	code int
	body string
}
