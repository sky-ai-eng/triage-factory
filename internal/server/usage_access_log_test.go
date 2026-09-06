package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// --- pure: label rendering ---

func TestAccessChangeLabel(t *testing.T) {
	tests := []struct {
		name   string
		change domain.AccessChange
		target string
		team   string
		want   string
	}{
		{
			name:   "org_member_granted self (invite-accept) reads as joined",
			change: domain.AccessChange{Action: domain.AccessActionOrgMemberGranted, ActorUserID: "u1", TargetUserID: "u1"},
			want:   "joined the org",
		},
		{
			name:   "org_member_granted self via SSO JIT reads as joined via SSO",
			change: domain.AccessChange{Action: domain.AccessActionOrgMemberGranted, ActorUserID: "u1", TargetUserID: "u1", DetailJSON: `{"source":"sso_jit","role":"member"}`},
			want:   "joined the org via SSO",
		},
		{
			name:   "org_member_granted by another reads as granted",
			change: domain.AccessChange{Action: domain.AccessActionOrgMemberGranted, ActorUserID: "u1", TargetUserID: "u2"},
			target: "Alice",
			want:   "granted Alice org membership",
		},
		{
			name:   "org_role_changed renders the from→to transition",
			change: domain.AccessChange{Action: domain.AccessActionOrgRoleChanged, TargetUserID: "u2", DetailJSON: `{"old_role":"member","new_role":"admin"}`},
			target: "Alice",
			want:   "changed Alice from member to admin",
		},
		{
			name:   "org_member_revoked",
			change: domain.AccessChange{Action: domain.AccessActionOrgMemberRevoked, TargetUserID: "u2"},
			target: "Bob",
			want:   "removed Bob from the org",
		},
		{
			name:   "org_ownership_transferred",
			change: domain.AccessChange{Action: domain.AccessActionOrgOwnershipTransferred, TargetUserID: "u3"},
			target: "Carol",
			want:   "transferred org ownership to Carol",
		},
		{
			name:   "team_member_added with role",
			change: domain.AccessChange{Action: domain.AccessActionTeamMemberAdded, TargetUserID: "u2", TeamID: "t1", DetailJSON: `{"new_role":"member"}`},
			target: "Bob",
			team:   "Platform",
			want:   "added Bob to Platform as member",
		},
		{
			name:   "team_member_added without role",
			change: domain.AccessChange{Action: domain.AccessActionTeamMemberAdded, TargetUserID: "u2", TeamID: "t1"},
			target: "Bob",
			team:   "Platform",
			want:   "added Bob to Platform",
		},
		{
			name:   "team_role_changed names the team",
			change: domain.AccessChange{Action: domain.AccessActionTeamRoleChanged, TargetUserID: "u2", TeamID: "t1", DetailJSON: `{"old_role":"member","new_role":"admin"}`},
			target: "Bob",
			team:   "Platform",
			want:   "changed Bob from member to admin on Platform",
		},
		{
			name:   "team_member_removed",
			change: domain.AccessChange{Action: domain.AccessActionTeamMemberRemoved, TargetUserID: "u2", TeamID: "t1"},
			target: "Bob",
			team:   "Platform",
			want:   "removed Bob from Platform",
		},
		{
			name:   "credential_set with host",
			change: domain.AccessChange{Action: domain.AccessActionCredentialSet, DetailJSON: `{"kind":"github_pat","host":"github.example.com"}`},
			want:   "set the GitHub PAT for github.example.com",
		},
		{
			name:   "credential_set without host (anthropic key)",
			change: domain.AccessChange{Action: domain.AccessActionCredentialSet, DetailJSON: `{"kind":"anthropic_key"}`},
			want:   "set the Anthropic API key",
		},
		{
			name:   "credential_set jira org with host",
			change: domain.AccessChange{Action: domain.AccessActionCredentialSet, DetailJSON: `{"kind":"jira_org","host":"jira.example.net"}`},
			want:   "set the Jira credential for jira.example.net",
		},
		{
			name:   "credential_set jira_user kind (per-user, no host)",
			change: domain.AccessChange{Action: domain.AccessActionCredentialSet, DetailJSON: `{"kind":"jira_user"}`},
			want:   "set the personal Jira credential",
		},
		{
			name:   "credential_set unknown kind falls back to generic",
			change: domain.AccessChange{Action: domain.AccessActionCredentialSet, DetailJSON: `{"kind":"mystery"}`},
			want:   "set the credential",
		},
		{
			name:   "credential_set bedrock (kind was previously unmapped)",
			change: domain.AccessChange{Action: domain.AccessActionCredentialSet, DetailJSON: `{"kind":"bedrock"}`},
			want:   "set the Bedrock credentials",
		},
		{
			name:   "credential_set github app names the app + host",
			change: domain.AccessChange{Action: domain.AccessActionCredentialSet, DetailJSON: `{"kind":"github_app","host":"https://github.com","name":"acme-triage"}`},
			want:   "set the GitHub App acme-triage for https://github.com",
		},
		{
			name:   "credential_set per-user github identity carries the login",
			change: domain.AccessChange{Action: domain.AccessActionCredentialSet, DetailJSON: `{"kind":"github_identity","host":"https://github.com","name":"@alice"}`},
			want:   "set the personal GitHub identity @alice for https://github.com",
		},
		{
			name:   "credential_removed reads as a removal of the same kind",
			change: domain.AccessChange{Action: domain.AccessActionCredentialRemoved, DetailJSON: `{"kind":"github_pat","host":"github.example.com"}`},
			want:   "removed the GitHub PAT for github.example.com",
		},
		{
			name:   "credential_removed github app names the torn-down app",
			change: domain.AccessChange{Action: domain.AccessActionCredentialRemoved, DetailJSON: `{"kind":"github_app","name":"acme-triage"}`},
			want:   "removed the GitHub App acme-triage",
		},
		{
			name:   "credential_removed atlassian oauth app",
			change: domain.AccessChange{Action: domain.AccessActionCredentialRemoved, DetailJSON: `{"kind":"jira_oauth_app","name":"abc123"}`},
			want:   "removed the Atlassian OAuth app abc123",
		},
		{
			name:   "invite_created names the address + granted role",
			change: domain.AccessChange{Action: domain.AccessActionInviteCreated, DetailJSON: `{"invite_id":"i1","email":"bob@example.com","role":"admin"}`},
			want:   "invited bob@example.com as admin",
		},
		{
			name:   "invite_created without a role degrades to the bare invite",
			change: domain.AccessChange{Action: domain.AccessActionInviteCreated, DetailJSON: `{"invite_id":"i1","email":"bob@example.com"}`},
			want:   "invited bob@example.com",
		},
		{
			name:   "invite_revoked names the address",
			change: domain.AccessChange{Action: domain.AccessActionInviteRevoked, DetailJSON: `{"invite_id":"i1","email":"bob@example.com"}`},
			want:   "revoked the invite for bob@example.com",
		},
		{
			name:   "invite_revoked with an unresolved address stays generic",
			change: domain.AccessChange{Action: domain.AccessActionInviteRevoked, DetailJSON: `{"invite_id":"i1"}`},
			want:   "revoked a pending invite",
		},
		{
			name:   "unresolved target falls back to a generic noun",
			change: domain.AccessChange{Action: domain.AccessActionOrgMemberRevoked, TargetUserID: "u9"},
			target: "", // GetDisplayName returned "" (a since-revoked member)
			want:   "removed a user from the org",
		},
		{
			name:   "malformed detail degrades to unknown roles, never errors",
			change: domain.AccessChange{Action: domain.AccessActionOrgRoleChanged, TargetUserID: "u2", DetailJSON: `not-json`},
			target: "Alice",
			want:   "changed Alice from an unknown role to an unknown role",
		},
		{
			name:   "unknown action shows the raw discriminator",
			change: domain.AccessChange{Action: "future_action", TargetUserID: "u2"},
			target: "Alice",
			want:   "future_action",
		},
		{
			name:   "slack workspace credential names the workspace",
			change: domain.AccessChange{Action: domain.AccessActionCredentialSet, DetailJSON: domain.AccessDetailSlackWorkspace("Acme Corp", "T01", "A01")},
			want:   "set the Slack workspace Acme Corp",
		},
		{
			name:   "slack workspace removal",
			change: domain.AccessChange{Action: domain.AccessActionCredentialRemoved, DetailJSON: domain.AccessDetailSlackWorkspace("Acme Corp", "T01", "A01")},
			want:   "removed the Slack workspace Acme Corp",
		},
		{
			name:   "sso_connection_created",
			change: domain.AccessChange{Action: domain.AccessActionSSOConnectionCreated, DetailJSON: domain.AccessDetailSSOConnection("p-1")},
			want:   "registered an SSO connection",
		},
		{
			name:   "sso_connection_disabled",
			change: domain.AccessChange{Action: domain.AccessActionSSOConnectionDisabled},
			want:   "disabled SSO",
		},
		{
			name:   "sso_enforcement_enabled",
			change: domain.AccessChange{Action: domain.AccessActionSSOEnforcementEnabled},
			want:   "started requiring SSO for this org",
		},
		{
			name:   "sso_enforcement_disabled",
			change: domain.AccessChange{Action: domain.AccessActionSSOEnforcementDisabled},
			want:   "stopped requiring SSO for this org",
		},
		{
			name:   "sso_domain_claimed names the domain",
			change: domain.AccessChange{Action: domain.AccessActionSSODomainClaimed, DetailJSON: domain.AccessDetailSSODomain("corp.example")},
			want:   "claimed the domain corp.example for SSO",
		},
		{
			name:   "sso_domain_verified names the domain",
			change: domain.AccessChange{Action: domain.AccessActionSSODomainVerified, DetailJSON: domain.AccessDetailSSODomain("corp.example")},
			want:   "verified the domain corp.example",
		},
		{
			name:   "sso_domain_removed with no detail still reads as English",
			change: domain.AccessChange{Action: domain.AccessActionSSODomainRemoved},
			want:   "removed a domain",
		},
		{
			name:   "sso_break_glass_added names the exempted principal",
			change: domain.AccessChange{Action: domain.AccessActionSSOBreakGlassAdded, TargetUserID: "u2"},
			target: "Alice",
			want:   "added Alice as an SSO break-glass principal",
		},
		{
			name:   "sso_break_glass_removed",
			change: domain.AccessChange{Action: domain.AccessActionSSOBreakGlassRemoved, TargetUserID: "u2"},
			target: "Alice",
			want:   "removed Alice from the SSO break-glass principals",
		},
		{
			name: "api_token_created carries every bound the mint recorded",
			change: domain.AccessChange{Action: domain.AccessActionAPITokenCreated, DetailJSON: domain.AccessDetailAPITokenCreated(
				"tok-1", "deploy", "tf_M2rTq8Wd", ptrTime(time.Date(2027, 6, 9, 12, 0, 0, 0, time.UTC)), ptrInt(90),
				[]string{"10.4.0.0/16", "52.14.9.20/32", "2600:1f18::/32"})},
			want: "created API token deploy (tf_M2rTq8Wd…) expiring 9 Jun 2027, under the org's 90-day cap, accepted from 3 IP ranges",
		},
		{
			name:   "api_token_created unbounded says so rather than trailing off",
			change: domain.AccessChange{Action: domain.AccessActionAPITokenCreated, DetailJSON: domain.AccessDetailAPITokenCreated("tok-1", "laptop", "tf_AbCdEfGh", nil, nil, nil)},
			want:   "created API token laptop (tf_AbCdEfGh…) with no expiry",
		},
		{
			name:   "api_token_created with one range is singular",
			change: domain.AccessChange{Action: domain.AccessActionAPITokenCreated, DetailJSON: domain.AccessDetailAPITokenCreated("tok-1", "hook", "tf_AbCdEfGh", nil, nil, []string{"10.0.0.0/8"})},
			want:   "created API token hook (tf_AbCdEfGh…) with no expiry, accepted from 1 IP range",
		},
		{
			name:   "api_token_revoked by its owner",
			change: domain.AccessChange{Action: domain.AccessActionAPITokenRevoked, ActorUserID: "u1", DetailJSON: domain.AccessDetailAPITokenRevoked("tok-1", "deploy", "tf_M2rTq8Wd", "")},
			want:   "revoked API token deploy (tf_M2rTq8Wd…)",
		},
		{
			name:   "api_token_revoked with the membership reads as the consequence",
			change: domain.AccessChange{Action: domain.AccessActionAPITokenRevoked, ActorUserID: "u1", TargetUserID: "u2", DetailJSON: domain.AccessDetailAPITokenRevoked("tok-1", "deploy", "tf_M2rTq8Wd", domain.AccessSourceMembershipRemoved)},
			target: "Bob",
			want:   "revoked Bob's API token deploy (tf_M2rTq8Wd…) with their org membership",
		},
		{
			name:   "api_token rows with no detail still read as English",
			change: domain.AccessChange{Action: domain.AccessActionAPITokenRevoked},
			want:   "revoked API token (unnamed)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := accessChangeLabel(tc.change, tc.target, tc.team); got != tc.want {
				t.Errorf("accessChangeLabel() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAccessLogList_StrictBody pins the strict-body contract on the access
// log: an unknown category is a rejected request rather than a filter that
// matches nothing, and the paging fields go through the shared resolver (so an
// out-of-range page_size is a 400, never a clamp). Every one of these used to
// default silently.
func TestAccessLogList_StrictBody(t *testing.T) {
	s := newLicensedAccessLogServer(t)
	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"page_size zero is the default, not a rejection — skip", nil},
		{"page_size negative", map[string]any{"page_size": -5}},
		{"page_size over cap", map[string]any{"page_size": 9999}},
		{"unknown category", map[string]any{"category": "membershipp"}},
		{"retired limit param", map[string]any{"limit": 10}},
		{"retired offset param", map[string]any{"offset": 10}},
	} {
		if tc.body == nil {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/usage/access-log/list", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// postAccessLog calls the access-log list and decodes the shared envelope.
func postAccessLog(t *testing.T, s *Server, body map[string]any) listEnvelope[accessChangeJSON] {
	t.Helper()
	return decodeList[accessChangeJSON](t, doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/usage/access-log/list", body))
}

// newLicensedAccessLogServer is a local-mode server with the governance
// entitlement granted, so the route is reachable.
func newLicensedAccessLogServer(t *testing.T) *Server {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeLocal)
	entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureGovernance))
	t.Cleanup(entitlements.Reset)
	return newUsageTestServer(t)
}

// --- local-mode end-to-end (SQLite, no Docker) ---

// TestUsageAccessLog_Local drives the viewer endpoint through the real router +
// withSession middleware against in-memory SQLite (local mode), with the
// governance feature licensed so the EE gate admits the request. It covers the
// 200 read: newest-first ordering, name + label rendering, the category filter,
// and limit/offset paging with has_more. The role-gate 403 is multi-mode-only
// (Postgres rig); the unlicensed 404 is its own test below.
func TestUsageAccessLog_Local(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureGovernance))
	t.Cleanup(entitlements.Reset)

	s := newUsageTestServer(t)
	seedAccessLogLocal(t, s)

	t.Run("all_categories_newest_first_with_names_and_labels", func(t *testing.T) {
		resp := postAccessLog(t, s, map[string]any{})
		if len(resp.Items) != 4 {
			t.Fatalf("items = %d, want 4: %+v", len(resp.Items), resp.Items)
		}
		if resp.NextPageToken != "" {
			t.Errorf("next_page_token = %q, want empty (4 rows < default page)", resp.NextPageToken)
		}
		if resp.Total() != 4 {
			t.Errorf("total_count = %d, want 4", resp.Total())
		}
		// Newest-first: the revoke (13:00) leads.
		first := resp.Items[0]
		if first.Action != domain.AccessActionOrgMemberRevoked {
			t.Errorf("items[0].action = %q, want %q", first.Action, domain.AccessActionOrgMemberRevoked)
		}
		if first.ActionLabel != "removed Bob from the org" {
			t.Errorf("items[0].action_label = %q, want %q", first.ActionLabel, "removed Bob from the org")
		}
		if first.ActorName != "You" {
			t.Errorf("items[0].actor_name = %q, want %q", first.ActorName, "You")
		}
		if first.TargetName != "Bob" {
			t.Errorf("items[0].target_name = %q, want %q", first.TargetName, "Bob")
		}
		// The whole rendered set, by label, proves each branch + team-name resolution.
		labels := map[string]bool{}
		for _, it := range resp.Items {
			labels[it.ActionLabel] = true
		}
		for _, want := range []string{
			"removed Bob from the org",
			"added Bob to Default as member",
			"changed Alice from member to admin",
			"set the GitHub PAT for github.example.com",
		} {
			if !labels[want] {
				t.Errorf("missing rendered label %q in %+v", want, labels)
			}
		}
		// detail_json passes through raw for the credential row.
		var sawCredentialDetail bool
		for _, it := range resp.Items {
			if it.Action == domain.AccessActionCredentialSet {
				if len(it.DetailJSON) == 0 {
					t.Errorf("credential row dropped detail_json: %+v", it)
				}
				sawCredentialDetail = true
			}
		}
		if !sawCredentialDetail {
			t.Errorf("no credential_set row found: %+v", resp.Items)
		}
	})

	t.Run("category_credential_filters_to_credential_set", func(t *testing.T) {
		resp := postAccessLog(t, s, map[string]any{"category": "credential"})
		if len(resp.Items) != 1 || resp.Items[0].Action != domain.AccessActionCredentialSet {
			t.Fatalf("category=credential items = %+v, want one credential_set", resp.Items)
		}
		if resp.Items[0].ActionLabel != "set the GitHub PAT for github.example.com" {
			t.Errorf("credential label = %q", resp.Items[0].ActionLabel)
		}
	})

	t.Run("category_membership_excludes_credentials", func(t *testing.T) {
		resp := postAccessLog(t, s, map[string]any{"category": "membership"})
		if len(resp.Items) != 3 {
			t.Fatalf("category=membership items = %d, want 3: %+v", len(resp.Items), resp.Items)
		}
		for _, it := range resp.Items {
			if it.Action == domain.AccessActionCredentialSet {
				t.Errorf("membership filter leaked a credential_set: %+v", it)
			}
		}
	})

	t.Run("unrecognized_category_is_rejected", func(t *testing.T) {
		// An unknown category used to fall through as "no filter", so a typo
		// answered with every row and looked like a working filter. It is a
		// closed vocabulary now: reject rather than guess which reading the
		// caller meant. The fault names the body field — category is a payload
		// field on this route, not a query param, so the envelope carries it.
		rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/usage/access-log/list", map[string]any{"category": "bogus"})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("category=bogus = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		assertFirstError(t, rec, httpx.ReasonInvalidField, "category")
	})

	t.Run("pages_partition_the_log", func(t *testing.T) {
		page1 := postAccessLog(t, s, map[string]any{"page_size": 2})
		if len(page1.Items) != 2 || page1.NextPageToken == "" || page1.Total() != 4 {
			t.Fatalf("page1 = %d items (total %d, token %q), want 2 rows, total 4, a next token",
				len(page1.Items), page1.Total(), page1.NextPageToken)
		}
		page2 := postAccessLog(t, s, map[string]any{"page_size": 2, "page_token": page1.NextPageToken})
		if len(page2.Items) != 2 || page2.NextPageToken != "" {
			t.Fatalf("page2 = %d items (token %q), want 2 rows and no next token", len(page2.Items), page2.NextPageToken)
		}
		// No overlap between the pages (distinct ids across the four rows).
		seen := map[string]bool{}
		for _, it := range append(page1.Items, page2.Items...) {
			if seen[it.ID] {
				t.Errorf("row %s appeared on both pages", it.ID)
			}
			seen[it.ID] = true
		}
	})
}

// TestUsageAccessLog_Unlicensed404 pins the EE gate: with no governance license
// (the community default), the route 404s even for the local implicit admin —
// the feature is hidden, not merely forbidden.
func TestUsageAccessLog_Unlicensed404(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	entitlements.Reset() // community checker: governance not licensed
	t.Cleanup(entitlements.Reset)

	s := newUsageTestServer(t)
	seedAccessLogLocal(t, s)

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/usage/access-log/list", map[string]any{})
	if rec.Code != http.StatusNotFound {
		t.Errorf("unlicensed access-log = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestUsageAccessLog_MalformedDetailDoesNotBreakResponse pins that a row whose
// detail_json isn't valid JSON degrades gracefully: the column is free-form TEXT
// (no JSON CHECK), and passing a malformed value straight into a json.RawMessage
// would fail the whole response marshal — httpx.WriteJSON has already written a
// 200 by then, so the client would get an empty body and the entire page would
// die. The handler must instead omit that one row's detail and still serve the
// page (the label, parsed leniently, still renders).
func TestUsageAccessLog_MalformedDetailDoesNotBreakResponse(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureGovernance))
	t.Cleanup(entitlements.Reset)

	s := newUsageTestServer(t)
	if _, err := s.db.Exec(`
		INSERT INTO access_change_log (id, org_id, actor_user_id, action, target_user_id, detail_json, created_at)
		VALUES ('bad-1', ?, ?, ?, 'u-x', 'not-json', '2026-06-20 10:00:00')`,
		runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, domain.AccessActionOrgRoleChanged,
	); err != nil {
		t.Fatalf("seed malformed-detail row: %v", err)
	}

	// doUsage fails the test if the status isn't 200 or the body isn't valid JSON —
	// which is exactly the pre-fix failure mode (200 + empty body), so a green
	// decode here is the regression guard.
	resp := postAccessLog(t, s, map[string]any{})
	if len(resp.Items) != 1 {
		t.Fatalf("items = %d, want 1: %+v", len(resp.Items), resp.Items)
	}
	row := resp.Items[0]
	// The corrupt detail is dropped so the response stays valid JSON...
	if len(row.DetailJSON) != 0 {
		t.Errorf("malformed detail_json passed through as %q; want it omitted", string(row.DetailJSON))
	}
	// ...and the label still renders (lenient parse → unknown-role phrasing).
	if row.ActionLabel == "" {
		t.Errorf("action_label empty for the malformed-detail row")
	}
}

// seedAccessLogLocal stages two named users (Alice, Bob) plus four governance
// audit rows on the local sentinel org, with explicit ascending timestamps so the
// newest-first read order is deterministic: a credential bind, an org role change,
// a team add, and an org revoke. The actor is the seeded local user ("You"); the
// team rows reference the local default team (named "Default").
func seedAccessLogLocal(t *testing.T, s *Server) {
	t.Helper()
	org := runmode.LocalDefaultOrgID
	actor := runmode.LocalDefaultUserID
	team := runmode.LocalDefaultTeamID
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(q, args...); err != nil {
			t.Fatalf("seed exec failed: %v\nSQL: %s", err, q)
		}
	}

	exec(`INSERT INTO users (id, display_name) VALUES ('u-alice', 'Alice'), ('u-bob', 'Bob')`)

	row := func(id, action, target, teamID, detail, when string) {
		exec(`INSERT INTO access_change_log (id, org_id, actor_user_id, action, target_user_id, team_id, detail_json, created_at)
		      VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			id, org, actor, action, nullable(target), nullable(teamID), nullable(detail), when)
	}
	row("acl-1", domain.AccessActionCredentialSet, "", "", `{"kind":"github_pat","host":"github.example.com"}`, "2026-06-20 10:00:00")
	row("acl-2", domain.AccessActionOrgRoleChanged, "u-alice", "", `{"old_role":"member","new_role":"admin"}`, "2026-06-20 11:00:00")
	row("acl-3", domain.AccessActionTeamMemberAdded, "u-bob", team, `{"new_role":"member"}`, "2026-06-20 12:00:00")
	row("acl-4", domain.AccessActionOrgMemberRevoked, "u-bob", "", "", "2026-06-20 13:00:00")
}

// TestUsageAccessLog_PolicyCategory_Local covers the policy bucket end to end:
// the SSO rows ee/sso writes are core-renderable and reachable through the
// category filter. Its own server + seed rather than an extra row in
// seedAccessLogLocal, whose subtests assert on exact row counts.
func TestUsageAccessLog_PolicyCategory_Local(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureGovernance))
	t.Cleanup(entitlements.Reset)

	s := newUsageTestServer(t)
	org := runmode.LocalDefaultOrgID
	actor := runmode.LocalDefaultUserID
	if _, err := s.db.Exec(`INSERT INTO users (id, display_name) VALUES ('u-alice', 'Alice')`); err != nil {
		t.Fatalf("seed users: %v", err)
	}
	row := func(id, action, target, detail, when string) {
		t.Helper()
		if _, err := s.db.Exec(`INSERT INTO access_change_log (id, org_id, actor_user_id, action, target_user_id, detail_json, created_at)
		      VALUES (?, ?, ?, ?, ?, ?, ?)`, id, org, actor, action, nullable(target), nullable(detail), when); err != nil {
			t.Fatalf("seed access row: %v", err)
		}
	}
	row("pol-1", domain.AccessActionOrgMemberRevoked, "u-alice", "", "2026-06-20 10:00:00")
	row("pol-2", domain.AccessActionSSOEnforcementEnabled, "", domain.AccessDetailSSOConnection("p-1"), "2026-06-20 11:00:00")
	row("pol-3", domain.AccessActionSSODomainVerified, "", domain.AccessDetailSSODomain("corp.example"), "2026-06-20 12:00:00")
	row("pol-4", domain.AccessActionSSOBreakGlassAdded, "u-alice", "", "2026-06-20 13:00:00")

	resp := postAccessLog(t, s, map[string]any{"category": "policy"})
	if len(resp.Items) != 3 {
		t.Fatalf("category=policy items = %d, want 3 (the SSO rows only): %+v", len(resp.Items), resp.Items)
	}
	labels := map[string]bool{}
	for _, it := range resp.Items {
		if it.Action == domain.AccessActionOrgMemberRevoked {
			t.Errorf("policy filter leaked a membership row: %+v", it)
		}
		labels[it.ActionLabel] = true
	}
	for _, want := range []string{
		"started requiring SSO for this org",
		"verified the domain corp.example",
		"added Alice as an SSO break-glass principal",
	} {
		if !labels[want] {
			t.Errorf("missing rendered label %q in %v", want, labels)
		}
	}
}

// nullable maps "" to a nil arg (SQL NULL) so the optional columns store NULL
// rather than an empty string, matching how the store's Record writes them.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func ptrTime(t time.Time) *time.Time { return &t }
func ptrInt(n int) *int              { return &n }
