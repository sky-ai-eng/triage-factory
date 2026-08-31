package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestGitHubAppManifest_RequestsMembersRead guards the single line in the
// manifest that two unrelated things hang off, only one of which is visible
// from the call site that uses it.
//
// The visible reason is GET /orgs/{org}/teams: the team-mapping import and the
// poller's deletion-reconcile both need it, and both 403 into a silent zero
// without it in an App-only org.
//
// The invisible one is that `members` is the ONLY organization permission the
// manifest requests, and GitHub restricts installing an App on an organization
// to that organization's owners exactly when the App asks for organization
// permissions. That guard is enforced entirely by GitHub in response to this
// one line — there is no code anywhere in this repository that implements it,
// so nothing but this test would fail if a scope-minimization pass removed the
// permission.
//
// The set-equality test next door holds the manifest and the bring-your-own-App
// import bar to the same permissions, so this assertion covers both doors: it
// is the one that fails when the permission leaves BOTH of them together, which
// is exactly how a scope-minimization pass would remove it.
func TestGitHubAppManifest_RequestsMembersRead(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	s.SetDeployConfig("http://localhost:3000", key)

	_, manifestJSON, _, err := s.buildManifestAndState(context.Background(),
		runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "org", "acme", "")
	if err != nil {
		t.Fatalf("buildManifestAndState: %v", err)
	}
	var manifest struct {
		DefaultPermissions map[string]string `json:"default_permissions"`
	}
	if err := json.Unmarshal([]byte(manifestJSON), &manifest); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}

	got, ok := manifest.DefaultPermissions["members"]
	if !ok {
		t.Fatal("the manifest requests no `members` permission: an App with no organization permission " +
			"can be installed on an organization by any repository admin, and GET /orgs/{org}/teams 403s")
	}
	if got != "read" {
		t.Errorf("members = %q, want \"read\" — TF lists teams and never edits membership", got)
	}
}
