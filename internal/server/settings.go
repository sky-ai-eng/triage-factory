package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// settingsHandler serves the Jira-connect / Jira-statuses / GitHub SSH-preflight
// settings endpoints, holding the transactional store runner.
type settingsHandler struct {
	tx db.TxRunner
	// az gates the org-scoped credential routes on the {org_id} path segment.
	// Always set — the single construction site in
	// registerSettingsHandlerRoutes passes the server's checker — and
	// dereferenced unguarded by those handlers. The struct's other handlers are
	// session-scoped and resolve the org from the session instead, so they
	// never read it.
	az *authz.Checker
	// bedrockRole resolves live AWS calls for the Bedrock role-mode setup +
	// connect probe (TFAC-616): sts:GetCallerIdentity for the trust-policy
	// snippet and a real sts:AssumeRole round-trip for the connect probe. A
	// getter (not a captured value) because routes register before the app
	// injects the resolver; nil when no ambient AWS SDK is wired (local mode).
	bedrockRole func() bedrockRoleResolver
	// kickJira re-dues Jira polling after this handler binds the org's Jira
	// credential. The poller callbacks live on the Server, so the single
	// construction site in registerSettingsHandlerRoutes hands this handler the
	// one method rather than a back-reference to the whole server.
	kickJira func(r *http.Request, orgID string)
	// guardJiraWrite takes the org's Jira credential lock, which the unbind
	// and a settings save moving the Jira site take too, and prepares the
	// bind's keychain writes in local mode: the Server's guardJiraWrite, wired
	// from registerSettingsHandlerRoutes like kickJira.
	guardJiraWrite func(ctx context.Context, orgID string) (restore, unlock func(), err error)
	// guardAnthropicWrite and guardBedrockWrite do the same for the LLM
	// credential binds and unbinds: the Server's guardLocalAnthropicWrite and
	// guardLocalBedrockWrite.
	guardAnthropicWrite func(ctx context.Context, orgID string) (restore, unlock func(), err error)
	guardBedrockWrite   func(ctx context.Context, orgID string) (restore, unlock func(), err error)
	// kickMemoryBacklog rings the org-wide memory doorbell after this handler
	// binds an LLM credential: a generation that failed because the org's
	// background-jobs model resolved to nothing may succeed now, and the
	// doorbell's org form re-sweeps that backlog instead of letting each owed
	// conversation age out of the attempt backoff one at a time. One argument,
	// because these handlers structurally have no conversation to name — what
	// they changed is the whole org's ability to generate at all. Wired from
	// registerSettingsHandlerRoutes like kickJira.
	kickMemoryBacklog func(orgID string)
}

// bedrockRoleResolver is the slice of internal/cred/llmcred the Bedrock role-setup
// and connect-probe handlers call. *llmcred.Resolver satisfies it; tests
// inject a fake to exercise the two probe failure classes without real AWS.
type bedrockRoleResolver interface {
	CallerIdentityARN(ctx context.Context) (string, error)
	ProbeRole(ctx context.Context, roleARN, externalID string) error
}

// secretKeyAnthropicAPIKey is the vault key the org's Anthropic API key is
// stored under (and the value AnthropicAPIKeyRef points at). It matches the key
// the credential resolver reads (internal/agentproc) and the GET presence flag;
// named here so the connect handler isn't the third copy of a magic string.
const secretKeyAnthropicAPIKey = "anthropic_api_key"

// normalizeJiraProjectKey trims whitespace and uppercases: the canonical
// spelling of a project key, which the team write registers a project under
// when Jira answers with it.
func normalizeJiraProjectKey(s string) string {
	return strings.ToUpper(strings.TrimSpace(s))
}

