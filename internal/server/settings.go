package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
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
	// Always set — the single construction site in routes() passes the server's
	// checker — and dereferenced unguarded by those handlers. The struct's other
	// handlers are session-scoped and resolve the org from the session instead,
	// so they never read it.
	az *authz.Checker
	// bedrockRole resolves live AWS calls for the Bedrock role-mode setup +
	// connect probe (TFAC-616): sts:GetCallerIdentity for the trust-policy
	// snippet and a real sts:AssumeRole round-trip for the connect probe. A
	// getter (not a captured value) because routes register before the app
	// injects the resolver; nil when no ambient AWS SDK is wired (local mode).
	bedrockRole func() bedrockRoleResolver
	// kickJira re-dues Jira polling after this handler binds the org's Jira
	// credential. The poller callbacks live on the Server, so the single
	// construction site in routes() hands this handler the one method rather
	// than a back-reference to the whole server.
	kickJira func(r *http.Request, orgID string)
}

// bedrockRoleResolver is the slice of internal/llmcred the Bedrock role-setup
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

// jiraProjectKeyRe matches Jira's standard project-key rule: a
// leading uppercase letter followed by uppercase letters or digits.
// Keys arriving through the API are uppercased before matching so
// users typing the key in lowercase land on the same canonical form
// as Jira's wire-side representation.
var jiraProjectKeyRe = regexp.MustCompile(`^[A-Z][A-Z0-9]*$`)

// normalizeJiraProjectKey trims whitespace and uppercases. Used at
// the HTTP boundary in handleTeamJiraProjectsPut (the write path) and in
// validateTrackerKeys (the read/compare path) so lookups match
// regardless of how the user typed the key.
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
// poller for). Rules are compared with set-equality on Members.
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
		if a[i].ProjectKey != b[i].ProjectKey {
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
	ID   string `json:"id"`
	Name string `json:"name"`
}

// jiraStatusRuleJSON is one rule as the API renders it. canonical is null on an
// unmapped rule rather than absent, so a client reads one shape in both states
// — and always null on pickup, which has no write target to name.
type jiraStatusRuleJSON struct {
	Members   []jiraStatusRefJSON `json:"members"`
	Canonical *jiraStatusRefJSON  `json:"canonical"`
}

// jiraProjectSettings is the per-project read shape: the key plus the four
// rules. Armed is not a field — it is derivable from these rules by the SPA
// and a headless caller alike.
type jiraProjectSettings struct {
	Key        string             `json:"key"`
	Pickup     jiraStatusRuleJSON `json:"pickup"`
	InProgress jiraStatusRuleJSON `json:"in_progress"`
	InReview   jiraStatusRuleJSON `json:"in_review"`
	Done       jiraStatusRuleJSON `json:"done"`
}

