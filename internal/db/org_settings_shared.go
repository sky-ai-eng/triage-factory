package db

import (
	"database/sql"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// SourceOverride is one source's org_event_sources.base_url / poll_interval,
// dialect-neutral once scanned — the two columns OrgsStore owns on that
// table; disabled / disabled_at / disabled_by on the same row stay owned by
// OrgEventSourceStore.SetDisabled. The zero value means "no override
// recorded" for both columns, matching the columns' own NULL convention. Each
// dialect store scans its own base_url / poll_interval read into this shape
// (the SQL and NULL handling stay dialect-local — a nullable Postgres
// interval and a nullable SQLite duration string decode differently) and
// hands it to ApplyOrgSourceOverrides below.
type SourceOverride struct {
	BaseURL     string
	Interval    time.Duration
	HasInterval bool
}

// ApplyOrgSourceOverrides fills set's four org_event_sources-backed fields. A
// missing poll-interval override resolves to DefaultOrgSettings()'s 5-minute
// cadence — the same fallback the org_settings NOT NULL DEFAULT columns used
// to give for free. A missing base-URL override resolves to "", matching that
// column's existing NULL round-trip.
func ApplyOrgSourceOverrides(set *domain.OrgSettings, github, jira SourceOverride) {
	defaults := domain.DefaultOrgSettings()
	set.GitHubBaseURL = github.BaseURL
	set.GitHubPollInterval = defaults.GitHubPollInterval
	if github.HasInterval {
		set.GitHubPollInterval = github.Interval
	}
	set.JiraBaseURL = jira.BaseURL
	set.JiraPollInterval = defaults.JiraPollInterval
	if jira.HasInterval {
		set.JiraPollInterval = jira.Interval
	}
}

// ScanOrgSettingsCore decodes the org_settings columns each dialect store's
// orgSettingsColumns projects, in that order: github_clone_protocol,
// anthropic_api_key_ref, bedrock_credentials_ref, enabled_models,
// background_jobs_model, llm_auth_method, max_daily_cost_usd,
// max_concurrent_runs, marketplace_enabled, api_token_max_age_days,
// github_credential_class, version.
// GitHubBaseURL / GitHubPollInterval / JiraBaseURL / JiraPollInterval are left
// at the Go zero value — callers apply ApplyOrgSourceOverrides afterward.
//
// The scan itself has no dialect content left (both backends read the same
// plain column types for what remains after github_base_url /
// github_poll_interval / jira_base_url / jira_poll_interval moved onto
// org_event_sources), so it lives here rather than duplicated per dialect.
func ScanOrgSettingsCore(scan func(...any) error) (domain.OrgSettings, error) {
	var (
		anthRef, bedRef, enabledModels sql.NullString
		cloneProto                     string
		backgroundJobsModel            string
		llmAuthMethod                  string
		maxDailyCost                   sql.NullFloat64
		maxConcurrentRuns              sql.NullInt64
		marketplaceEnabled             bool
		apiTokenMaxAgeDays             sql.NullInt64
		credentialClass                string
		version                        int
	)
	if err := scan(
		&cloneProto,
		&anthRef, &bedRef, &enabledModels, &backgroundJobsModel, &llmAuthMethod,
		&maxDailyCost, &maxConcurrentRuns, &marketplaceEnabled, &apiTokenMaxAgeDays,
		&credentialClass, &version,
	); err != nil {
		return domain.OrgSettings{}, err
	}
	// NULL stays nil rather than becoming an empty slice: the absent set and a
	// set naming nothing resolve differently, and this is the one place the
	// column's NULL reaches Go.
	enabled, err := UnmarshalModelSetColumn(enabledModels, "org_settings.enabled_models")
	if err != nil {
		return domain.OrgSettings{}, err
	}
	// Clamp a stray negative up to 0. No DB CHECK guards this column, and
	// everything downstream (the claim) reads <= 0 as unlimited — so surface a
	// negative as 0 here too, keeping "unlimited" consistently 0 and never
	// handing the settings UI a value it rejects.
	concurrentRuns := int(maxConcurrentRuns.Int64) // NULL → 0 (unlimited)
	if concurrentRuns < 0 {
		concurrentRuns = 0
	}
	// Same shape, and the same reason on the dialect that needs it: the
	// Postgres column carries a 1..365 CHECK, but the SQLite twin carries none
	// (adding one by ALTER TABLE would mean rebuilding org_settings to
	// constrain a column local mode never reads), so a value below the band
	// can exist there. Read as 0 — "uncapped" keeps one spelling in Go, and
	// the settings form is never handed a value its own validator rejects.
	tokenMaxAge := int(apiTokenMaxAgeDays.Int64) // NULL → 0 (uncapped)
	if tokenMaxAge < domain.APITokenMaxAgeDaysMin {
		tokenMaxAge = 0
	}
	return domain.OrgSettings{
		GitHubCloneProtocol:   cloneProto,
		AnthropicAPIKeyRef:    anthRef.String,
		BedrockCredentialsRef: bedRef.String,
		EnabledModels:         enabled,
		// NOT NULL on both backends, so no null coalescing: "" is the org's own
		// "not picked", which is the value the multi-mode column defaults to.
		BackgroundJobsModel: backgroundJobsModel,
		// Surfaced verbatim; the dialect defaults differ and the mode-level
		// answer is domain.EffectiveLLMAuthMethod's, not the scan's.
		LLMAuthMethod:      llmAuthMethod,
		MaxDailyCostUSD:    maxDailyCost.Float64, // NULL → 0 (no cap)
		MaxConcurrentRuns:  concurrentRuns,
		MarketplaceEnabled: marketplaceEnabled,
		APITokenMaxAgeDays: tokenMaxAge,
		// Surfaced verbatim, never coerced to a known value: callers switch on
		// it and refuse what they don't recognise, which is the whole point of
		// storing the class instead of inferring it.
		GitHubCredentialClass: domain.GitHubCredentialClass(credentialClass),
		Version:               version,
	}, nil
}
