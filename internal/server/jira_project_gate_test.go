package server

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/zalando/go-keyring"
)

// jiraProjectsBody builds a replace-set body naming each project by key, the
// spelling putTeamJiraProjects rewrites to the route's identifiers, each fully
// armed — the gate is about the project, so the rules are held constant and
// valid.
func jiraProjectsBody(keys ...string) map[string]any {
	projects := make([]map[string]any, len(keys))
	for i, k := range keys {
		projects[i] = map[string]any{
			"key": k, "pickup": validPickup(), "in_progress": validInProgress(), "done": validDone(),
		}
	}
	return map[string]any{"jira_projects": projects}
}

// putJiraProjects sends the replace-set write and hands back the recorder.
func putJiraProjects(t *testing.T, s *Server, keys ...string) *httptest.ResponseRecorder {
	t.Helper()
	return putTeamJiraProjects(t, s, jiraProjectsBody(keys...))
}

// TestJiraProjectsPut_AddsAProjectTheCatalogKnows is the happy path the gate
// must not get in the way of.
func TestJiraProjectsPut_AddsAProjectTheCatalogKnows(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY", "OPS")

	if rec := putJiraProjects(t, s, "SKY"); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := teamJiraProjectKeys(t, s); len(got) != 1 || got[0] != "SKY" {
		t.Errorf("stored keys = %v, want [SKY]", got)
	}
	if fake.Calls() == 0 {
		t.Error("an added project was stored without asking Jira whether it exists")
	}
}

// TestJiraProjectsPut_UnknownProjectRejected: the picker offers only what the
// org credential can see, so the handler enforces the same set — and it names
// the element the bad id arrived in rather than faulting the collection.
func TestJiraProjectsPut_UnknownProjectRejected(t *testing.T) {
	s, _ := newServerWithJiraCatalog(t, "SKY", "OPS")

	rec := putJiraProjects(t, s, "SKY", "GHOST")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT with an unknown project = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	items := decodeErrorItems(t, rec)
	if len(items) != 1 {
		t.Fatalf("errors = %d, want 1 (only GHOST is unknown); body=%s", len(items), rec.Body.String())
	}
	if items[0].Reason != "INVALID_FIELD" {
		t.Errorf("reason = %q, want INVALID_FIELD", items[0].Reason)
	}
	if items[0].Field != "jira_projects[1].jira_id" {
		t.Errorf("field = %q, want jira_projects[1].jira_id", items[0].Field)
	}
	if !strings.Contains(items[0].Message, fakeJiraProjectID("GHOST")) {
		t.Errorf("message %q does not name the offending id", items[0].Message)
	}
	if got := teamJiraProjectKeys(t, s); len(got) != 0 {
		t.Errorf("stored keys = %v, want nothing — a rejected write stores none of it", got)
	}
}

// TestJiraProjectsPut_EveryUnknownProjectNamed — validation reports every
// failing field, not the first, so a caller fixing three typos learns about
// three.
func TestJiraProjectsPut_EveryUnknownProjectNamed(t *testing.T) {
	s, _ := newServerWithJiraCatalog(t, "SKY")

	rec := putJiraProjects(t, s, "GHOST", "SKY", "PHANTOM")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	items := decodeErrorItems(t, rec)
	if len(items) != 2 {
		t.Fatalf("errors = %d, want one per unknown project; body=%s", len(items), rec.Body.String())
	}
	named := items[0].Message + " " + items[1].Message
	if !strings.Contains(named, fakeJiraProjectID("GHOST")) || !strings.Contains(named, fakeJiraProjectID("PHANTOM")) {
		t.Errorf("errors %q do not name both unknown ids", named)
	}
}