// rulesToProjectConfigsOrdered reorders the stored rules to match order.
// Keys present in order but missing from rules are skipped (the store is
// the source of truth for rule existence); keys in rules but not in order
// get appended after the ordered set in their store-returned (ASC)
// position so a manual DB poke that adds a row without updating
// team_settings.jira_projects still surfaces.
func rulesToProjectConfigsOrdered(rules []domain.JiraProjectStatusRules, order []string) []domain.JiraProjectStatusRules {
	if len(rules) == 0 {
		return nil
	}
	byKey := make(map[string]domain.JiraProjectStatusRules, len(rules))
	for _, r := range rules {
		byKey[r.ProjectKey] = r
	}
	out := make([]domain.JiraProjectStatusRules, 0, len(rules))
	seen := make(map[string]bool, len(rules))
	for _, k := range order {
		r, ok := byKey[k]
		if !ok {
			continue
		}
		out = append(out, r)
		seen[k] = true
	}
	for _, r := range rules {
		if seen[r.ProjectKey] {
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
			Key:        p.ProjectKey,
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

// projectKeysFromConfigs returns the ordered project keys from a
// per-project rule slice with empty entries filtered out.
func projectKeysFromConfigs(projects []domain.JiraProjectStatusRules) []string {
	keys := make([]string, 0, len(projects))
	for _, p := range projects {
		if p.ProjectKey != "" {
			keys = append(keys, p.ProjectKey)
		}
	}
	return keys
}

// The org's Jira service credential body, one struct per deployment. The
// deployment is an EXPLICIT discriminator rather than something inferred from
// which fields came back non-empty: inference made a half-filled Cloud form and
// a Data Center bind indistinguishable at the point where it mattered, and it
// meant the other flavor's fields were accepted and dropped. Two structs behind
// a strict decode make sending `pat` to a Cloud bind a 400 that names the
// field.
//
// The values are jira.Deployment's, so the discriminator a client sends is the
// same string the status read hands back in jira_deployment — one spelling for
// one concept across the surface.
type jiraCredentialDeployment struct {
	Deployment string `json:"deployment"`
}

// jiraCloudCredential is deployment "cloud": an Atlassian API token (email +
// token → Basic auth, REST v3).
type jiraCloudCredential struct {
	Deployment string `json:"deployment"`
	URL        string `json:"url"`
	Email      string `json:"email"`
	Token      string `json:"token"`
}

// jiraDataCenterCredential is deployment "data_center": a personal access
// token (Bearer, REST v2).
type jiraDataCenterCredential struct {
	Deployment string `json:"deployment"`
	URL        string `json:"url"`
	PAT        string `json:"pat"`
}

// handleJiraConnect binds (or rotates) the org's Jira service credential. It is
// the Jira half of the credential-resource pair — see org_credentials.go for
// why credentials are their own routes rather than fields on the bulk settings
// save; handleJiraCredentialDelete is the unbind.
//
// The caller names its deployment and supplies exactly that deployment's
// fields. The chosen scheme is recorded as an auth-method marker so the system
// resolver (jira.Resolver.ForSystem) rebuilds the matching client, and it is
// what the credential is validated under — so what the user filled in, what we
// checked against Jira, and what the resolver will later do all agree.
//
// Org-admin gated at the handler, from the {org_id} path segment. The DB layer
// enforces it too (org_settings_update RLS), but an explicit 403 beats an
// opaque rolled-back transaction.
//
// PUT /api/orgs/{org_id}/jira/access/credential
func (se *settingsHandler) handleJiraConnect(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := se.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	body, ok := httpx.BodyBytes(w, r)
	if !ok {
		return
	}
	deployment, ok := decodeJiraDeployment(w, body)
	if !ok {
		return
	}

	var (
		cfg   jira.Config
		url   string
		cloud = deployment == jira.DeploymentCloud
		pat   string
		email string
		token string
	)
	if cloud {
		var req jiraCloudCredential
		if !httpx.DecodeJSONStrictBytes(w, body, &req) {
			return
		}
		url, email, token = req.URL, req.Email, req.Token
		var v httpx.Validation
		if url == "" {
			v.Missing("url")
		}
		if email == "" {
			v.Missing("email")
		}
		if token == "" {
			v.Missing("token")
		}
		if v.Flush(w, http.StatusBadRequest) {
			return
		}
		cfg = jira.CloudAPIToken(url, email, token)
	} else {
		var req jiraDataCenterCredential
		if !httpx.DecodeJSONStrictBytes(w, body, &req) {
			return
		}
		url, pat = req.URL, req.PAT
		var v httpx.Validation
		if url == "" {
			v.Missing("url")
		}
		if pat == "" {
			v.Invalid("pat", "a personal access token is required for Jira Data Center")
		}
		if v.Flush(w, http.StatusBadRequest) {
			return
		}
		cfg = jira.DataCenterPAT(url, pat)
	}

	jiraUser, err := auth.ValidateJira(r.Context(), cfg)
	if err != nil {
		// A mismatched scheme (Cloud creds against a DC host, or vice versa)
		// fails validation here rather than being stored — so we never persist a
		// credential that can't authenticate. When the chosen scheme disagrees
		// with the URL's host shape, the likeliest cause is the wrong deployment
		// pick, not a bad token, so append a hint. The host shape is ONLY a hint:
		// the validation above is the real authority (custom domains exist), so
		// this never blocks a combo that actually authenticates.
		msg := err.Error()
		switch hostDeployment := jira.DeploymentForHost(url); {
		case cloud && hostDeployment == jira.DeploymentDataCenter:
			msg += " — this looks like a Data Center URL, which uses a personal access token rather than an email + API token. Double-check your Jira deployment."
		case !cloud && hostDeployment == jira.DeploymentCloud:
			msg += " — this looks like an Atlassian Cloud URL, which uses an email + API token rather than a personal access token. Double-check your Jira deployment."
		}
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
			Reason: httpx.ReasonUpstreamRejected, Message: msg,
		})
		return
	}

	// One WithTx for the whole read + write window: credentials go
	// through tx.Secrets (Postgres vault writes need claims set) and
	// org_settings goes through tx.Orgs (org_settings_update RLS gates on
	// admin). All-or-nothing so creds + settings can't land in a partial
	// state. The earlier "manual rollback via ClearJira" pattern collapses
	// to plain tx rollback semantics.
	//
	// This is org-level Jira ACCESS (PAT_1) only — it deliberately does NOT
	// write the caller's per-user Jira identity. That is captured solely by
	// the dedicated bind surface (POST .../jira/identity/pat), so org access
	// and user identity stay independent even when the same token connects
	// the org.
	if err := se.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		creds, err := integrations.Load(r.Context(), tx.Secrets, orgID)
		if err != nil {
			return fmt.Errorf("load credentials: %w", err)
		}
		orgSet, err := tx.Orgs.GetSettings(r.Context(), orgID)
		if err != nil {
			return fmt.Errorf("load org settings: %w", err)
		}
		creds.JiraURL = url
		if cloud {
			creds.JiraEmail = email
			creds.JiraAPIToken = token
			creds.JiraAuthMethod = string(jira.AuthMethodCloudAPIToken)
		} else {
			creds.JiraPAT = pat
			creds.JiraAuthMethod = string(jira.AuthMethodDCPAT)
		}
		orgSet.JiraBaseURL = url
		if err := integrations.Save(r.Context(), tx.Secrets, orgID, creds); err != nil {
			return fmt.Errorf("store credentials: %w", err)
		}
		// Save skips empty values (never deletes), so switching schemes (DC↔Cloud)
		// would otherwise leave the prior scheme's secret behind — a stale
		// credential a later read could mistake for the org's auth. Drop the
		// scheme not in use so the stored set matches the marker exactly.
		if err := integrations.ClearJiraOtherScheme(r.Context(), tx.Secrets, orgID, jira.AuthMethod(creds.JiraAuthMethod)); err != nil {
			return fmt.Errorf("clear stale jira credential: %w", err)
		}
		if _, err := tx.Orgs.UpdateSettings(r.Context(), orgID, orgSet); err != nil {
			return fmt.Errorf("save org settings: %w", err)
		}
		// Audit the org Jira credential bind/rotate in the same tx,
		// against the canonicalized host (see auditJiraHost).
		if err := tx.AccessChangeLog.Record(r.Context(), orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialSet,
			DetailJSON:  accessDetailCredential(domain.CredentialKindJiraOrg, auditJiraHost(url)),
		}); err != nil {
			return fmt.Errorf("audit credential set: %w", err)
		}
		return nil
	}); err != nil {
		// Log the underlying wrap-chain (SQL / vault / FK errors) for
		// operator debugging, but return a stable user-facing message
		// so we don't leak Postgres internals to API clients.
		internalError(w, "settings", fmt.Errorf("persist jira connection: %w", err))
		return
	}

	// Re-due Jira polling under the new credential. The unbind has always done
	// this; the bind used to get it second-hand from the fused setup route, so
	// without it an org that just connected Jira would poll nothing until its
	// next scheduled cycle.
	se.kickJira(r, orgID)

	writeJSON(w, http.StatusOK, map[string]string{
		"status":       "connected",
		"display_name": jiraUser.DisplayName,
	})
}

