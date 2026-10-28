package app

import (
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// setKnowledgeEnv is the object-store environment the bundled stack gives a
// control pod: the shared endpoint and the knowledge bucket's key pair.
func setKnowledgeEnv(t *testing.T) {
	t.Helper()
	t.Setenv("TF_BLOB_ENDPOINT", "http://seaweedfs:8333")
	t.Setenv("TF_KNOWLEDGE_BUCKET", "")
	t.Setenv("TF_KNOWLEDGE_ACCESS_KEY", "kb-ak")
	t.Setenv("TF_KNOWLEDGE_SECRET_KEY", "kb-sk")
}

// TestOpenBlobStores_ControlNeedsOnlyKnowledge: a control pod is given no
// workspace credentials, so it must boot without reading any, and must hand
// the spawner no workspace store.
func TestOpenBlobStores_ControlNeedsOnlyKnowledge(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	setKnowledgeEnv(t)
	// A workspace config the workspace bucket would refuse, were it read.
	t.Setenv("TF_WORKSPACE_BUCKET", "tf-knowledge")
	t.Setenv("TF_WORKSPACE_ACCESS_KEY", "ws-ak")
	t.Setenv("TF_WORKSPACE_SECRET_KEY", "")

	workspaces, knowledge, err := openBlobStores(planForRole(runmode.RoleControl))
	if err != nil {
		t.Fatalf("openBlobStores(control): %v", err)
	}
	if workspaces != nil {
		t.Errorf("control's workspace store = %T; want none", workspaces)
	}
	if knowledge == nil {
		t.Error("control's knowledge store = nil; want the knowledge bucket")
	}
}

func TestOpenBlobStores_ExecutorBuildsBoth(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	setKnowledgeEnv(t)
	t.Setenv("TF_WORKSPACE_BUCKET", "")
	t.Setenv("TF_WORKSPACE_ACCESS_KEY", "ws-ak")
	t.Setenv("TF_WORKSPACE_SECRET_KEY", "ws-sk")

	workspaces, knowledge, err := openBlobStores(planForRole(runmode.RoleExecutor))
	if err != nil {
		t.Fatalf("openBlobStores(executor): %v", err)
	}
	if workspaces == nil || knowledge == nil {
		t.Fatalf("executor stores = (%v, %v); want both", workspaces, knowledge)
	}
}

func TestOpenBlobStores_ExecutorRefusesOneBucketForBoth(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	setKnowledgeEnv(t)
	t.Setenv("TF_WORKSPACE_BUCKET", "tf-knowledge")

	_, _, err := openBlobStores(planForRole(runmode.RoleExecutor))
	if err == nil || !strings.Contains(err.Error(), "TF_WORKSPACE_BUCKET") || !strings.Contains(err.Error(), "TF_KNOWLEDGE_BUCKET") {
		t.Fatalf("openBlobStores with one bucket for both = %v; want a refusal naming TF_WORKSPACE_BUCKET and TF_KNOWLEDGE_BUCKET", err)
	}
}

// TestOpenBlobStores_LocalReadsNoObjectConfig: local mode snapshots to disk
// and keeps knowledge as plain files, so no object-store variable is needed.
func TestOpenBlobStores_LocalReadsNoObjectConfig(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	paths.SetForTest(t, t.TempDir())
	t.Setenv("TF_BLOB_ENDPOINT", "")

	workspaces, knowledge, err := openBlobStores(planForRole(runmode.RoleAll))
	if err != nil {
		t.Fatalf("openBlobStores(local): %v", err)
	}
	if workspaces == nil {
		t.Error("local workspace store = nil; want the on-disk store")
	}
	if knowledge != nil {
		t.Errorf("local knowledge store = %T; want none", knowledge)
	}
}