// TestJiraProjectsPut_StoredProjectGrandfathered is the reason the gate is
// scoped to added projects. This route is a replace-set — every write resends
// the whole set — so gating a stored project would mean a project deleted in
// Jira locks the team out of its own configuration door, including the write
// that would remove the offending row.
func TestJiraProjectsPut_StoredProjectGrandfathered(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY", "OPS")

	if rec := putJiraProjects(t, s, "SKY", "OPS"); rec.Code != http.StatusOK {
		t.Fatalf("seeding PUT = %d, body=%s", rec.Code, rec.Body.String())
	}
	// SKY is deleted upstream. The team's stored set still names it.
	fake.SetKeys("OPS")

	if rec := putJiraProjects(t, s, "SKY", "OPS"); rec.Code != http.StatusOK {
		t.Fatalf("re-PUT of a stored set whose project vanished upstream = %d, want 200; body=%s",
			rec.Code, rec.Body.String())
	}
	if got := teamJiraProjectKeys(t, s); len(got) != 2 {
		t.Errorf("stored keys = %v, want both preserved", got)
	}
	// And the removal that clears it still works, with Jira offering nothing.
	if rec := putJiraProjects(t, s, "OPS"); rec.Code != http.StatusOK {
		t.Fatalf("removal PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestJiraProjectsPut_RemovalOnlyAsksJiraNothing: no project is being added,
// so there is nothing to verify — and the door stays open while Jira is down.
func TestJiraProjectsPut_RemovalOnlyAsksJiraNothing(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY", "OPS")

	if rec := putJiraProjects(t, s, "SKY", "OPS"); rec.Code != http.StatusOK {
		t.Fatalf("seeding PUT = %d, body=%s", rec.Code, rec.Body.String())
	}
	fake.SetFailing(true)
	before := fake.Calls()

	if rec := putJiraProjects(t, s, "SKY"); rec.Code != http.StatusOK {
		t.Fatalf("removal PUT with Jira down = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if after := fake.Calls(); after != before {
		t.Errorf("a removal-only write reached Jira %d time(s)", after-before)
	}
	if got := teamJiraProjectKeys(t, s); len(got) != 1 || got[0] != "SKY" {
		t.Errorf("stored keys = %v, want [SKY]", got)
	}
}

// TestJiraProjectsPut_ArmingAStoredProjectSkipsTheProjectCheck: mapping a
// watched project's statuses adds no project, so its existence is not
// re-checked — but the status ids are new, so that project's workflow is
// read, and the names it comes back with are what get stored.
func TestJiraProjectsPut_ArmingAStoredProjectSkipsTheProjectCheck(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY")

	watch := map[string]any{"jira_projects": []map[string]any{{"key": "SKY"}}}
	if rec := putTeamJiraProjects(t, s, watch); rec.Code != http.StatusOK {
		t.Fatalf("watch PUT = %d, body=%s", rec.Code, rec.Body.String())
	}
	// SKY disappears from the catalog. Arming it must still work: the project
	// is already stored, so nothing re-checks it.
	fake.SetKeys()

	if rec := putJiraProjects(t, s, "SKY"); rec.Code != http.StatusOK {
		t.Fatalf("arming PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := storedJiraProject(t, s, "SKY")
	if got.InProgress.Canonical == nil || got.InProgress.Canonical.Name != "In Progress" {
		t.Errorf("stored in-progress canonical = %+v, want the server-resolved name", got.InProgress.Canonical)
	}
}

// TestJiraProjectsPut_UnchangedRulesAreNeverRevalidated is the grandfathering
// that keeps the door open: a status retired from the workflow after a team
// armed it must not brick every later save of that team's set.
func TestJiraProjectsPut_UnchangedRulesAreNeverRevalidated(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY")

	if rec := putJiraProjects(t, s, "SKY"); rec.Code != http.StatusOK {
		t.Fatalf("arming PUT = %d, body=%s", rec.Code, rec.Body.String())
	}
	// The in-progress status is retired upstream, and then Jira stops answering
	// at all. Neither can touch a rule nobody changed.
	fake.SetStatuses("SKY", fakeJiraStatus{ID: statusToDoID, Name: "To Do"},
		fakeJiraStatus{ID: statusDoneID, Name: "Done"})
	fake.SetFailing(true)
	before := fake.Calls()

	if rec := putJiraProjects(t, s, "SKY"); rec.Code != http.StatusOK {
		t.Fatalf("re-PUT of an unchanged set = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if after := fake.Calls(); after != before {
		t.Errorf("an unchanged set reached Jira %d time(s)", after-before)
	}
	got := storedJiraProject(t, s, "SKY")
	if got.InProgress.Canonical == nil || got.InProgress.Canonical.ID != statusInProgressID {
		t.Errorf("stored in-progress canonical = %+v, want the retired status kept byte-for-byte", got.InProgress.Canonical)
	}
}

// TestJiraProjectsPut_UnknownStatusIDRejected: a status the project's workflow
// does not have is the caller's mistake, and the fault names the rule field it
// arrived in rather than the project.
func TestJiraProjectsPut_UnknownStatusIDRejected(t *testing.T) {
	s, _ := newServerWithJiraCatalog(t, "SKY")

	body := teamProjectsBodyWith("SKY",
		validPickup(),
		map[string]any{"member_ids": []string{statusUnknownID}, "canonical_id": statusUnknownID},
		validDone(),
	)
	rec := putTeamJiraProjects(t, s, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT with an unknown status id = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	items := decodeErrorItems(t, rec)
	fields := make([]string, 0, len(items))
	for _, item := range items {
		if item.Reason != "INVALID_FIELD" {
			t.Errorf("reason = %q, want INVALID_FIELD", item.Reason)
		}
		fields = append(fields, item.Field)
	}
	if !slices.Contains(fields, "jira_projects[0].in_progress.member_ids") {
		t.Errorf("faulting fields = %v, want the member_ids that carried the unknown id", fields)
	}
	if got := teamJiraProjectKeys(t, s); len(got) != 0 {
		t.Errorf("stored keys = %v, want nothing — a rejected write stores none of it", got)
	}
}

// TestJiraProjectsPut_StatusNamesAreResolvedNotAccepted: the caller supplies
// ids, the server supplies names. There is no wire field a name could arrive
// in, so a stored name is always one Jira gave us.
func TestJiraProjectsPut_StatusNamesAreResolvedNotAccepted(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY")
	fake.SetStatuses("SKY",
		fakeJiraStatus{ID: statusToDoID, Name: "Selected for work"},
		fakeJiraStatus{ID: statusInProgressID, Name: "In Progress"},
		fakeJiraStatus{ID: statusDoneID, Name: "Done"})

	if rec := putJiraProjects(t, s, "SKY"); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, body=%s", rec.Code, rec.Body.String())
	}
	got := storedJiraProject(t, s, "SKY")
	if len(got.Pickup.Members) != 1 || got.Pickup.Members[0].Name != "Selected for work" {
		t.Errorf("stored pickup = %+v, want the name Jira spells that id with today", got.Pickup.Members)
	}
}

// TestJiraProjectsPut_UpstreamFailureStoresNothing keeps the two failure modes
// apart. Jira not answering establishes nothing, so it is an upstream error —
// never a silent skip, and never a stored success, which is the one outcome a
// caller can neither see nor undo by retrying.
func TestJiraProjectsPut_UpstreamFailureStoresNothing(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY")
	fake.SetFailing(true)

	rec := putJiraProjects(t, s, "SKY")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("PUT with Jira failing = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
	if reason := decodeErrorItems(t, rec)[0].Reason; reason != "UPSTREAM_UNAVAILABLE" {
		t.Errorf("reason = %q, want UPSTREAM_UNAVAILABLE", reason)
	}
	if got := teamJiraProjectKeys(t, s); len(got) != 0 {
		t.Errorf("stored keys = %v, want nothing stored on an unconfirmed add", got)
	}
}

// TestJiraProjectsPut_UnconnectedOrgCannotAdd: with no credential there
// is no catalog to check against, so an add is refused with the configuration
// answer rather than stored unverified.
func TestJiraProjectsPut_UnconnectedOrgCannotAdd(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	rec := putJiraProjects(t, s, "SKY")
	if rec.Code != http.StatusConflict {
		t.Fatalf("PUT on an unconnected org = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if reason := decodeErrorItems(t, rec)[0].Reason; reason != "NOT_CONFIGURED" {
		t.Errorf("reason = %q, want NOT_CONFIGURED", reason)
	}
}

// TestJiraProjectsPut_ShapeFaultsPrecedeTheGate: a malformed project id never
// reaches Jira. The gate is an existence check, not a grammar one, and asking
// about "SKY" (a key, where an id belongs) would spend a round trip to learn
// what the pattern already knows.
func TestJiraProjectsPut_ShapeFaultsPrecedeTheGate(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY")

	rec := doJSON(t, s, http.MethodPut, teamJiraProjectsPath("default"),
		map[string]any{"jira_projects": []map[string]any{{"jira_id": "SKY"}}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if items := decodeErrorItems(t, rec); len(items) != 1 || items[0].Field != "jira_projects[0].jira_id" {
		t.Errorf("errors = %+v, want one on jira_projects[0].jira_id", items)
	}
	if fake.Calls() != 0 {
		t.Errorf("a malformed id still reached Jira (%d calls)", fake.Calls())
	}
}

// TestJiraStatusesRead_ForwardsIDs pins the other half of the id contract. The
// PUT accepts status ids and nothing else, so a caller — the SPA or a headless
// one — needs somewhere to learn which id is which: this read is that catalog,
// and it would be useless if it served names alone.
func TestJiraStatusesRead_ForwardsIDs(t *testing.T) {
	s, _ := newServerWithJiraCatalog(t, "SKY")

	got := decodeList[jiraStatusJSON](t, doJSON(t, s, http.MethodPost, jiraStatusesListPath("SKY"), map[string]any{})).Items
	if len(got) != len(jiraFixtureStatuses) {
		t.Fatalf("statuses = %+v, want the project's whole workflow", got)
	}
	byName := map[string]string{}
	for _, st := range got {
		if st.ID == "" {
			t.Errorf("status %q came back with no id", st.Name)
		}
		byName[st.Name] = st.ID
	}
	if byName["In Progress"] != statusInProgressID {
		t.Errorf("In Progress id = %q, want %q", byName["In Progress"], statusInProgressID)
	}
}

// TestJiraProjectsPut_FieldFaultSurvivesALaterUpstreamFailure: a replace-set
// can carry both kinds of fault at once, and they are not equally useful. A bad
// identifier is the caller's own and is true whether or not Jira is reachable —
// they have to fix it before any retry can store anything. "Jira did not
// answer" only says try again. Reporting the second over the first throws away
// the half of the answer that is certain and sends the caller round a loop to
// rediscover it.
func TestJiraProjectsPut_FieldFaultSurvivesALaterUpstreamFailure(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY")
	// SKY resolves as a project but its workflow read fails, so the fault the
	// gate finds first (GHOST) and the one that stops it are in one request.
	fake.SetStatusesFailing("SKY")

	rec := putJiraProjects(t, s, "GHOST", "SKY")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT = %d, want 400 naming GHOST; body=%s", rec.Code, rec.Body.String())
	}
	items := decodeErrorItems(t, rec)
	if len(items) != 1 || !strings.Contains(items[0].Message, fakeJiraProjectID("GHOST")) {
		t.Fatalf("errors = %+v, want the unknown project named; body=%s", items, rec.Body.String())
	}
	if items[0].Field == "" {
		t.Error("the field fault lost its field on the way out")
	}
	// Nothing is stored under either fault.
	if got := teamJiraProjectKeys(t, s); len(got) != 0 {
		t.Errorf("stored keys = %v, want none — neither project was verified", got)
	}
}

// The other half: with no field fault to report, an unreachable Jira is still
// an upstream error rather than a 400 blaming the caller.
func TestJiraProjectsPut_UpstreamFailureAloneIsStillUpstream(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY")
	fake.SetStatusesFailing("SKY")

	rec := putJiraProjects(t, s, "SKY")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("PUT = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
	if items := decodeErrorItems(t, rec); len(items) != 1 || items[0].Reason != "UPSTREAM_UNAVAILABLE" {
		t.Errorf("errors = %+v, want a single upstream reason", items)
	}
}
