package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// jiraProjectKeyRe matches Jira's standard project-key rule: a
// leading uppercase letter followed by uppercase letters or digits.
// Keys arriving through the API are uppercased before matching so
// users typing "sky" land on the same canonical form as Jira's
// wire-side "SKY-123".
var jiraProjectKeyRe = regexp.MustCompile(`^[A-Z][A-Z0-9]*$`)

// normalizeJiraProjectKey trims whitespace and uppercases. Used at
// the HTTP boundary in handleSettingsPost (the write path) and in
// validateTrackerKeys (the read/compare path) so lookups match
// regardless of how the user typed the key.
func normalizeJiraProjectKey(s string) string {
	return strings.ToUpper(strings.TrimSpace(s))
}

// jiraStatusRule is the wire shape for a single status rule (pickup,
// in-progress, or done). Local to this package now that internal/
// config is gone; mirrors the prior config.JiraStatusRule view.
type jiraStatusRule struct {
	Members   []string `json:"members"`
	Canonical string   `json:"canonical,omitempty"`
}

// jiraProjectConfig is the per-project wire shape for the settings
// handler. Three status rules — pickup, in-progress, done — keyed by
// project_key. Mirrors what the deleted config.JiraProjectConfig
// exposed.
type jiraProjectConfig struct {
	Key        string         `json:"key"`
	Pickup     jiraStatusRule `json:"pickup"`
	InProgress jiraStatusRule `json:"in_progress"`
	Done       jiraStatusRule `json:"done"`
}

// validateProjectRules enforces the per-project invariant that every
// persisted project carries fully-populated Pickup/InProgress/Done
// rules. The jpsr_*_populated CHECK constraints in the baseline are
// the DB-level mirror; this is the user-facing gate that surfaces a
// readable error instead of a constraint violation.
//
// Pickup: members required, canonical must be empty (TF never writes
// to pickup). InProgress/Done: members + canonical required, and the
// canonical must itself be a member (PG CHECK can't subquery, so this
// check stays in Go).
func validateProjectRules(p jiraProjectConfig) error {
	if len(p.Pickup.Members) == 0 {
		return fmt.Errorf("project %s: pickup members are required", p.Key)
	}
	if p.Pickup.Canonical != "" {
		return fmt.Errorf("project %s: pickup canonical must be empty — TF never writes tickets back to pickup", p.Key)
	}
	for _, r := range []struct {
		name string
		rule jiraStatusRule
	}{
		{"in_progress", p.InProgress},
		{"done", p.Done},
	} {
		if len(r.rule.Members) == 0 {
			return fmt.Errorf("project %s: %s members are required", p.Key, r.name)
		}
		if r.rule.Canonical == "" {
			return fmt.Errorf("project %s: %s canonical is required", p.Key, r.name)
		}
		if !slices.Contains(r.rule.Members, r.rule.Canonical) {
			return fmt.Errorf("project %s: %s canonical %q is not in members", p.Key, r.name, r.rule.Canonical)
		}
	}
	return nil
}

// normalizeMembers returns a sorted, deduplicated copy of members so rules can
// be compared using set semantics without mutating the original slice.
func normalizeMembers(members []string) []string {
	normalized := slices.Clone(members)
	slices.Sort(normalized)
	return slices.Compact(normalized)
}

// ruleEqual compares two status rules by value. Used by change detection to
// decide whether a Jira poller restart is needed. Nil-safe on the Members slice.
func ruleEqual(a, b jiraStatusRule) bool {
	return a.Canonical == b.Canonical &&
		slices.Equal(normalizeMembers(a.Members), normalizeMembers(b.Members))
}

// cloneJiraProjects returns a deep copy so the pre-change snapshot
// stays stable while the handler mutates the desired project list.
func cloneJiraProjects(in []jiraProjectConfig) []jiraProjectConfig {
	out := make([]jiraProjectConfig, len(in))
	for i, p := range in {
		out[i] = jiraProjectConfig{
			Key: p.Key,
			Pickup: jiraStatusRule{
				Members:   slices.Clone(p.Pickup.Members),
				Canonical: p.Pickup.Canonical,
			},
			InProgress: jiraStatusRule{
				Members:   slices.Clone(p.InProgress.Members),
				Canonical: p.InProgress.Canonical,
			},
			Done: jiraStatusRule{
				Members:   slices.Clone(p.Done.Members),
				Canonical: p.Done.Canonical,
			},
		}
	}
	return out
}

// jiraProjectsEqual compares two per-project lists by value, treating
// order as significant (the user-facing UI keeps projects in the order
// they were added; reordering counts as a change worth restarting the
// poller for). Rules are compared with set-equality on Members.
func jiraProjectsEqual(a, b []jiraProjectConfig) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Key != b[i].Key {
			return false
		}
		if !ruleEqual(a[i].Pickup, b[i].Pickup) ||
			!ruleEqual(a[i].InProgress, b[i].InProgress) ||
			!ruleEqual(a[i].Done, b[i].Done) {
			return false
		}
	}
	return true
}