// validateProjectRules enforces the per-project rule invariants.
//
// A stored project is the team's commitment to WATCH it. Being ARMED — pickup
// plus the in-progress and done rules complete, which is what it takes to
// contribute anything to the discovery JQL — is a second, later state, reached
// when someone maps the project's workflow statuses. So a project may carry no
// rules at all, and each rule is independently complete-or-empty:
//
//   - Pickup carries members and nothing else — TF never transitions a ticket
//     back into pickup, so there is no write target to pair them with, and
//     empty members is the whole of "unmapped". Nothing to check.
//   - InProgress / InReview / Done: members and canonical together, or neither.
//     The canonical is the status TF transitions a ticket INTO, so members
//     without one is a rule that cannot be executed — rejected. InReview is
//     checked identically even though it arms nothing: half a rule is no more
//     executable there than anywhere else, and a stored half is what a later
//     reader would have to guess at.
//
// The jpsr_*_complete_or_empty CHECK constraints are the DB-level mirror of
// the pairing; this is the user-facing gate that surfaces a readable error
// instead of a constraint violation. "The canonical is itself a member" stays
// here alone, because a CHECK can't subquery in either dialect.
func validateProjectRules(p domain.JiraProjectStatusRules) error {
	for _, r := range []struct {
		name      string
		members   []domain.JiraStatusRef
		canonical domain.JiraStatusRef
	}{
		{"in_progress", p.InProgressMembers, p.InProgressCanonical},
		{"in_review", p.InReviewMembers, p.InReviewCanonical},
		{"done", p.DoneMembers, p.DoneCanonical},
	} {
		hasMembers, hasCanonical := len(r.members) > 0, !r.canonical.IsZero()
		switch {
		case !hasMembers && !hasCanonical:
			// Unmapped, which is a state a watched project is allowed to be in.
			continue
		case !hasMembers:
			return fmt.Errorf("project %s: %s members are required", p.ProjectKey, r.name)
		case !hasCanonical:
			return fmt.Errorf("project %s: %s canonical is required", p.ProjectKey, r.name)
		}
		if !slices.ContainsFunc(r.members, r.canonical.SameStatus) {
			return fmt.Errorf("project %s: %s canonical %q is not in members", p.ProjectKey, r.name, r.canonical.Name)
		}
	}
	return nil
}

// armedJiraProjects returns the armed subset, order preserved. It is what
// change detection compares: watching a project without mapping it changes
// nothing the poller would ask Jira, so it must not re-due a poll.
func armedJiraProjects(in []domain.JiraProjectStatusRules) []domain.JiraProjectStatusRules {
	out := make([]domain.JiraProjectStatusRules, 0, len(in))
	for _, p := range in {
		if p.Armed() {
			out = append(out, p)
		}
	}
	return out
}

// normalizeMembers returns a sorted, deduplicated copy of members so rules can
// be compared using set semantics without mutating the original slice. Refs
// sort on their dedup key — the id when there is one — so two spellings of the
// same set compare equal regardless of the order they were picked in.
func normalizeMembers(members []domain.JiraStatusRef) []string {
	normalized := make([]string, 0, len(members))
	for _, m := range members {
		normalized = append(normalized, domain.JiraStatusDedupKey(m))
	}
	slices.Sort(normalized)
	return slices.Compact(normalized)
}

// ruleEqual compares two status rules by identity. Used by change detection to
// decide whether a Jira poller restart is needed, and by the write path to tell
// a rule that moved from one that was merely resent. Display names are NOT
// compared: a name refreshed from Jira is not a configuration change.
func ruleEqual(aMembers []domain.JiraStatusRef, aCanonical domain.JiraStatusRef,
	bMembers []domain.JiraStatusRef, bCanonical domain.JiraStatusRef) bool {
	return domain.JiraStatusDedupKey(aCanonical) == domain.JiraStatusDedupKey(bCanonical) &&
		slices.Equal(normalizeMembers(aMembers), normalizeMembers(bMembers))
}

// cloneJiraProjects returns a deep copy so the pre-change snapshot
// stays stable while the handler mutates the desired project list.
func cloneJiraProjects(in []domain.JiraProjectStatusRules) []domain.JiraProjectStatusRules {
	out := make([]domain.JiraProjectStatusRules, len(in))
	for i, p := range in {
		out[i] = p
		out[i].PickupMembers = slices.Clone(p.PickupMembers)
		out[i].InProgressMembers = slices.Clone(p.InProgressMembers)
		out[i].InReviewMembers = slices.Clone(p.InReviewMembers)
		out[i].DoneMembers = slices.Clone(p.DoneMembers)
	}
	return out
}

