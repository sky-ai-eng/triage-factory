package server

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestOrgSettings_CloneProtocolClear_LocalKeepsTheDefault pins the clear arm
// in the mode where "ssh" is a real answer: local honors the stored value, so a
// null means the package default and has to land on it rather than on the value
// that happens to be the other option.
func TestOrgSettings_CloneProtocolClear_LocalKeepsTheDefault(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	patchOrgSettingsOK(t, s, map[string]any{"github_clone_protocol": nil})

	got, err := s.orgs.GetSettingsSystem(t.Context(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("read back settings: %v", err)
	}
	if got.GitHubCloneProtocol != "https" {
		t.Errorf("stored github_clone_protocol = %q; want %q", got.GitHubCloneProtocol, "https")
	}
}

// TestOrgSettings_MultiNeverStoresSSH walks every door onto the column in the
// mode that cannot honor an SSH clone at all — an App installation token is an
// HTTPS bearer credential, and the runtime container carries no key, agent or
// known_hosts.
//
// All three doors matter because they fail differently. Provisioning names only
// org_id and takes the column from its DEFAULT, so the seed is the value a
// fresh org is born with and nothing in the API is involved. An explicit null
// means "back to the default", which reaches the column through a resolver
// rather than as the raw package value. The set arm is the only one that was
// ever guarded.
//
// Asserted against the stored column rather than the GET: the read side
// normalizes, so every one of these looks correct through the API whatever the
// row says, and only a consumer reading the column directly — the profiler,
// choosing the clone URL it writes onto every repository row — can tell.
func TestOrgSettings_MultiNeverStoresSSH(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	founder := r.seedUser()
	orgA, _ := r.seedOrg(founder, "acme")
	sid := r.signInAs(founder)

	// Verbatim the statement seedSettingsRows runs at provisioning: naming only
	// the foreign key is what lets every other column come from its DEFAULT, so
	// this is the row shape a real multi org is born with.
	if _, err := r.h.AdminDB.Exec(
		`INSERT INTO org_settings (org_id) VALUES ($1) ON CONFLICT (org_id) DO NOTHING`, orgA,
	); err != nil {
		t.Fatalf("seed org settings: %v", err)
	}

	storedProtocol := func() string {
		t.Helper()
		var proto string
		if err := r.h.AdminDB.QueryRow(
			`SELECT github_clone_protocol FROM org_settings WHERE org_id = $1`, orgA,
		).Scan(&proto); err != nil {
			t.Fatalf("read stored clone protocol: %v", err)
		}
		return proto
	}

	if got := storedProtocol(); got != "https" {
		t.Fatalf("a freshly provisioned org stores %q; want %q", got, "https")
	}

	if got := patchCloneProtocol(t, r, orgA, sid, nil); got != http.StatusOK {
		t.Fatalf("clear: %d; want 200", got)
	}
	if got := storedProtocol(); got != "https" {
		t.Errorf("after an explicit clear the column holds %q; want %q", got, "https")
	}

	code := patchCloneProtocol(t, r, orgA, sid, "ssh")
	if code != http.StatusBadRequest && code != http.StatusUnprocessableEntity {
		t.Errorf("set ssh: %d; want a field rejection", code)
	}
	if got := storedProtocol(); got != "https" {
		t.Errorf("after a refused ssh write the column holds %q; want %q", got, "https")
	}
}

// patchCloneProtocol PATCHes just this field with the row's live concurrency
// token folded in, and returns the status.
func patchCloneProtocol(t *testing.T, r *authRig, orgID uuid.UUID, sid string, value any) int {
	t.Helper()
	path := "/api/orgs/" + orgID.String() + "/settings"

	resp := r.requestWithSid(http.MethodGet, path, sid)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d: %s", path, resp.StatusCode, body)
	}
	var cur struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(body, &cur); err != nil {
		t.Fatalf("decode settings: %v", err)
	}

	out := r.postJSONWithSid(http.MethodPatch, path, sid, map[string]any{
		"version":               cur.Version,
		"github_clone_protocol": value,
	})
	out.Body.Close()
	return out.StatusCode
}
