package main

import (
	"bytes"
	"database/sql"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/server"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

var updateAPISpec = flag.Bool("update-api-spec", false, "write api/openapi.json from the declared operations instead of comparing against it")

const apiSpecPath = "api/openapi.json"

// notInSpec are the /api routes no API client calls: a browser navigates to
// them and they answer with a redirect or an HTML page, or only a third party
// calls them. The list is complete: no route is added to it.
var notInSpec = map[string]string{
	"GET /api/auth/oauth/{provider}":                      "starts a browser sign-in with a redirect to the provider",
	"GET /api/auth/callback":                              "the provider's redirect back after a sign-in",
	"GET /api/github/managed/callback":                    "GitHub's redirect back after installing the deployment's App",
	"GET /api/orgs/{org_id}/github/app/register/launch":   "an HTML page that posts the App manifest to GitHub",
	"GET /api/orgs/{org_id}/github/app/register/callback": "GitHub's redirect back after registering an App",
	"GET /api/orgs/{org_id}/github/connect/start":         "starts the browser's GitHub connection with a redirect",
	"GET /api/orgs/{org_id}/github/connect/callback":      "GitHub's redirect back after connecting",
	"GET /api/orgs/{org_id}/jira/connect/start":           "starts the browser's Jira connection with a redirect",
	"GET /api/orgs/{org_id}/jira/connect/callback":        "Atlassian's redirect back after connecting",
	"GET /api/orgs/{org_id}/linear/install/start":         "starts the browser's Linear install with a redirect",
	"GET /api/linear/install/callback":                    "Linear's redirect back after installing",
	"GET /api/orgs/{org_id}/linear/connect/start":         "starts the browser's Linear connection with a redirect",
	"GET /api/linear/connect/callback":                    "Linear's redirect back after connecting",
	"GET /api/orgs/{org_id}/slack/install/start":          "starts the browser's Slack install with a redirect",
	"GET /api/slack/install/callback":                     "Slack's redirect back after installing",
	"GET /api/sso/connection/test":                        "starts a browser sign-in that tests an SSO connection",
	"POST /api/webhooks/github/{org_id}":                  "GitHub's webhook deliveries to an organization's App",
	"POST /api/webhooks/github":                           "GitHub's webhook deliveries to the deployment's App",
	"POST /api/webhooks/slack/{org_id}":                   "Slack's event deliveries to an organization's app",
	"POST /api/webhooks/slack":                            "Slack's event deliveries to the deployment's app",
	"/api/":                                               "the JSON 404 for every unmatched /api path",
}

// unannotated are the /api routes still mounted without a declared
// operation. A route leaves this list when it gains one, and nothing is added
// to it: a new route is declared.
var unannotated = []string{
	"POST /api/auth/logout",
	"POST /api/auth/logout/all",
	"POST /api/bedrock/role-setup",
	"POST /api/blueprint-runs/list",
	"GET /api/blueprint-runs/{id}",
	"POST /api/blueprint-steps/list",
	"POST /api/blueprints",
	"POST /api/blueprints/duplicate",
	"POST /api/blueprints/list",
	"GET /api/blueprints/{id}",
	"PUT /api/blueprints/{id}",
	"DELETE /api/blueprints/{id}",
	"POST /api/blueprints/{id}/merge",
	"POST /api/blueprints/{id}/reconnect",
	"POST /api/blueprints/{id}/split",
	"PUT /api/blueprints/{id}/steps",
	"GET /api/config",
	"GET /api/entitlements",
	"POST /api/event-handlers",
	"POST /api/event-handlers/list",
	"PUT /api/event-handlers/reorder",
	"GET /api/event-handlers/{id}",
	"PATCH /api/event-handlers/{id}",
	"DELETE /api/event-handlers/{id}",
	"GET /api/event-schemas",
	"GET /api/event-schemas/{event_type}",
	"GET /api/event-types",
	"GET /api/fleet/backlog",
	"GET /api/fleet/claims/{id}",
	"GET /api/fleet/claims/{id}/series",
	"GET /api/fleet/instances",
	"POST /api/fleet/instances/{id}/drain",
	"GET /api/fleet/instances/{id}/sandboxes",
	"GET /api/fleet/overview",
	"GET /api/fleet/placement",
	"GET /api/fleet/queue",
	"GET /api/fleet/timeseries",
	"POST /api/github/preflight-ssh",
	"POST /api/github/reachability",
	"POST /api/github/repos/list",
	"POST /api/github/repos/refresh",
	"GET /api/integrations/status",
	"POST /api/invites",
	"POST /api/invites/accept",
	"POST /api/invites/list",
	"GET /api/invites/preview",
	"GET /api/invites/{id}",
	"POST /api/invites/{id}/revoke",
	"POST /api/jira/reachability",
	"POST /api/marketplace/listings",
	"GET /api/marketplace/listings/by-source/{source_id}",
	"POST /api/marketplace/listings/list",
	"GET /api/marketplace/listings/{id}",
	"POST /api/marketplace/listings/{id}/delist",
	"POST /api/marketplace/listings/{id}/install",
	"POST /api/marketplace/listings/{id}/relist",
	"POST /api/marketplace/listings/{id}/versions",
	"PUT /api/marketplace/listings/{id}/vote",
	"DELETE /api/marketplace/listings/{id}/vote",
	"GET /api/me",
	"POST /api/me/active-org",
	"GET /api/me/activity",
	"GET /api/me/identities",
	"POST /api/me/notifications/dismiss",
	"POST /api/me/notifications/facets",
	"POST /api/me/notifications/list",
	"POST /api/me/notifications/mark-read",
	"GET /api/me/notifications/{id}",
	"PATCH /api/me/notifications/{id}",
	"POST /api/me/notifications/{id}/actions/{action_id}",
	"GET /api/me/settings",
	"PATCH /api/me/settings",
	"POST /api/me/tokens",
	"POST /api/me/tokens/list",
	"PATCH /api/me/tokens/{id}",
	"DELETE /api/me/tokens/{id}",
	"POST /api/orgs",
	"GET /api/orgs/{org_id}",
	"GET /api/orgs/{org_id}/api-token-policy",
	"GET /api/orgs/{org_id}/github/app",
	"DELETE /api/orgs/{org_id}/github/app",
	"POST /api/orgs/{org_id}/github/app/cutover",
	"GET /api/orgs/{org_id}/github/app/cutover-preflight",
	"POST /api/orgs/{org_id}/github/app/disconnect",
	"POST /api/orgs/{org_id}/github/app/import",
	"GET /api/orgs/{org_id}/github/app/install-url",
	"POST /api/orgs/{org_id}/github/app/installations/refresh",
	"POST /api/orgs/{org_id}/github/app/webhook/replay",
	"GET /api/orgs/{org_id}/github/commit-identity",
	"GET /api/orgs/{org_id}/github/commit-signer",
	"POST /api/orgs/{org_id}/github/commit-signer",
	"PATCH /api/orgs/{org_id}/github/commit-signer",
	"DELETE /api/orgs/{org_id}/github/commit-signer",
	"POST /api/orgs/{org_id}/github/commit-signer/rotate",
	"POST /api/orgs/{org_id}/github/commit-signer/verify",
	"GET /api/orgs/{org_id}/github/dependabot-access",
	"POST /api/orgs/{org_id}/github/grant/reach-without-purpose/list",
	"POST /api/orgs/{org_id}/github/grant/scope-drift/list",
	"GET /api/orgs/{org_id}/github/identity",
	"POST /api/orgs/{org_id}/github/identity/pat",
	"POST /api/orgs/{org_id}/github/managed/connect-account",
	"POST /api/orgs/{org_id}/github/managed/disconnect",
	"POST /api/orgs/{org_id}/github/managed/installations/{installation_id}/disconnect",
	"PUT /api/orgs/{org_id}/github/pat",
	"DELETE /api/orgs/{org_id}/github/pat",
	"POST /api/orgs/{org_id}/github/pat/preflight",
	"POST /api/orgs/{org_id}/github/pat/switch-to",
	"GET /api/orgs/{org_id}/github/users/by-name/{login}",
	"GET /api/orgs/{org_id}/github/users/{github_user_id}",
	"GET /api/orgs/{org_id}/integrations/rate-limits",
	"PUT /api/orgs/{org_id}/jira/access/credential",
	"DELETE /api/orgs/{org_id}/jira/access/credential",
	"GET /api/orgs/{org_id}/jira/app",
	"POST /api/orgs/{org_id}/jira/app",
	"DELETE /api/orgs/{org_id}/jira/app",
	"GET /api/orgs/{org_id}/jira/identity",
	"POST /api/orgs/{org_id}/jira/identity/pat",
	"POST /api/orgs/{org_id}/jira/priorities/list",
	"GET /api/orgs/{org_id}/jira/priorities/{priority_id}",
	"POST /api/orgs/{org_id}/jira/projects/list",
	"GET /api/orgs/{org_id}/jira/projects/{jira_id}",
	"POST /api/orgs/{org_id}/jira/projects/{jira_id}/issue-types/list",
	"GET /api/orgs/{org_id}/jira/projects/{jira_id}/issue-types/{issue_type_id}",
	"POST /api/orgs/{org_id}/jira/projects/{jira_id}/statuses/list",
	"GET /api/orgs/{org_id}/jira/projects/{jira_id}/statuses/{status_id}",
	"GET /api/orgs/{org_id}/linear/access",
	"PUT /api/orgs/{org_id}/linear/access/credential",
	"DELETE /api/orgs/{org_id}/linear/access/credential",
	"GET /api/orgs/{org_id}/linear/access/team-visibility",
	"GET /api/orgs/{org_id}/linear/app",
	"POST /api/orgs/{org_id}/linear/app",
	"DELETE /api/orgs/{org_id}/linear/app",
	"GET /api/orgs/{org_id}/linear/identity",
	"DELETE /api/orgs/{org_id}/linear/identity",
	"POST /api/orgs/{org_id}/linear/identity/api-key",
	"POST /api/orgs/{org_id}/linear/teams/list",
	"GET /api/orgs/{org_id}/linear/teams/{linear_team_id}",
	"POST /api/orgs/{org_id}/linear/teams/{linear_team_id}/labels/list",
	"GET /api/orgs/{org_id}/linear/teams/{linear_team_id}/labels/{label_id}",
	"POST /api/orgs/{org_id}/linear/teams/{linear_team_id}/states/list",
	"GET /api/orgs/{org_id}/linear/teams/{linear_team_id}/states/{state_id}",
	"POST /api/orgs/{org_id}/linear/users/list",
	"GET /api/orgs/{org_id}/linear/users/{linear_user_id}",
	"PUT /api/orgs/{org_id}/llm/anthropic",
	"DELETE /api/orgs/{org_id}/llm/anthropic",
	"DELETE /api/orgs/{org_id}/llm/bedrock",
	"PUT /api/orgs/{org_id}/llm/bedrock/access-keys",
	"PUT /api/orgs/{org_id}/llm/bedrock/bearer",
	"PUT /api/orgs/{org_id}/llm/bedrock/role",
	"POST /api/orgs/{org_id}/members/list",
	"PATCH /api/orgs/{org_id}/members/{user_id}",
	"DELETE /api/orgs/{org_id}/members/{user_id}",
	"GET /api/orgs/{org_id}/models",
	"POST /api/orgs/{org_id}/models/tests",
	"POST /api/orgs/{org_id}/models/{model_key}/test",
	"POST /api/orgs/{org_id}/notifications/facets",
	"POST /api/orgs/{org_id}/notifications/list",
	"GET /api/orgs/{org_id}/settings",
	"PATCH /api/orgs/{org_id}/settings",
	"POST /api/orgs/{org_id}/slack/connections/import",
	"GET /api/orgs/{org_id}/slack/connections/{workspace_id}/{api_app_id}",
	"DELETE /api/orgs/{org_id}/slack/connections/{workspace_id}/{api_app_id}",
	"POST /api/orgs/{org_id}/slack/connections/{workspace_id}/{api_app_id}/activate",
	"GET /api/orgs/{org_id}/slack/connections/{workspace_id}/{api_app_id}/activation-preview",
	"GET /api/orgs/{org_id}/slack/manifest",
	"GET /api/orgs/{org_id}/sources",
	"GET /api/orgs/{org_id}/sources/{kind}",
	"PATCH /api/orgs/{org_id}/sources/{kind}",
	"POST /api/orgs/{org_id}/teams/list",
	"POST /api/orgs/{org_id}/transfer-ownership",
	"POST /api/orgs/{org_id}/usage/access-log/list",
	"POST /api/orgs/{org_id}/usage/actions/facets",
	"POST /api/orgs/{org_id}/usage/actions/list",
	"POST /api/orgs/{org_id}/usage/artifacts/facets",
	"POST /api/orgs/{org_id}/usage/artifacts/list",
	"GET /api/orgs/{org_id}/usage/ops",
	"POST /api/orgs/{org_id}/usage/team-caps/list",
	"POST /api/prompts",
	"POST /api/prompts/from-disk",
	"POST /api/prompts/list",
	"POST /api/prompts/upload",
	"GET /api/prompts/{id}",
	"PUT /api/prompts/{id}",
	"DELETE /api/prompts/{id}",
	"GET /api/prompts/{id}/stats",
	"POST /api/repos",
	"GET /api/repos/by-name/{owner}/{repo}",
	"POST /api/repos/list",
	"GET /api/repos/{id}",
	"POST /api/repos/{id}/branches/list",
	"GET /api/slack/deployment-app",
	"GET /api/sso/break-glass",
	"POST /api/sso/break-glass",
	"DELETE /api/sso/break-glass/{user_id}",
	"GET /api/sso/connection",
	"POST /api/sso/connection",
	"PATCH /api/sso/connection",
	"PATCH /api/sso/connection/enforcement",
	"POST /api/sso/discover",
	"GET /api/sso/domains",
	"POST /api/sso/domains",
	"DELETE /api/sso/domains/{id}",
	"POST /api/sso/domains/{id}/verify",
	"POST /api/teams",
	"POST /api/teams/archived/list",
	"POST /api/teams/list",
	"GET /api/teams/{team_id}",
	"PATCH /api/teams/{team_id}",
	"GET /api/teams/{team_id}/activity",
	"POST /api/teams/{team_id}/archive",
	"GET /api/teams/{team_id}/archive/preview",
	"GET /api/teams/{team_id}/github-groups",
	"PUT /api/teams/{team_id}/github-groups",
	"POST /api/teams/{team_id}/github-groups/list",
	"GET /api/teams/{team_id}/github-repos",
	"PUT /api/teams/{team_id}/github-repos",
	"PUT /api/teams/{team_id}/jira-projects",
	"POST /api/teams/{team_id}/kb/files",
	"POST /api/teams/{team_id}/kb/list",
	"POST /api/teams/{team_id}/kb/move",
	"GET /api/teams/{team_id}/kb/{path...}",
	"DELETE /api/teams/{team_id}/kb/{path...}",
	"PUT /api/teams/{team_id}/linear-teams",
	"POST /api/teams/{team_id}/members",
	"POST /api/teams/{team_id}/members/list",
	"PATCH /api/teams/{team_id}/members/{user_id}",
	"GET /api/teams/{team_id}/models",
	"POST /api/teams/{team_id}/prs/list",
	"POST /api/teams/{team_id}/restore",
	"GET /api/teams/{team_id}/settings",
	"PATCH /api/teams/{team_id}/settings",
	"PUT /api/teams/{team_id}/slack-channels",
	"POST /api/teams/{team_id}/slack-channels/facets",
	"POST /api/teams/{team_id}/slack-channels/list",
	"GET /api/teams/{team_id}/slack-channels/{channel_id}",
	"PATCH /api/teams/{team_id}/slack-channels/{channel_id}",
	"POST /api/teams/{team_id}/usage/actions/facets",
	"POST /api/teams/{team_id}/usage/actions/list",
	"POST /api/teams/{team_id}/usage/artifacts/facets",
	"POST /api/teams/{team_id}/usage/artifacts/list",
	"PUT /api/teams/{team_id}/usage/cap",
}

// TestAPISpec checks every declared operation, that every /api route has one
// or is listed above, and that api/openapi.json is what the operations render.
// It builds the server in package main because only package main installs the
// ee extensions, whose routes are part of the API.
func TestAPISpec(t *testing.T) {
	database, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	database.SetMaxOpenConns(1)
	if err := db.BootstrapSchemaForTest(database); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	routes := server.New(database, sqlitestore.New(database)).RouteTable()

	var entries []httpx.SpecEntry
	mounted := map[string]httpx.Operation{}
	ids := map[string]string{}
	for _, route := range routes {
		mounted[route.Pattern] = route.Op
		if route.Op == nil {
			continue
		}
		entries = append(entries, httpx.SpecEntry{Op: route.Op, Raw: route.Raw})
		spec := route.Op.Spec()
		switch {
		case spec.Summary == "":
			t.Errorf("%s: Summary is empty", spec.Pattern)
		case len([]rune(spec.Summary)) > 60:
			t.Errorf("%s: Summary %q is longer than 60 characters", spec.Pattern, spec.Summary)
		case strings.HasSuffix(spec.Summary, "."):
			t.Errorf("%s: Summary %q ends with a period", spec.Pattern, spec.Summary)
		}
		if spec.Description == "" {
			t.Errorf("%s: Description is empty", spec.Pattern)
		}
		for name, doc := range spec.PathParams {
			if doc == "" {
				t.Errorf("%s: path parameter %q has no doc", spec.Pattern, name)
			}
		}
		for _, q := range spec.Query {
			if q.Doc == "" {
				t.Errorf("%s: query parameter %q has no doc", spec.Pattern, q.Name)
			}
		}
		for _, part := range spec.Form {
			if part.Doc == "" {
				t.Errorf("%s: form part %q has no doc", spec.Pattern, part.Name)
			}
		}
		if prev, dup := ids[spec.ID]; dup {
			t.Errorf("%s and %s share the operation ID %q", prev, spec.Pattern, spec.ID)
		}
		ids[spec.ID] = spec.Pattern
	}

	listed := map[string]bool{}
	for pattern := range notInSpec {
		listed[pattern] = true
	}
	for _, pattern := range unannotated {
		if listed[pattern] {
			t.Errorf("%s is listed in both notInSpec and unannotated", pattern)
		}
		listed[pattern] = true
	}
	for _, route := range routes {
		if route.Op == nil && isAPIPattern(route.Pattern) && !listed[route.Pattern] {
			t.Errorf("%s is mounted without an operation; declare one with httpx.NewJSONOp or its siblings and mount it with op", route.Pattern)
		}
	}
	for pattern := range listed {
		op, ok := mounted[pattern]
		switch {
		case !ok:
			t.Errorf("%s is listed but no route is mounted at it; remove it from the list", pattern)
		case op != nil:
			t.Errorf("%s is declared as an operation; remove it from the list", pattern)
		}
	}

	doc, problems := httpx.OpenAPI(entries)
	for _, p := range problems {
		t.Error(p)
	}
	if *updateAPISpec {
		if err := os.MkdirAll(filepath.Dir(apiSpecPath), 0o755); err != nil {
			t.Fatalf("create %s: %v", filepath.Dir(apiSpecPath), err)
		}
		if err := os.WriteFile(apiSpecPath, doc, 0o644); err != nil {
			t.Fatalf("write %s: %v", apiSpecPath, err)
		}
		return
	}
	stored, err := os.ReadFile(apiSpecPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read %s: %v", apiSpecPath, err)
	}
	if !bytes.Equal(stored, doc) {
		t.Errorf("%s does not match the declared operations; run ./scripts/lint.sh --fix to regenerate it", apiSpecPath)
	}
}

// isAPIPattern reports whether a mux pattern serves a path under /api/.
func isAPIPattern(pattern string) bool {
	_, path, found := strings.Cut(pattern, " ")
	if !found {
		path = pattern
	}
	return strings.HasPrefix(path, "/api/")
}