// jiraProjectsEqual compares two per-project lists by value, treating
// order as significant (the user-facing UI keeps projects in the order
// they were added; reordering counts as a change worth restarting the
// poller for). Projects are compared by registry row, so a key the
// registry refreshed is not a change; rules are compared with
// set-equality on Members.
//
// InReview is deliberately absent from the comparison. This function exists to
// answer one question — did anything the Jira poller asks change? — and the
// in-review rule reaches neither the discovery JQL nor any classification.
// Mapping or clearing it alone must therefore not re-due a poll, which would
// re-ask Jira for exactly what it just answered.
func jiraProjectsEqual(a, b []domain.JiraProjectStatusRules) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].JiraProjectID != b[i].JiraProjectID {
			return false
		}
		if !ruleEqual(a[i].PickupMembers, domain.JiraStatusRef{}, b[i].PickupMembers, domain.JiraStatusRef{}) ||
			!ruleEqual(a[i].InProgressMembers, a[i].InProgressCanonical, b[i].InProgressMembers, b[i].InProgressCanonical) ||
			!ruleEqual(a[i].DoneMembers, a[i].DoneCanonical, b[i].DoneMembers, b[i].DoneCanonical) {
			return false
		}
	}
	return true
}

// defaultedCloneProtocolView normalizes a stored CloneProtocol value for the
// API surface using the same effective semantics as backend clone-URL
// selection, and is mode-aware: multi-mode always reports "https"
// regardless of the stored value (SSH is unavailable there), while local mode
// treats only the literal "ssh" as SSH and everything else as HTTPS. Clients
// always see one of the two known forms, matching what the clone path will
// actually do.
func defaultedCloneProtocolView(stored string) string {
	return domain.EffectiveCloneProtocol(stored, runmode.Current() == runmode.ModeMulti)
}

// jiraStatusRefJSON is one workflow status as the API renders it: the id the
// rules are keyed on, and the display name the SERVER resolved for that id.
// A caller never supplies a name — see the write shape — so a rendered name is
// always one Jira gave us, as of the rule's last save.
type jiraStatusRefJSON struct {
	ID   string `json:"id" doc:"Jira's id for the workflow status."`
	Name string `json:"name" doc:"The status's name in Jira when the rule was last saved."`
}

// jiraStatusRuleJSON is one rule as the API renders it. canonical is null on an
// unmapped rule rather than absent, so a client reads one shape in both states
// — and always null on pickup, which has no write target to name.
type jiraStatusRuleJSON struct {
	Members   []jiraStatusRefJSON `json:"members" doc:"The workflow statuses that count as this rule's state, empty when the rule is not mapped."`
	Canonical *jiraStatusRefJSON  `json:"canonical" doc:"The status Triage Factory moves an issue into for this rule, or null when the rule is not mapped and always on pickup."`
}

// jiraProjectSettings is the per-project read shape: the project's registry
// row id, which a predicate's jira_project_id_in and the team write name it
// by, Jira's own id for it, its key and name as the registry holds them now,
// and the four rules. Armed is not a field — it is derivable from these rules
// by the SPA and a headless caller alike.
type jiraProjectSettings struct {
	ID string `json:"id" doc:"The project's id in Triage Factory."`
	// JiraID is Jira's project id, the jira_id the org's catalog serves the
	// project under, so a client matches catalog candidates against the
	// tracked set by it. "" while the poll has not resolved the registry row
	// against Jira.
	JiraID     string             `json:"jira_id" doc:"Jira's id for the project, empty until polling has matched the project in Jira."`
	Key        string             `json:"key" doc:"The project's key in Jira, such as PROJ."`
	Name       string             `json:"name" doc:"The project's name in Jira."`
	Pickup     jiraStatusRuleJSON `json:"pickup" doc:"The statuses an issue may be in for the team to pick it up."`
	InProgress jiraStatusRuleJSON `json:"in_progress" doc:"The statuses that count as in progress, and the status an issue moves into when work on it starts."`
	InReview   jiraStatusRuleJSON `json:"in_review" doc:"The statuses that count as in review, and the status an issue moves into when its work goes to review."`
	Done       jiraStatusRuleJSON `json:"done" doc:"The statuses that count as done, and the status an issue moves into when its work is complete."`
}

