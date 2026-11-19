package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
)

// teamSettingsAs calls a team-settings handler as one user of the rig's org,
// with the claims and active org a session would have seeded.
func (r *archivedTeamRig) teamSettingsAs(t *testing.T, userID, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/api/teams/"+r.teamID+"/settings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("team_id", r.teamID)
	ctx := httpx.WithClaims(req.Context(), &verify.Claims{Subject: userID})
	req = req.WithContext(httpx.WithOrgID(ctx, r.orgID))

	rec := httptest.NewRecorder()
	if method == http.MethodGet {
		r.s.handleTeamSettingsGet(rec, req)
	} else {
		r.s.handleTeamSettingsPatch(rec, req)
	}
	return rec
}

// storeLocalPlugins writes the team's plugin list straight through the store,
// the way a list saved before the deployment turned multi would be there.
func (r *archivedTeamRig) storeLocalPlugins(t *testing.T, list []domain.LocalPlugin) {
	t.Helper()
	set := domain.DefaultTeamSettingsFor(true)
	set.LocalPlugins = list
	if err := r.s.tx.WithTx(context.Background(), r.orgID, r.owner, func(tx db.TxStores) error {
		_, err := tx.Teams.UpdateSettings(context.Background(), r.teamID, set)
		return err
	}); err != nil {
		t.Fatalf("store local plugins: %v", err)
	}
}

type teamSettingsPluginView struct {
	TeamSettings struct {
		LocalPlugins json.RawMessage `json:"LocalPlugins"`
	} `json:"team_settings"`
	LocalPluginCount int `json:"local_plugin_count"`
}

func decodePluginView(t *testing.T, rec *httptest.ResponseRecorder) teamSettingsPluginView {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var v teamSettingsPluginView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	return v
}

// TestTeamSettingsGet_LocalPlugins_RedactedForNonAdmin: paths and env values
// are operator detail. A team admin reads the list; any other member reads
// null and a count, so the settings page can say plugins exist without saying
// what they are.
func TestTeamSettingsGet_LocalPlugins_RedactedForNonAdmin(t *testing.T) {
	r := newArchivedTeamRig(t)
	r.storeLocalPlugins(t, []domain.LocalPlugin{
		{Path: "/opt/plugins/guard", Required: true, Env: map[string]string{"GUARD_STATE": "{run_state_dir}s"}, GrantTools: []string{"Skill"}},
		{Path: "/opt/plugins/notes", Env: map[string]string{}, GrantTools: []string{}},
	})

	admin := decodePluginView(t, r.teamSettingsAs(t, r.owner, http.MethodGet, ""))
	var list []domain.LocalPlugin
	if err := json.Unmarshal(admin.TeamSettings.LocalPlugins, &list); err != nil || len(list) != 2 {
		t.Fatalf("team admin LocalPlugins = %s (err %v), want the two entries", admin.TeamSettings.LocalPlugins, err)
	}
	if list[0].Path != "/opt/plugins/guard" || list[0].Env["GUARD_STATE"] != "{run_state_dir}s" {
		t.Errorf("team admin's first entry = %+v, want it in full", list[0])
	}
	if admin.LocalPluginCount != 2 {
		t.Errorf("team admin local_plugin_count = %d, want 2", admin.LocalPluginCount)
	}

	rec := r.teamSettingsAs(t, r.member, http.MethodGet, "")
	member := decodePluginView(t, rec)
	if string(member.TeamSettings.LocalPlugins) != "null" {
		t.Errorf("member LocalPlugins = %s, want null", member.TeamSettings.LocalPlugins)
	}
	if member.LocalPluginCount != 2 {
		t.Errorf("member local_plugin_count = %d, want 2", member.LocalPluginCount)
	}
	for _, leaked := range []string{"/opt/plugins", "GUARD_STATE", "run_state_dir"} {
		if strings.Contains(rec.Body.String(), leaked) {
			t.Errorf("the member's response carries %q: %s", leaked, rec.Body.String())
		}
	}
}

// TestTeamSettingsPatch_LocalPlugins_TeamAdminOnly: a plain team member cannot
// write the list, whatever it says.
func TestTeamSettingsPatch_LocalPlugins_TeamAdminOnly(t *testing.T) {
	r := newArchivedTeamRig(t)

	rec := r.teamSettingsAs(t, r.member, http.MethodPatch, `{"local_plugins": []}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member PATCH = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	httpxtest.AssertFirstError(t, rec, httpx.ReasonForbidden, "")
}

// TestTeamSettingsPatch_LocalPlugins_RefusedInMultiMode: a multi deployment
// runs delegations on the native loop, with no Claude Code to load a plugin
// into, so a non-empty list is a 422 naming the field. Emptying the list is
// not refused: a list stored before the deployment turned multi can still be
// cleared.
func TestTeamSettingsPatch_LocalPlugins_RefusedInMultiMode(t *testing.T) {
	r := newArchivedTeamRig(t)
	dir := pluginDir(t)

	body, _ := json.Marshal(map[string]any{"local_plugins": []map[string]any{pluginEntry(dir)}})
	rec := r.teamSettingsAs(t, r.owner, http.MethodPatch, string(body))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("non-empty list in multi mode = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	httpxtest.AssertFirstError(t, rec, httpx.ReasonModeUnsupported, "local_plugins")
	if !strings.Contains(rec.Body.String(), "local plugins apply to local mode only") {
		t.Errorf("the refusal does not say why: %s", rec.Body.String())
	}
	if got := decodePluginView(t, r.teamSettingsAs(t, r.owner, http.MethodGet, "")); got.LocalPluginCount != 0 {
		t.Errorf("a refused save stored %d plugins", got.LocalPluginCount)
	}

	r.storeLocalPlugins(t, []domain.LocalPlugin{{Path: dir}})
	for _, clear := range []string{`{"local_plugins": null}`, `{"local_plugins": []}`} {
		r.storeLocalPlugins(t, []domain.LocalPlugin{{Path: dir}})
		got := decodePluginView(t, r.teamSettingsAs(t, r.owner, http.MethodPatch, clear))
		if got.LocalPluginCount != 0 || string(got.TeamSettings.LocalPlugins) != "[]" {
			t.Errorf("%s in multi mode left %s, count %d; want [] and 0", clear, got.TeamSettings.LocalPlugins, got.LocalPluginCount)
		}
	}
}
