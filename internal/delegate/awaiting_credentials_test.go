package delegate

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/credseal"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestAwaitCredentials_NonExecutorRoleIsPassthrough pins TFAC-614's zero-
// behavior-change contract for every role except executor: no status
// mutation, no wait, the bundle-carrying ctx is unchanged.
func TestAwaitCredentials_NonExecutorRoleIsPassthrough(t *testing.T) {
	runmode.SetRoleForTest(t, runmode.RoleAll)
	database := newDelegateTestDB(t)
	seedRun(t, database, "run-passthrough", "sess", "/tmp/wt")
	stores := sqlitestore.New(database)
	s := NewSpawner(database, stores, nil, nil, "")

	run := &domain.AgentRun{ID: "run-passthrough", OrgID: runmode.LocalDefaultOrgID}
	ctx, ok := s.awaitCredentials(context.Background(), run.OrgID, run)
	if !ok {
		t.Fatal("awaitCredentials returned ok=false on a non-executor role")
	}
	if _, has := credbundle.FromContext(ctx); has {
		t.Fatal("non-executor role returned a ctx carrying a bundle")
	}

	var status string
	if err := database.QueryRow(`SELECT status FROM runs WHERE id = ?`, run.ID).Scan(&status); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	if status != "running" {
		t.Fatalf("run status = %q, want unchanged %q", status, "running")
	}
}

// TestAwaitCredentials_TimesOutAndRequeues pins the brain-down crash
// window (TFAC-614 point 4): an executor-role run that never gets a
// bundle parks in 'awaiting_credentials', times out, and is released back
// to 'queued' with its ownership stamp cleared.
func TestAwaitCredentials_TimesOutAndRequeues(t *testing.T) {
	runmode.SetRoleForTest(t, runmode.RoleExecutor)
	database := newDelegateTestDB(t)
	seedRun(t, database, "run-timeout", "sess", "/tmp/wt")
	stores := sqlitestore.New(database)
	s := NewSpawner(database, stores, nil, nil, "")
	s.SetExecutorID("executor-1", 1)
	s.SetAwaitingCredentialsTimeout(150*time.Millisecond, 20*time.Millisecond)

	run := &domain.AgentRun{ID: "run-timeout", OrgID: runmode.LocalDefaultOrgID}
	_, ok := s.awaitCredentials(context.Background(), run.OrgID, run)
	if ok {
		t.Fatal("awaitCredentials returned ok=true with no bundle ever provisioned")
	}

	var status string
	var executorID *string
	if err := database.QueryRow(`SELECT status, executor_id FROM runs WHERE id = ?`, run.ID).Scan(&status, &executorID); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if status != "queued" {
		t.Fatalf("run status after timeout = %q, want %q", status, "queued")
	}
	if executorID != nil {
		t.Fatalf("run executor_id after timeout = %v, want cleared", *executorID)
	}
}

// TestAwaitCredentials_UnsealsFreshBundle pins the happy path: a bundle
// sealed for the executor's current boot epoch is found, unsealed, and
// carried on the returned ctx.
func TestAwaitCredentials_UnsealsFreshBundle(t *testing.T) {
	runmode.SetRoleForTest(t, runmode.RoleExecutor)
	database := newDelegateTestDB(t)
	seedRun(t, database, "run-fresh", "sess", "/tmp/wt")
	stores := sqlitestore.New(database)
	s := NewSpawner(database, stores, nil, nil, "")
	s.SetExecutorID("executor-1", 7)
	s.SetAwaitingCredentialsTimeout(2*time.Second, 20*time.Millisecond)

	kp, err := credseal.GenerateKeyPair()
	if err != nil {
		t.Fatalf("generate keypair: %v", err)
	}
	s.SetSealingKey(kp)

	bundle := &credbundle.Bundle{BootEpoch: 7, LLM: map[string]string{"ANTHROPIC_API_KEY": "sk-ant-test"}}
	plaintext, err := bundle.Marshal()
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	sealed, err := credseal.Seal(kp.Public, plaintext)
	if err != nil {
		t.Fatalf("seal bundle: %v", err)
	}

	// Provision concurrently with the wait, mirroring the real handshake
	// (the brain writes asynchronously off the executor's cred_request).
	go func() {
		time.Sleep(40 * time.Millisecond)
		if err := stores.RunCredentials.Put(context.Background(), runmode.LocalDefaultOrgID, "run-fresh", "executor-1", 7, sealed); err != nil {
			t.Errorf("put run credentials: %v", err)
		}
	}()

	run := &domain.AgentRun{ID: "run-fresh", OrgID: runmode.LocalDefaultOrgID}
	ctx, ok := s.awaitCredentials(context.Background(), run.OrgID, run)
	if !ok {
		t.Fatal("awaitCredentials returned ok=false; want the bundle to be found")
	}
	got, has := credbundle.FromContext(ctx)
	if !has {
		t.Fatal("returned ctx carries no bundle")
	}
	if got.LLM["ANTHROPIC_API_KEY"] != "sk-ant-test" {
		t.Fatalf("unsealed bundle mismatch: %+v", got)
	}
}

// TestAwaitCredentials_NeverOpensStaleEpochBundle pins the never-decrypt-
// an-old-epoch-bundle invariant (TFAC-614 point 4): a bundle sealed for a
// PRIOR boot epoch must never be handed to credseal.Open even though it
// sits in run_credentials for this exact run — the wait must keep polling
// (and eventually time out) rather than unseal it.
func TestAwaitCredentials_NeverOpensStaleEpochBundle(t *testing.T) {
	runmode.SetRoleForTest(t, runmode.RoleExecutor)
	database := newDelegateTestDB(t)
	seedRun(t, database, "run-stale-epoch", "sess", "/tmp/wt")
	stores := sqlitestore.New(database)
	s := NewSpawner(database, stores, nil, nil, "")
	// Current boot is epoch 9; the stale bundle below was sealed for the
	// PRIOR boot's epoch (2) and a DIFFERENT (now-discarded) keypair.
	s.SetExecutorID("executor-1", 9)
	s.SetAwaitingCredentialsTimeout(150*time.Millisecond, 20*time.Millisecond)

	oldKP, err := credseal.GenerateKeyPair()
	if err != nil {
		t.Fatalf("generate old keypair: %v", err)
	}
	newKP, err := credseal.GenerateKeyPair()
	if err != nil {
		t.Fatalf("generate new keypair: %v", err)
	}
	s.SetSealingKey(newKP)

	staleBundle := &credbundle.Bundle{BootEpoch: 2, LLM: map[string]string{"ANTHROPIC_API_KEY": "stale-key"}}
	plaintext, err := staleBundle.Marshal()
	if err != nil {
		t.Fatalf("marshal stale bundle: %v", err)
	}
	sealed, err := credseal.Seal(oldKP.Public, plaintext)
	if err != nil {
		t.Fatalf("seal stale bundle: %v", err)
	}
	if err := stores.RunCredentials.Put(context.Background(), runmode.LocalDefaultOrgID, "run-stale-epoch", "executor-1", 2, sealed); err != nil {
		t.Fatalf("put stale run credentials: %v", err)
	}

	run := &domain.AgentRun{ID: "run-stale-epoch", OrgID: runmode.LocalDefaultOrgID}
	_, ok := s.awaitCredentials(context.Background(), run.OrgID, run)
	if ok {
		t.Fatal("awaitCredentials returned ok=true against a stale-epoch bundle; it must never be opened")
	}
}