// rulesToProjectConfigsOrdered reorders the stored rules to match order, the
// team's registry row ids in display order (team_settings.jira_projects).
// Ids present in order but missing from rules are skipped (the store is
// the source of truth for rule existence, and a rule on another Jira site
// is not read at all); rules not in order get appended after the ordered
// set in their store-returned (key ASC) position so a manual DB poke that
// adds a row without updating team_settings.jira_projects still surfaces.
func rulesToProjectConfigsOrdered(rules []domain.JiraProjectStatusRules, order []string) []domain.JiraProjectStatusRules {
	if len(rules) == 0 {
		return nil
	}
	byID := make(map[string]domain.JiraProjectStatusRules, len(rules))
	for _, r := range rules {
		byID[r.JiraProjectID] = r
	}
	out := make([]domain.JiraProjectStatusRules, 0, len(rules))
	seen := make(map[string]bool, len(rules))
	for _, id := range order {
		r, ok := byID[id]
		if !ok || seen[id] {
			continue
		}
		out = append(out, r)
		seen[id] = true
	}
	for _, r := range rules {
		if seen[r.JiraProjectID] {
			continue
		}
		out = append(out, r)
	}
	return out
}

// toJiraProjectSettings converts the persisted rules into the read shape,
// normalizing nil member slices to empty ones so the JSON response is friendly
// to consumers (no `members:null`).
func toJiraProjectSettings(in []domain.JiraProjectStatusRules) []jiraProjectSettings {
	out := make([]jiraProjectSettings, 0, len(in))
	for _, p := range in {
		out = append(out, jiraProjectSettings{
			ID:         p.JiraProjectID,
			JiraID:     p.ProjectExternalID,
			Key:        p.ProjectKey,
			Name:       p.ProjectName,
			Pickup:     toJiraStatusRuleJSON(p.PickupMembers, domain.JiraStatusRef{}),
			InProgress: toJiraStatusRuleJSON(p.InProgressMembers, p.InProgressCanonical),
			InReview:   toJiraStatusRuleJSON(p.InReviewMembers, p.InReviewCanonical),
			Done:       toJiraStatusRuleJSON(p.DoneMembers, p.DoneCanonical),
		})
	}
	return out
}

func toJiraStatusRuleJSON(members []domain.JiraStatusRef, canonical domain.JiraStatusRef) jiraStatusRuleJSON {
	out := jiraStatusRuleJSON{Members: make([]jiraStatusRefJSON, 0, len(members))}
	for _, m := range members {
		out.Members = append(out.Members, jiraStatusRefJSON{ID: m.ID, Name: m.Name})
	}
	if !canonical.IsZero() {
		out.Canonical = &jiraStatusRefJSON{ID: canonical.ID, Name: canonical.Name}
	}
	return out
}

// jiraProjectIDsFromConfigs returns the ordered registry row ids from a
// per-project rule slice — the display order team_settings.jira_projects
// stores.
func jiraProjectIDsFromConfigs(projects []domain.JiraProjectStatusRules) []string {
	ids := make([]string, 0, len(projects))
	for _, p := range projects {
		if p.JiraProjectID != "" {
			ids = append(ids, p.JiraProjectID)
		}
	}
	return ids
}

// The org's Jira service credential is bound by one route per deployment, so a
// route's required fields are fixed and strict decoding makes sending the other
// deployment's field a 400 that names it.

// jiraCloudCredential is the body of PUT …/jira/access/credential/cloud: an
// Atlassian API token (email + token → Basic auth, REST v3).
type jiraCloudCredential struct {
	URL   string `json:"url" doc:"The Atlassian Cloud site's URL, such as https://acme.atlassian.net, which becomes the organization's Jira site."`
	Email string `json:"email" doc:"The email address of the Atlassian account the API token belongs to."`
	Token string `json:"token" doc:"An Atlassian API token for that account."`
}

// jiraDataCenterCredential is the body of PUT
// …/jira/access/credential/data-center: a personal access token (Bearer, REST
// v2).
type jiraDataCenterCredential struct {
	URL string `json:"url" doc:"The Jira Server or Data Center site's URL, which becomes the organization's Jira site."`
	PAT string `json:"pat" doc:"A personal access token issued by that site."`
}

// jiraCredentialResponse is what both Jira credential binds answer.
type jiraCredentialResponse struct {
	Status      string `json:"status" doc:"Always connected."`
	DisplayName string `json:"display_name" doc:"The display name of the Jira account the credential signs in as."`
}