// projectConfigsToRules is the inverse of rulesToProjectConfigsOrdered. Used
// by handleSettingsPost when persisting the team's project list back
// to jira_project_status_rules via JiraStatusRulesStore.ReplaceForTeam.
func projectConfigsToRules(projects []jiraProjectConfig) []domain.JiraProjectStatusRules {
	out := make([]domain.JiraProjectStatusRules, 0, len(projects))
	for _, p := range projects {
		out = append(out, domain.JiraProjectStatusRules{
			ProjectKey:          p.Key,
			PickupMembers:       slices.Clone(p.Pickup.Members),
			InProgressMembers:   slices.Clone(p.InProgress.Members),
			InProgressCanonical: p.InProgress.Canonical,
			DoneMembers:         slices.Clone(p.Done.Members),
			DoneCanonical:       p.Done.Canonical,
		})
	}
	return out
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

// jiraProjectSettings is the per-project wire shape. Mirrors
// jiraProjectConfig but with explicit empty-slice initialization so the
// JSON response always carries members:[] rather than members:null.
type jiraProjectSettings struct {
	Key        string         `json:"key"`
	Pickup     jiraStatusRule `json:"pickup"`
	InProgress jiraStatusRule `json:"in_progress"`
	Done       jiraStatusRule `json:"done"`
}

// rulesToProjectConfigsOrdered converts the domain rules into the
// wire shape and reorders them to match order. Keys present in
// order but missing from rules are skipped (the store is the source
// of truth for rule existence); keys in rules but not in order get
// appended after the ordered set in their store-returned (ASC)
// position so a manual DB poke that adds a row without updating
// team_settings.jira_projects still surfaces.
func rulesToProjectConfigsOrdered(rules []domain.JiraProjectStatusRules, order []string) []jiraProjectConfig {
	if len(rules) == 0 {
		return nil
	}
	byKey := make(map[string]domain.JiraProjectStatusRules, len(rules))
	for _, r := range rules {
		byKey[r.ProjectKey] = r
	}
	out := make([]jiraProjectConfig, 0, len(rules))
	seen := make(map[string]bool, len(rules))
	for _, k := range order {
		r, ok := byKey[k]
		if !ok {
			continue
		}
		out = append(out, ruleToProjectConfig(r))
		seen[k] = true
	}
	for _, r := range rules {
		if seen[r.ProjectKey] {
			continue
		}
		out = append(out, ruleToProjectConfig(r))
	}
	return out
}

func ruleToProjectConfig(r domain.JiraProjectStatusRules) jiraProjectConfig {
	return jiraProjectConfig{
		Key: r.ProjectKey,
		Pickup: jiraStatusRule{
			Members: slices.Clone(r.PickupMembers),
		},
		InProgress: jiraStatusRule{
			Members:   slices.Clone(r.InProgressMembers),
			Canonical: r.InProgressCanonical,
		},
		Done: jiraStatusRule{
			Members:   slices.Clone(r.DoneMembers),
			Canonical: r.DoneCanonical,
		},
	}
}

// toJiraProjectSettings converts the persisted view into the wire
// shape, normalizing nil Members slices to empty slices so the JSON
// response is friendly to FE consumers (no `members:null`).
func toJiraProjectSettings(in []jiraProjectConfig) []jiraProjectSettings {
	out := make([]jiraProjectSettings, 0, len(in))
	for _, p := range in {
		out = append(out, jiraProjectSettings{
			Key:        p.Key,
			Pickup:     normalizeRule(p.Pickup),
			InProgress: normalizeRule(p.InProgress),
			Done:       normalizeRule(p.Done),
		})
	}
	return out
}

// normalizeRule replaces a nil Members slice with an empty one so the
// JSON encoding is `[]` rather than `null`. Canonical and other fields
// pass through unchanged.
func normalizeRule(r jiraStatusRule) jiraStatusRule {
	if r.Members == nil {
		r.Members = []string{}
	}
	return r
}

// projectKeysFromConfigs returns the ordered project keys from a
// per-project config slice with empty entries filtered out.
func projectKeysFromConfigs(projects []jiraProjectConfig) []string {
	keys := make([]string, 0, len(projects))
	for _, p := range projects {
		if p.Key != "" {
			keys = append(keys, p.Key)
		}
	}
	return keys
}

// handleJiraConnect validates and stores Jira credentials without saving
// the rest of the settings. This powers the two-stage settings flow: connect
// first, then configure projects and statuses.
//
// Requires an active org because credentials write through the SecretStore
// — see handleSettingsPost for the multi-mode rationale.
func (s *Server) handleJiraConnect(w http.ResponseWriter, r *http.Request) {
	userID := ClaimsFrom(r.Context()).Subject
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	var req struct {
		URL string `json:"url"`
		PAT string `json:"pat"`
	}
	if !decodeJSON(w, r, &req, "") {
		return
	}
	if req.URL == "" || req.PAT == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url and pat are required"})
		return
	}

	jiraUser, err := auth.ValidateJira(r.Context(), jira.DataCenterPAT(req.URL, req.PAT))
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}

	// One WithTx for the whole read + write window: credentials go
	// through tx.Secrets (Postgres vault writes need claims set),
	// org_settings goes through tx.Orgs (org_settings_update RLS
	// gates on admin), and SKY-270's Jira identity write goes through
	// tx.Users. All-or-nothing so creds + settings + identity can't
	// land in a partial state. The earlier "manual rollback via
	// ClearJira" pattern collapses to plain tx rollback semantics.
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		creds, err := integrations.Load(r.Context(), tx.Secrets, orgID)
		if err != nil {
			return fmt.Errorf("load credentials: %w", err)
		}
		orgSet, err := tx.Orgs.GetSettings(r.Context(), orgID)
		if err != nil {
			return fmt.Errorf("load org settings: %w", err)
		}
		creds.JiraURL = req.URL
		creds.JiraPAT = req.PAT
		orgSet.JiraBaseURL = req.URL
		if err := integrations.Save(r.Context(), tx.Secrets, orgID, creds); err != nil {
			return fmt.Errorf("store credentials: %w", err)
		}
		if err := tx.Orgs.UpdateSettings(r.Context(), orgID, orgSet); err != nil {
			return fmt.Errorf("save org settings: %w", err)
		}
		// SKY-270: persist the captured Jira identity on the users
		// row. Bundled in the same tx so the connect either fully
		// lands (creds + URL + identity) or fully rolls back.
		if err := tx.Users.SetJiraIdentity(r.Context(), userID, jiraUser.StableID(), jiraUser.DisplayName); err != nil {
			return fmt.Errorf("persist users.jira_identity: %w", err)
		}
		return nil
	}); err != nil {
		// Log the underlying wrap-chain (SQL / vault / FK errors) for
		// operator debugging, but return a stable user-facing message
		// so we don't leak Postgres internals to API clients.
		log.Printf("[settings] handleJiraConnect persist failed: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to connect Jira"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status":       "connected",
		"display_name": jiraUser.DisplayName,
	})
}