// decodeJiraDeployment takes the lenient first look at a Jira credential body:
// just the discriminator, so the caller can then decode STRICTLY into that
// deployment's own struct and have the other one's fields come back as
// UNKNOWN_FIELD. It is the only place on this surface that reads a body without
// strictness, and it reads exactly one field.
//
// An absent or unrecognized value is a 400 rather than a fallback to either
// backend: guessing is what this discriminator replaced.
func decodeJiraDeployment(w http.ResponseWriter, body []byte) (jira.Deployment, bool) {
	var probe jiraCredentialDeployment
	if err := json.Unmarshal(body, &probe); err != nil {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidBody, Message: "invalid request body",
		})
		return "", false
	}
	switch jira.Deployment(probe.Deployment) {
	case jira.DeploymentCloud:
		return jira.DeploymentCloud, true
	case jira.DeploymentDataCenter:
		return jira.DeploymentDataCenter, true
	}
	reason := httpx.ReasonInvalidField
	if probe.Deployment == "" {
		reason = httpx.ReasonMissingField
	}
	httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
		Reason: reason,
		Message: fmt.Sprintf("deployment must be %q (an Atlassian Cloud site) or %q (self-hosted Jira Server / Data Center)",
			jira.DeploymentCloud, jira.DeploymentDataCenter),
		Field: "deployment",
	})
	return "", false
}