// jiraBinding is what one deployment's PUT hands the shared bind: the client
// configuration it validated, the canonical site, and the secrets it stores.
// The secrets the deployment does not use are empty, and the bind deletes
// them.
type jiraBinding struct {
	cfg               jira.Config
	url               string
	cloud             bool
	pat, email, token string
}

var opJiraCloudCredentialPut = httpx.NewJSONOp[jiraCloudCredential, jiraCredentialResponse](httpx.OpSpec{
	Pattern:     "PUT /api/orgs/{org_id}/jira/access/credential/cloud",
	ID:          "replace_jira_cloud_credential",
	Summary:     "Connect the organization to Atlassian Cloud Jira",
	Description: "Checks an Atlassian account's email and API token against the Jira Cloud site, then stores them as the organization's Jira credential, replacing any stored one, sets the organization's Jira site to url, and starts a Jira poll. A credential connected to a different site must be removed with DELETE /api/orgs/{org_id}/jira/access/credential first.",
	PathParams:  map[string]string{"org_id": "The organization's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintYes},
})

var opJiraDataCenterCredentialPut = httpx.NewJSONOp[jiraDataCenterCredential, jiraCredentialResponse](httpx.OpSpec{
	Pattern:     "PUT /api/orgs/{org_id}/jira/access/credential/data-center",
	ID:          "replace_jira_data_center_credential",
	Summary:     "Connect the organization to Jira Data Center",
	Description: "Checks a personal access token against the Jira Server or Data Center site, then stores it as the organization's Jira credential, replacing any stored one, sets the organization's Jira site to url, and starts a Jira poll. A credential connected to a different site must be removed with DELETE /api/orgs/{org_id}/jira/access/credential first.",
	PathParams:  map[string]string{"org_id": "The organization's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintYes},
})

// handleJiraCloudCredentialPut binds (or rotates) the org's Jira service
// credential as an Atlassian Cloud API token. It is the Cloud half of the Jira
// credential resource — see org_credentials.go for why credentials are their
// own routes rather than fields on the bulk settings save;
// handleJiraCredentialDelete is the unbind.
//
// Org-admin gated at the handler, from the {org_id} path segment. The DB layer
// enforces it too (org_settings_update RLS), but an explicit 403 beats an
// opaque rolled-back transaction.
//
// PUT /api/orgs/{org_id}/jira/access/credential/cloud
func (se *settingsHandler) handleJiraCloudCredentialPut(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := se.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	req, ok := opJiraCloudCredentialPut.Decode(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	url := validateJiraSite(&v, req.URL)
	if req.Email == "" {
		v.Missing("email")
	}
	if req.Token == "" {
		v.Missing("token")
	}
	if v.Flush(w, http.StatusBadRequest) {
		return
	}
	resp, ok := se.bindJiraCredential(w, r, orgID, userID, jiraBinding{
		cfg: jira.CloudAPIToken(url, req.Email, req.Token), url: url, cloud: true,
		email: req.Email, token: req.Token,
	})
	if !ok {
		return
	}
	opJiraCloudCredentialPut.Write(w, resp)
}

// handleJiraDataCenterCredentialPut binds (or rotates) the org's Jira service
// credential as a Jira Server / Data Center personal access token, under the
// same gate as the Cloud bind.
//
// PUT /api/orgs/{org_id}/jira/access/credential/data-center
func (se *settingsHandler) handleJiraDataCenterCredentialPut(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := se.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	req, ok := opJiraDataCenterCredentialPut.Decode(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	url := validateJiraSite(&v, req.URL)
	if req.PAT == "" {
		v.Invalid("pat", "a personal access token is required for Jira Data Center")
	}
	if v.Flush(w, http.StatusBadRequest) {
		return
	}
	resp, ok := se.bindJiraCredential(w, r, orgID, userID, jiraBinding{
		cfg: jira.DataCenterPAT(url, req.PAT), url: url, pat: req.PAT,
	})
	if !ok {
		return
	}
	opJiraDataCenterCredentialPut.Write(w, resp)
}

// bindJiraCredential performs what both deployments' binds share: it checks
// the credential against Jira, stores it with the auth-method marker the
// system resolver (jira.Resolver.ForSystem) rebuilds the matching client from,
// sets the org's Jira site, audits the bind, and re-dues Jira polling — so what
// the user filled in, what we checked against Jira, and what the resolver will
// later do all agree. On a failure the response is already written and ok is
// false.
func (se *settingsHandler) bindJiraCredential(w http.ResponseWriter, r *http.Request, orgID, userID string, b jiraBinding) (resp jiraCredentialResponse, ok bool) {
	jiraUser, err := auth.ValidateJira(r.Context(), b.cfg)
	if err != nil {
		// A mismatched scheme (Cloud creds against a DC host, or vice versa)
		// fails validation here rather than being stored — so we never persist a
		// credential that can't authenticate. When the chosen scheme disagrees
		// with the URL's host shape, the likeliest cause is the wrong deployment
		// pick, not a bad token, so append a hint. The host shape is ONLY a hint:
		// the validation above is the real authority (custom domains exist), so
		// this never blocks a combo that actually authenticates.
		msg := err.Error()
		switch hostDeployment := jira.DeploymentForHost(b.url); {
		case b.cloud && hostDeployment == jira.DeploymentDataCenter:
			msg += " — this looks like a Data Center URL, which uses a personal access token rather than an email + API token. Double-check your Jira deployment."
		case !b.cloud && hostDeployment == jira.DeploymentCloud:
			msg += " — this looks like an Atlassian Cloud URL, which uses an email + API token rather than a personal access token. Double-check your Jira deployment."
		}
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
			Reason: httpx.ReasonUpstreamRejected, Message: msg,
		})
		return jiraCredentialResponse{}, false
	}

	method := jira.AuthMethodDCPAT
	if b.cloud {
		method = jira.AuthMethodCloudAPIToken
	}

	// In local mode the keys land in the keychain, outside the transaction, so
	// a failure later in it puts the prior keys back rather than leaving the
	// new credential half-bound — or, on a rotation, the working one replaced
	// by a credential the org never committed to.
	restore, unlock, err := se.guardJiraWrite(r.Context(), orgID)
	if err != nil {
		internalError(w, "settings", err)
		return jiraCredentialResponse{}, false
	}
	defer unlock()

	// One transaction for the credential, the host and the audit row:
	// credentials go through tx.Secrets (Postgres vault writes need claims
	// set) and the host through tx.Orgs (the org_settings and
	// org_event_sources RLS policies gate writes on admin).
	//
	// This is org-level Jira ACCESS (PAT_1) only — it deliberately does NOT
	// write the caller's per-user Jira identity. That is captured solely by
	// the dedicated bind surface (POST .../jira/identity/pat), so org access
	// and user identity stay independent even when the same token connects
	// the org.
	if err := se.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		// A rotation stays on the site its credential was issued for: a Jira
		// credential works only on its own site, and the org's site is the one
		// record of which site that is. Moving to another site is a disconnect
		// and a fresh connect.
		cur, err := tx.Orgs.GetSettings(r.Context(), orgID)
		if err != nil {
			return fmt.Errorf("load org settings: %w", err)
		}
		if cur.JiraBaseURL != b.url {
			prior, err := integrations.Load(r.Context(), tx.Secrets, orgID)
			if err != nil {
				return fmt.Errorf("load jira credential: %w", err)
			}
			if _, connected := integrations.JiraSystemConfig(prior, cur.JiraBaseURL); connected {
				return fmt.Errorf("the org's Jira credential is connected to %s: %w", cur.JiraBaseURL, errJiraSiteHasCredential)
			}
		}
		// The keys are written in the order ClearJira deletes them, the
		// unbind's order (integrations.JiraKeys). A key the chosen deployment
		// does not use is deleted, so switching between Cloud and Data Center
		// leaves no stale secret for a later read to mistake for the org's
		// auth.
		for _, kv := range [][2]string{
			{integrations.KeyJiraPAT, b.pat},
			{integrations.KeyJiraEmail, b.email},
			{integrations.KeyJiraAPIToken, b.token},
			{integrations.KeyJiraAuthMethod, string(method)},
		} {
			if kv[1] == "" {
				if _, err := tx.Secrets.Delete(r.Context(), orgID, kv[0]); err != nil {
					return fmt.Errorf("clear %s: %w", kv[0], err)
				}
				continue
			}
			if err := tx.Secrets.Put(r.Context(), orgID, kv[0], kv[1], ""); err != nil {
				return fmt.Errorf("store %s: %w", kv[0], err)
			}
		}
		if _, err := tx.Orgs.SetSourceBaseURL(r.Context(), orgID, eventsource.KindJira, b.url); err != nil {
			return fmt.Errorf("save jira base url: %w", err)
		}
		// Audit the org Jira credential bind/rotate in the same tx,
		// against the canonicalized host (see auditJiraHost).
		if err := tx.AccessChangeLog.Record(r.Context(), orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialSet,
			DetailJSON:  accessDetailCredential(domain.CredentialKindJiraOrg, auditJiraHost(b.url)),
		}); err != nil {
			return fmt.Errorf("audit credential set: %w", err)
		}
		return nil
	}); err != nil {
		if restore != nil {
			restore()
		}
		if errors.Is(err, errJiraSiteHasCredential) {
			httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
				Reason: httpx.ReasonConflict, Message: err.Error(), Field: "url",
			})
			return jiraCredentialResponse{}, false
		}
		// Log the underlying wrap-chain (SQL / vault / FK errors) for
		// operator debugging, but return a stable user-facing message
		// so we don't leak Postgres internals to API clients.
		internalError(w, "settings", fmt.Errorf("persist jira connection: %w", err))
		return jiraCredentialResponse{}, false
	}

	// Re-due Jira polling under the new credential, so an org that just
	// connected Jira does not poll nothing until its next scheduled cycle.
	se.kickJira(r, orgID)

	return jiraCredentialResponse{Status: "connected", DisplayName: jiraUser.DisplayName}, true
}

