package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestGitHubAppManifest_RequestsMembersRead: the manifest asks for exactly the
// import bar's permissions, including members:read, its only organization
// permission, which is what makes GitHub let only an org's owners install it.
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

	want := make(map[string]string, len(importRequiredPermissions))
	for _, req := range importRequiredPermissions {
		want[req.name] = req.level
	}
	if !reflect.DeepEqual(manifest.DefaultPermissions, want) {
		t.Errorf("manifest default_permissions = %v, want the import bar %v", manifest.DefaultPermissions, want)
	}
}