// handleJiraStatuses returns available statuses for given Jira projects.
// Query params: ?project=PROJ1&project=PROJ2 (or uses configured projects if omitted).
func (se *settingsHandler) handleJiraStatuses(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFrom(r.Context())
	userID := ClaimsFrom(r.Context()).Subject
	projects := r.URL.Query()["project"]
	// Validate every requested key against the same grammar the write path
	// enforces. Unvalidated, a garbage key reached Jira and came back as a
	// 502 — an upstream failure standing in for a malformed request.
	for _, p := range projects {
		if !jiraProjectKeyRe.MatchString(normalizeJiraProjectKey(p)) {
			httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
				Reason:  httpx.ReasonInvalidParam,
				Message: "project " + strconv.Quote(p) + " is not a valid Jira project key",
				Field:   "project",
			})
			return
		}
	}
	// Read creds + (if needed) the team's tracked-projects fallback
	// through the app pool inside WithTx so the org_secrets read and
	// team_settings_select run under the user's claims.
	var creds auth.Credentials
	if err := se.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		// A failed credential read is a 500, not "Jira not configured" —
		// telling a configured org to re-enter its credentials is the
		// swallowed-error failure mode this sweep closes.
		var lerr error
		creds, lerr = integrations.Load(r.Context(), tx.Secrets, orgID)
		if lerr != nil {
			return fmt.Errorf("load integration credentials: %w", lerr)
		}
		if len(projects) > 0 {
			return nil
		}
		teamID, e := tx.Teams.GetDefaultForOrg(r.Context(), orgID)
		if e != nil || teamID == "" {
			return nil
		}
		teamSet, te := tx.Teams.GetSettings(r.Context(), teamID)
		if te != nil {
			return nil
		}
		projects = append(projects, teamSet.JiraProjects...)
		return nil
	}); err != nil {
		internalError(w, "settings", err)
		return
	}
	// Build the system client from the loaded creds, routed Cloud-vs-DC by the
	// stored auth-method marker (the request-path analog of the resolver's
	// ForSystem) — a Cloud org has no PAT, so gating/constructing on JiraPAT
	// alone would 400 a configured Cloud org here.
	cfg, ok := integrations.JiraSystemConfig(creds)
	if !ok {
		writeNotConfigured(w, "Jira is not connected for this workspace")
		return
	}
	if len(projects) == 0 {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonInvalidParam,
			Message: "no projects specified, and this workspace's team tracks none",
			Field:   "project",
		})
		return
	}

	client := jira.NewClient(cfg)

	// Intersect statuses across all projects — only return statuses that
	// exist in every project. A union would let users pick a status that
	// fails on some projects (TransitionTo can't find the transition).
	var counts map[string]int            // status name → number of projects it appears in
	var canonical map[string]jira.Status // status name → first-seen Status object
	for i, proj := range projects {
		projectStatuses, err := client.ProjectStatuses(r.Context(), proj)
		if err != nil {
			httpx.WriteErrors(w, http.StatusBadGateway, httpx.ErrorItem{
				Reason:  httpx.ReasonUpstreamUnavailable,
				Message: "failed to fetch statuses for " + proj + " from Jira" + httpx.LocalDetail(err),
			})
			return
		}
		if i == 0 {
			counts = make(map[string]int, len(projectStatuses))
			canonical = make(map[string]jira.Status, len(projectStatuses))
		}
		seen := map[string]bool{}
		for _, st := range projectStatuses {
			if !seen[st.Name] {
				seen[st.Name] = true
				counts[st.Name]++
				if _, ok := canonical[st.Name]; !ok {
					canonical[st.Name] = st
				}
			}
		}
	}

	var statuses []jira.Status
	for name, count := range counts {
		if count == len(projects) {
			statuses = append(statuses, canonical[name])
		}
	}
	sort.Slice(statuses, func(i, j int) bool {
		return statuses[i].Name < statuses[j].Name
	})

	writeJSON(w, http.StatusOK, statuses)
}

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
	// Probe target tracks the configured GitHub base URL so the Test
	// SSH button on the Settings page works for GHE deployments. We
	// load creds (not settings) because creds.GitHubURL is the URL the
	// user actually authenticates against; org_settings.github_base_url
	// mirrors it but the SecretStore copy is the source of truth.
	// Wrapped in WithTx so the org_secrets read sees claims and matches
	// the rest of the settings surface.
	orgID := OrgIDFrom(r.Context())
	userID := ClaimsFrom(r.Context()).Subject
	var creds auth.Credentials
	if err := se.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var lerr error
		creds, lerr = integrations.Load(r.Context(), tx.Secrets, orgID)
		return lerr
	}); err != nil {
		internalError(w, "settings", fmt.Errorf("load integration credentials: %w", err))
		return
	}
	sshHost := worktree.SSHHostFromBaseURL(creds.GitHubURL)

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if err := worktree.PreflightSSH(ctx, sshHost); err != nil {
		settingsLog.Warn("ssh preflight failed", "ssh_host", sshHost, "error", err)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     false,
			"stderr": err.Error(),
			"host":   sshHost,
		})
		return
	}
	settingsLog.Info("ssh preflight ok", "ssh_host", sshHost)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "host": sshHost})
}