// errJiraSiteHasCredential refuses moving an org's Jira site while its Jira
// service credential is stored: the credential works only on the site that
// issued it, and the org's site is the one record of which site that is.
var errJiraSiteHasCredential = errors.New("a Jira credential only works on the Jira site that issued it — disconnect it before changing the Jira URL")

// validateJiraSite checks a Jira credential body's url and returns it in the
// canonical form the org's site is stored and compared in, recording a fault
// on v when it is missing or is not an http(s) URL.
func validateJiraSite(v *httpx.Validation, raw string) string {
	if strings.TrimSpace(raw) == "" {
		v.Missing("url")
		return ""
	}
	site, ok := jira.CanonicalHost(raw)
	if !ok {
		v.Invalid("url", "must be an http or https URL")
		return ""
	}
	return site
}

// sshPreflightResponse is the verdict of an SSH preflight. Stderr is set on a
// failed probe only.
type sshPreflightResponse struct {
	OK     bool   `json:"ok" doc:"Whether this machine signed in to GitHub over SSH."`
	Stderr string `json:"stderr,omitempty" doc:"The ssh command's output with a hint about the likely cause, present only when ok is false."`
	Host   string `json:"host" doc:"The SSH host that was tried, derived from the organization's GitHub URL."`
}

var opGitHubPreflightSSH = httpx.NewJSONOp[httpx.NoBody, sshPreflightResponse](httpx.OpSpec{
	Pattern:     "POST /api/github/preflight-ssh",
	ID:          "preflight_github_ssh",
	Summary:     "Test this machine's SSH access to GitHub",
	Description: "Runs ssh against the organization's GitHub host and reports whether the machine's SSH key signed in, adding the host's key to known_hosts if it is not there yet. A probe that fails is reported with ok false rather than as an error. Local mode only.",
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintNo, Idempotent: httpx.HintYes, OpenWorld: httpx.HintYes},
})

