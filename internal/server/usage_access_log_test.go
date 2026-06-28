package server

import (
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
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
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := accessChangeLabel(tc.change, tc.target, tc.team); got != tc.want {
				t.Errorf("accessChangeLabel() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseAccessLogPaging(t *testing.T) {
	limits := []struct {
		in   string
		want int
	}{
		{"", accessLogDefaultLimit},
		{"0", accessLogDefaultLimit},
		{"-5", accessLogDefaultLimit},
		{"abc", accessLogDefaultLimit},
		{"10", 10},
		{"9999", accessLogMaxLimit},
	}
	for _, tc := range limits {
		if got := parseAccessLogLimit(tc.in); got != tc.want {
			t.Errorf("parseAccessLogLimit(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
	offsets := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"-1", 0},
		{"abc", 0},
		{"5", 5},
	}
	for _, tc := range offsets {
		if got := parseAccessLogOffset(tc.in); got != tc.want {
			t.Errorf("parseAccessLogOffset(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
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
	entitlements.Register(governanceGrant{})
	t.Cleanup(entitlements.Reset)

	s := newUsageTestServer(t)
	seedAccessLogLocal(t, s)

	t.Run("all_categories_newest_first_with_names_and_labels", func(t *testing.T) {
		var resp accessLogResponse
		doUsage(t, s, "/api/usage/org/access-log", &resp)
		if len(resp.Items) != 4 {
			t.Fatalf("items = %d, want 4: %+v", len(resp.Items), resp.Items)
		}
		if resp.HasMore {
			t.Errorf("has_more = true, want false (4 rows < default page)")
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
		var resp accessLogResponse
		doUsage(t, s, "/api/usage/org/access-log?category=credential", &resp)
		if len(resp.Items) != 1 || resp.Items[0].Action != domain.AccessActionCredentialSet {
			t.Fatalf("category=credential items = %+v, want one credential_set", resp.Items)
		}
		if resp.Items[0].ActionLabel != "set the GitHub PAT for github.example.com" {
			t.Errorf("credential label = %q", resp.Items[0].ActionLabel)
		}
	})

	t.Run("category_membership_excludes_credentials", func(t *testing.T) {
		var resp accessLogResponse
		doUsage(t, s, "/api/usage/org/access-log?category=membership", &resp)
		if len(resp.Items) != 3 {
			t.Fatalf("category=membership items = %d, want 3: %+v", len(resp.Items), resp.Items)
		}
		for _, it := range resp.Items {
			if it.Action == domain.AccessActionCredentialSet {
				t.Errorf("membership filter leaked a credential_set: %+v", it)
			}
		}
	})

	t.Run("unrecognized_category_is_no_filter", func(t *testing.T) {
		// The handler passes q.Get("category") straight through; an unknown value
		// must fall back to "no filter" (every row), not an empty page.
		var resp accessLogResponse
		doUsage(t, s, "/api/usage/org/access-log?category=bogus", &resp)
		if len(resp.Items) != 4 {
			t.Fatalf("category=bogus items = %d, want all 4 (no filter): %+v", len(resp.Items), resp.Items)
		}
	})

	t.Run("paging_with_limit_offset_and_has_more", func(t *testing.T) {
		var page1 accessLogResponse
		doUsage(t, s, "/api/usage/org/access-log?limit=2&offset=0", &page1)
		if len(page1.Items) != 2 || !page1.HasMore {
			t.Fatalf("page1 = %d items has_more=%v, want 2 + true", len(page1.Items), page1.HasMore)
		}
		if page1.Limit != 2 || page1.Offset != 0 {
			t.Errorf("page1 echo = limit %d offset %d, want 2/0", page1.Limit, page1.Offset)
		}
		var page2 accessLogResponse
		doUsage(t, s, "/api/usage/org/access-log?limit=2&offset=2", &page2)
		if len(page2.Items) != 2 || page2.HasMore {
			t.Fatalf("page2 = %d items has_more=%v, want 2 + false", len(page2.Items), page2.HasMore)
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

	rec := doJSON(t, s, "GET", "/api/usage/org/access-log", nil)
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
	entitlements.Register(governanceGrant{})
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
	var resp accessLogResponse
	doUsage(t, s, "/api/usage/org/access-log", &resp)
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

// nullable maps "" to a nil arg (SQL NULL) so the optional columns store NULL
// rather than an empty string, matching how the store's Record writes them.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