// handleJiraStatuses returns available statuses for given Jira projects.
// Query params: ?project=PROJ1&project=PROJ2 (or uses configured projects if omitted).
func (s *Server) handleJiraStatuses(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFrom(r.Context())
	userID := ClaimsFrom(r.Context()).Subject
	projects := r.URL.Query()["project"]
	// Read creds + (if needed) the team's tracked-projects fallback
	// through the app pool inside WithTx so vault_decrypt and
	// team_settings_select run under the user's claims.
	var creds auth.Credentials
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		creds, _ = integrations.Load(r.Context(), tx.Secrets, orgID)
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
	if creds.JiraPAT == "" || creds.JiraURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Jira not configured"})
		return
	}
	if len(projects) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no projects specified"})
		return
	}

	client := jira.NewClient(jira.DataCenterPAT(creds.JiraURL, creds.JiraPAT))

	// Intersect statuses across all projects — only return statuses that
	// exist in every project. A union would let users pick a status that
	// fails on some projects (TransitionTo can't find the transition).
	var counts map[string]int            // status name → number of projects it appears in
	var canonical map[string]jira.Status // status name → first-seen Status object
	for i, proj := range projects {
		projectStatuses, err := client.ProjectStatuses(proj)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to fetch statuses for " + proj + ": " + err.Error()})
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
func (s *Server) handleGitHubPreflightSSH(w http.ResponseWriter, r *http.Request) {
	// SSH is local-mode-only. PreflightSSH writes the container's
	// ~/.ssh/known_hosts (accept-new) and probes the operator's ssh-agent —
	// neither exists in a hosted multi-mode container, and the clone path
	// there is hardwired to HTTPS. Refuse rather than run the probe so no SSH
	// machinery is ever touched in multi mode. The UI hides the button there;
	// this is the defense-in-depth backstop.
	if runmode.Current() != runmode.ModeLocal {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"ok":    false,
			"error": "ssh clone protocol is not available in this deployment",
		})
		return
	}
	// Probe target tracks the configured GitHub base URL so the Test
	// SSH button on the Settings page works for GHE deployments. We
	// load creds (not settings) because creds.GitHubURL is the URL the
	// user actually authenticates against; org_settings.github_base_url
	// mirrors it but the SecretStore copy is the source of truth.
	// Wrapped in WithTx so vault_decrypt sees claims and the read
	// matches the rest of the post-SKY-355 settings surface.
	orgID := OrgIDFrom(r.Context())
	userID := ClaimsFrom(r.Context()).Subject
	var creds auth.Credentials
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		creds, _ = integrations.Load(r.Context(), tx.Secrets, orgID)
		return nil
	}); err != nil {
		internalError(w, "settings", err)
		return
	}
	sshHost := worktree.SSHHostFromBaseURL(creds.GitHubURL)

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if err := worktree.PreflightSSH(ctx, sshHost); err != nil {
		log.Printf("[settings] SSH preflight against %s failed: %v", sshHost, err)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     false,
			"stderr": err.Error(),
			"host":   sshHost,
		})
		return
	}
	log.Printf("[settings] SSH preflight ok (%s authenticated)", sshHost)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "host": sshHost})
}