// handleGitHubPreflightSSH probes whether the user's machine can
// authenticate to GitHub over SSH (key + agent + known_hosts all
// usable). Powers the Setup wizard's gating UX and the Settings
// page's "Test SSH connection" button. Always returns HTTP 200 — the
// body's "ok" flag is the verdict — so the client can distinguish
// "preflight reported failure" from "the server itself errored".
//
// Logs both the success path and the failure stderr to the daemon's
// log so users investigating issues see the exact ssh output even
// when the UI only renders the friendly summary.
func (se *settingsHandler) handleGitHubPreflightSSH(w http.ResponseWriter, r *http.Request) {
	// SSH is local-mode-only. PreflightSSH writes the container's
	// ~/.ssh/known_hosts (accept-new) and probes the operator's ssh-agent —
	// neither exists in a multi-mode container, and the clone path
	// there is hardwired to HTTPS. Refuse rather than run the probe so no SSH
	// machinery is ever touched in multi mode. The UI hides the button there;
	// this is the defense-in-depth backstop.
	if runmode.Current() != runmode.ModeLocal {
		// A route that doesn't exist in this deployment mode is a plain 404
		// in the envelope — not a verdict body, which is reserved for a
		// probe that actually ran.
		notFound(w, "route")
		return
	}
	// Probe target tracks the org's GitHub base URL so the Test SSH button on
	// the Settings page works for GHE deployments. Read under the request's
	// claims, like the rest of the settings surface.
	orgID := OrgIDFrom(r.Context())
	userID := ClaimsFrom(r.Context()).Subject
	var orgSet domain.OrgSettings
	if err := se.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var lerr error
		orgSet, lerr = tx.Orgs.GetSettings(r.Context(), orgID)
		return lerr
	}); err != nil {
		internalError(w, "settings", fmt.Errorf("load org settings: %w", err))
		return
	}
	if orgSet.GitHubBaseURL == "" {
		writeNotConfigured(w, "set this organization's GitHub URL before testing SSH")
		return
	}
	sshHost := worktree.SSHHostFromBaseURL(orgSet.GitHubBaseURL)

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if err := worktree.PreflightSSH(ctx, sshHost); err != nil {
		settingsLog.Warn("ssh preflight failed", "ssh_host", sshHost, "error", err)
		opGitHubPreflightSSH.Write(w, sshPreflightResponse{OK: false, Stderr: err.Error(), Host: sshHost})
		return
	}
	settingsLog.Info("ssh preflight ok", "ssh_host", sshHost)
	opGitHubPreflightSSH.Write(w, sshPreflightResponse{OK: true, Host: sshHost})
}

func (s *Server) registerSettingsHandlerRoutes() {
	se := &settingsHandler{
		tx: s.tx, az: s.az,
		bedrockRole:         func() bedrockRoleResolver { return s.bedrockRole },
		kickJira:            s.kickJiraChanged,
		guardJiraWrite:      s.guardJiraWrite,
		guardAnthropicWrite: s.guardLocalAnthropicWrite,
		guardBedrockWrite:   s.guardLocalBedrockWrite,
		kickMemoryBacklog:   func(orgID string) { s.kickMemoryOwed(orgID, "") },
	}
	s.op(opGitHubPreflightSSH, se.handleGitHubPreflightSSH)
	// Bedrock role-mode setup: returns the control service's caller ARN + the
	// TF-generated External ID so the UI can render a filled trust-policy
	// snippet. POST (not GET) because first entry generates + persists the
	// External ID — a state change that needs CSRF protection, per the
	// mutating-verb convention (route_auth_test enforces GET ≠ apiMutating).
	s.op(opBedrockRoleSetup, se.handleBedrockRoleSetup)

	// The org's LLM provider credential — one resource per credential SHAPE,
	// so a route's required fields are fixed and a blank secret never selects
	// a second behaviour. Rotation is the PUT with a new value; removal is the
	// DELETE. An org may hold both providers at once: a bind replaces only
	// material of its own provider, and says so in the response. See
	// llm_credentials.go.
	s.op(opAnthropicPut, se.handleAnthropicPut)
	s.op(opAnthropicDelete, se.handleAnthropicDelete)
	s.op(opBedrockAccessKeysPut, se.handleBedrockAccessKeysPut)
	s.op(opBedrockBearerPut, se.handleBedrockBearerPut)
	s.op(opBedrockRolePut, se.handleBedrockRolePut)
	s.op(opBedrockDelete, se.handleBedrockDelete)

	// The org's Jira credential — the Jira half of the addressable-resource
	// pair whose rationale sits with the GitHub PAT routes in
	// registerGitHubRoutes. A bind is one route per deployment; the unbind is
	// one route for both.
	s.op(opJiraCloudCredentialPut, se.handleJiraCloudCredentialPut)
	s.op(opJiraDataCenterCredentialPut, se.handleJiraDataCenterCredentialPut)
	s.op(opJiraCredentialDelete, s.handleJiraCredentialDelete)
}
