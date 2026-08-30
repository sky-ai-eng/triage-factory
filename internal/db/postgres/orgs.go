package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// orgsStore is the Postgres impl of db.OrgsStore. Holds both pools —
// see the OrgsStore interface comment for the pool-split rationale.
//
//   - admin: ListActiveSystem, GetSettingsSystem. Background services
//     iterating the active org set or reading per-org settings without
//     a JWT-claims context.
//   - app: GetSettings, UpdateSettings. Request-handler reads/writes
//     gated by the org_settings_select / org_settings_update RLS
//     policies (org membership / org admin).
type orgsStore struct {
	app   queryer
	admin queryer
}

func newOrgsStore(app, admin queryer) db.OrgsStore {
	return &orgsStore{app: app, admin: admin}
}

var _ db.OrgsStore = (*orgsStore)(nil)

func (s *orgsStore) GetOrg(ctx context.Context, orgID string) (*domain.Org, error) {
	return getOrg(ctx, s.app, orgID)
}

func (s *orgsStore) GetOrgSystem(ctx context.Context, orgID string) (*domain.Org, error) {
	return getOrg(ctx, s.admin, orgID)
}

// CreateLocalTenant is local-mode only — multi-mode provisions real
// tenant rows per signup in auth_provision.go, never the synthetic
// LocalDefault* sentinels. In Postgres (multi mode) this should never be called;
// return a clear error so incorrect callers fail loudly.
func (s *orgsStore) CreateLocalTenant(ctx context.Context) error {
	return fmt.Errorf("db: CreateLocalTenant is not supported in multi mode")
}

func getOrg(ctx context.Context, q queryer, orgID string) (*domain.Org, error) {
	var (
		o     domain.Org
		owner sql.NullString
	)
	err := q.QueryRowContext(ctx, `
		SELECT id::text, name, slug, owner_user_id::text, created_at
		  FROM orgs WHERE id = $1
	`, orgID).Scan(&o.ID, &o.Name, &o.Slug, &owner, &o.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read org: %w", err)
	}
	o.OwnerUserID = owner.String
	return &o, nil
}

func (s *orgsStore) ListActiveSystem(ctx context.Context) ([]string, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT id FROM orgs
		WHERE deleted_at IS NULL
		ORDER BY id ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *orgsStore) GetSettings(ctx context.Context, orgID string) (domain.OrgSettings, error) {
	return getOrgSettings(ctx, s.app, orgID)
}

func (s *orgsStore) GetSettingsSystem(ctx context.Context, orgID string) (domain.OrgSettings, error) {
	return getOrgSettings(ctx, s.admin, orgID)
}

// orgSettingsColumns is the canonical projection of an org_settings row, in
// the order db.ScanOrgSettingsCore reads them. GetSettings SELECTs it and every
// writer below RETURNs it, so the write shape cannot drift from the read
// shape.
//
// EXTRACT(EPOCH FROM interval) returns numeric in PG13+; the ::double
// precision cast pins the row-out type so pgx can scan straight into float64
// without a string detour. Cleaner round-trip than ::text +
// time.ParseDuration (which can't parse the Postgres "HH:MM:SS" interval
// rendering anyway). RETURNING evaluates these expressions over the written
// row just as SELECT does, so the same column list works in both places.
//
// github_base_url / github_poll_interval / jira_base_url / jira_poll_interval
// are NOT here — they moved onto org_event_sources.base_url /
// poll_interval, keyed by kind. getOrgSettings composes them in from
// readSourceOverrides below; every writer merges u.GitHubBaseURL etc. (or,
// for SetGitHubCredentialClass which doesn't touch them, a fresh read) into
// what it returns, so the struct this file hands back is unchanged even
// though the row it comes from is now two tables.
const orgSettingsColumns = `github_clone_protocol,
	       anthropic_api_key_ref, bedrock_credentials_ref, enabled_models,
	       background_jobs_model, llm_auth_method,
	       max_daily_cost_usd, max_concurrent_runs, marketplace_enabled,
	       api_token_max_age_days, github_credential_class, version`

func getOrgSettings(ctx context.Context, q queryer, orgID string) (domain.OrgSettings, error) {
	set, err := db.ScanOrgSettingsCore(q.QueryRowContext(ctx, `
		SELECT `+orgSettingsColumns+`
		FROM org_settings WHERE org_id = $1
	`, orgID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		// Provisioning seeds org_settings rows at org-create time
		// (auth provisioning); this fallback covers the narrow window
		// before the first signup runs (or test fixtures that build a
		// DB without going through provisioning). Matches the schema
		// DEFAULT clauses.
		set = domain.DefaultOrgSettings()
	} else if err != nil {
		return domain.OrgSettings{}, fmt.Errorf("read org_settings: %w", err)
	}
	github, jira, err := readSourceOverrides(ctx, q, orgID)
	if err != nil {
		return domain.OrgSettings{}, fmt.Errorf("read org_event_sources overrides: %w", err)
	}
	db.ApplyOrgSourceOverrides(&set, github, jira)
	return set, nil
}

// readSourceOverrides reads the github + jira org_event_sources rows'
// base_url / poll_interval in one query. A NULL column, or an altogether
// absent row, reports the zero db.SourceOverride for that column — applied by
// db.ApplyOrgSourceOverrides. EXTRACT(EPOCH FROM NULL) is NULL, so the
// nullable interval scans straight into sql.NullFloat64 the same way
// orgSettingsColumns' poll-interval expressions used to.
func readSourceOverrides(ctx context.Context, q queryer, orgID string) (github, jira db.SourceOverride, err error) {
	rows, err := q.QueryContext(ctx, `
		SELECT kind, base_url, EXTRACT(EPOCH FROM poll_interval)::double precision
		FROM org_event_sources
		WHERE org_id = $1 AND kind IN ('github', 'jira')`, orgID)
	if err != nil {
		return db.SourceOverride{}, db.SourceOverride{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			kind    string
			baseURL sql.NullString
			secs    sql.NullFloat64
		)
		if err := rows.Scan(&kind, &baseURL, &secs); err != nil {
			return db.SourceOverride{}, db.SourceOverride{}, err
		}
		ov := db.SourceOverride{BaseURL: baseURL.String}
		if secs.Valid {
			ov.Interval, ov.HasInterval = secondsToDuration(secs.Float64), true
		}
		switch kind {
		case "github":
			github = ov
		case "jira":
			jira = ov
		}
	}
	return github, jira, rows.Err()
}

// upsertSourceOverride writes org_event_sources.base_url + poll_interval for
// one (org, kind) — the two columns OrgsStore owns on this table. A partial
// upsert: disabled / disabled_at / disabled_by are absent from both the
// INSERT column list and the SET list, so this can never touch the pause
// SetDisabled owns, and a fresh row relies on disabled's schema
// DEFAULT the same way SetGitHubCredentialClass's partial insert already
// relies on org_settings' other defaults. make_interval(secs => $N) mirrors
// upsertSettings' own use below — a numeric second count, no hand-rolled
// interval string.
func upsertSourceOverride(ctx context.Context, q queryer, orgID, kind, baseURL string, pollInterval time.Duration) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO org_event_sources (org_id, kind, base_url, poll_interval)
		VALUES ($1, $2, $3, make_interval(secs => $4))
		ON CONFLICT (org_id, kind) DO UPDATE SET
			base_url      = EXCLUDED.base_url,
			poll_interval = EXCLUDED.poll_interval`,
		orgID, kind, nullString(baseURL), pollInterval.Seconds())
	return err
}

// UpdateSettings upserts every org_settings column this writer owns.
//
// github_credential_class is deliberately absent from BOTH the INSERT column
// list and the ON CONFLICT SET list, and must stay that way. Absent from both,
// the column takes its DEFAULT on insert and is left untouched on update —
// exactly the behaviour required, because the class is owned by the credential
// transitions (SetGitHubCredentialClass), not by the settings writer. Adding it
// here would look like tidiness and would instead reset the class to the
// struct's zero value on every bulk settings save, silently converting a
// BYO-App org to PAT. u.GitHubCredentialClass is read-only; it is ignored here.
func (s *orgsStore) UpdateSettings(ctx context.Context, orgID string, u domain.OrgSettings) (domain.OrgSettings, error) {
	// No version guard: an unguarded save is last-writer-wins on purpose. Its
	// callers are the credential transitions, which own the specific fields
	// they touch and have nothing to lose a race about. It still bumps the
	// token, so an admin's in-flight settings edit conflicts rather than
	// landing on top of a credential change it never saw.
	stored, err := s.upsertSettings(ctx, orgID, u, orgSettingsConflictUpdate)
	if err != nil {
		return domain.OrgSettings{}, fmt.Errorf("upsert org_settings: %w", err)
	}
	return stored, nil
}

// UpdateSettingsVersioned is UpdateSettings under the row's concurrency token.
//
// The two assertions it can be handed are two different statements, because
// they are two different questions:
//
//   - expected 0 says "there is no row yet", which is a create. It is an
//     INSERT that does nothing on conflict, so a racing creator makes this
//     caller the loser rather than the second writer of a row it believed it
//     was the first to touch.
//   - any other expected says "the row is at this version", which is an
//     update. It is a plain guarded UPDATE, so an absent row and a moved
//     version give the same answer — nothing matched — which is exactly right:
//     both mean the caller's read no longer describes the world.
//
// Folding the two into one guarded upsert is the shape this started as, and it
// is wrong in a way that is easy to miss: the guard can only ride the conflict
// arm, so a caller asserting a stale non-zero version against a row that had
// since been deleted would fall through to the INSERT arm and silently CREATE
// the row at version 1 — a create reported as a successful update.
func (s *orgsStore) UpdateSettingsVersioned(ctx context.Context, orgID string, u domain.OrgSettings, expected int) (domain.OrgSettings, error) {
	var (
		stored domain.OrgSettings
		err    error
	)
	if expected == 0 {
		stored, err = s.upsertSettings(ctx, orgID, u, `ON CONFLICT (org_id) DO NOTHING`)
	} else {
		stored, err = s.updateSettingsAtVersion(ctx, orgID, u, expected)
	}
	if errors.Is(err, sql.ErrNoRows) {
		// The conflict/no-match arm: RETURNING produced no row, which for a
		// DO NOTHING insert or a version-guarded UPDATE means nothing landed.
		return domain.OrgSettings{}, db.ErrOrgSettingsVersion
	}
	if err != nil {
		return domain.OrgSettings{}, fmt.Errorf("write org_settings: %w", err)
	}
	return stored, nil
}

// orgSettingsConflictUpdate is the unguarded writer's conflict action: replace
// every column this writer owns and bump the token. It is a constant rather
// than inline so the INSERT half of the statement has exactly one spelling —
// see upsertSettings.
const orgSettingsConflictUpdate = `
		ON CONFLICT (org_id) DO UPDATE SET
			github_clone_protocol = EXCLUDED.github_clone_protocol,
			anthropic_api_key_ref = EXCLUDED.anthropic_api_key_ref,
			bedrock_credentials_ref = EXCLUDED.bedrock_credentials_ref,
			enabled_models = EXCLUDED.enabled_models,
			background_jobs_model = EXCLUDED.background_jobs_model,
			llm_auth_method = EXCLUDED.llm_auth_method,
			max_daily_cost_usd = EXCLUDED.max_daily_cost_usd,
			max_concurrent_runs = EXCLUDED.max_concurrent_runs,
			marketplace_enabled = EXCLUDED.marketplace_enabled,
			api_token_max_age_days = EXCLUDED.api_token_max_age_days,
			version = org_settings.version + 1,
			updated_at = now()`

// orgSettingsWriteArgs is the ordered argument list every statement in this
// writer takes — $1 the org, $2.. the columns it owns — so the INSERT and
// the guarded UPDATE below can never disagree about which value is which.
// GitHubBaseURL / GitHubPollInterval / JiraBaseURL / JiraPollInterval are NOT
// here — they are org_event_sources columns now; finishSettingsWrite below
// writes them separately, in the same transaction.
func orgSettingsWriteArgs(orgID string, u domain.OrgSettings) []any {
	cloneProto := u.GitHubCloneProtocol
	if cloneProto == "" {
		cloneProto = "https"
	}
	return []any{
		orgID,
		cloneProto,
		nullString(u.AnthropicAPIKeyRef),
		nullString(u.BedrockCredentialsRef),
		db.ModelSetColumnValue(u.EnabledModels),
		// Plain string, not nullString: the column is NOT NULL and "" is the
		// org's own "not picked yet" rather than an absent value.
		u.BackgroundJobsModel,
		// "" is restated as the column's own DEFAULT rather than written
		// through: this dialect is multi mode, whose only credential source is
		// the org's own, and a caller that built the struct without knowing
		// about this field must not blank it into a value the read would have
		// to guess at.
		authMethodOrDefault(u.LLMAuthMethod),
		nullFloat(u.MaxDailyCostUSD),
		nullInt(u.MaxConcurrentRuns),
		u.MarketplaceEnabled,
		// 0 is "uncapped" and writes NULL, the same 0 ↔ NULL round-trip the two
		// caps above take. The column's CHECK admits only 1..365, so a value
		// the handler let through below the band would be an error at the DB
		// rather than a silently stored non-policy — nullInt keeps 0 out of it.
		nullInt(u.APITokenMaxAgeDays),
	}
}

// authMethodOrDefault substitutes the column's DEFAULT for an unset field, the
// way orgSettingsWriteArgs substitutes "ssh" for an unset clone protocol.
func authMethodOrDefault(method string) string {
	if method == "" {
		return domain.LLMAuthBYOK
	}
	return method
}

// upsertSettings writes every org_settings column this writer owns and
// returns the row RETURNING produced, with the caller's conflict action
// deciding what an existing row means: replace it (the unguarded save, which
// always returns a row) or leave it alone (the create assertion's ON
// CONFLICT DO NOTHING, which yields sql.ErrNoRows on conflict — the "nothing
// written" case UpdateSettingsVersioned's create arm relies on).
func (s *orgsStore) upsertSettings(ctx context.Context, orgID string, u domain.OrgSettings, conflict string) (domain.OrgSettings, error) {
	stored, err := db.ScanOrgSettingsCore(s.app.QueryRowContext(ctx, `
		INSERT INTO org_settings (
			org_id, github_clone_protocol,
			anthropic_api_key_ref, bedrock_credentials_ref, enabled_models,
			background_jobs_model, llm_auth_method,
			max_daily_cost_usd, max_concurrent_runs, marketplace_enabled,
			api_token_max_age_days,
			version, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 1, now()
		)`+conflict+`
		RETURNING `+orgSettingsColumns, orgSettingsWriteArgs(orgID, u)...).Scan)
	return s.finishSettingsWrite(ctx, orgID, u, stored, err)
}

// updateSettingsAtVersion writes the same columns as an ordinary UPDATE under
// the row's concurrency token and returns the row RETURNING produced. It
// never creates a row: a caller that asserted a version read one, and if that
// row is gone the honest answer is the same sql.ErrNoRows a moved version
// gets — WHERE matches nothing, so RETURNING produces nothing.
//
// Its SET list must stay in step with orgSettingsConflictUpdate above — same
// columns, same exclusions. github_credential_class is absent from both for the
// reason UpdateSettings' doc gives.
func (s *orgsStore) updateSettingsAtVersion(ctx context.Context, orgID string, u domain.OrgSettings, expected int) (domain.OrgSettings, error) {
	args := append(orgSettingsWriteArgs(orgID, u), expected)
	stored, err := db.ScanOrgSettingsCore(s.app.QueryRowContext(ctx, `
		UPDATE org_settings SET
			github_clone_protocol = $2,
			anthropic_api_key_ref = $3,
			bedrock_credentials_ref = $4,
			enabled_models = $5,
			background_jobs_model = $6,
			llm_auth_method = $7,
			max_daily_cost_usd = $8,
			max_concurrent_runs = $9,
			marketplace_enabled = $10,
			api_token_max_age_days = $11,
			version = version + 1,
			updated_at = now()
		WHERE org_id = $1 AND version = $12
		RETURNING `+orgSettingsColumns, args...).Scan)
	return s.finishSettingsWrite(ctx, orgID, u, stored, err)
}

// finishSettingsWrite is the shared tail of upsertSettings and
// updateSettingsAtVersion: given the org_settings statement's own result, it
// either propagates a failed/no-match write untouched (a version conflict or
// a losing create must write NOTHING, org_event_sources included, so this
// returns before touching it) or, on success, upserts the
// github/jira org_event_sources rows from u and merges those four fields into
// the row it hands back. Both writes land in the same transaction as the
// org_settings statement (the shared s.app connection, itself the caller's
// claims-bound tx), so a rollback after this point undoes both halves
// together — ordinary transaction atomicity is what gives the guarded caller
// its "nothing written on conflict" contract; there is no second version
// token to invent (see the org_settings.version schema comment).
func (s *orgsStore) finishSettingsWrite(ctx context.Context, orgID string, u domain.OrgSettings, stored domain.OrgSettings, err error) (domain.OrgSettings, error) {
	if err != nil {
		return domain.OrgSettings{}, err
	}
	if err := upsertSourceOverride(ctx, s.app, orgID, "github", u.GitHubBaseURL, u.GitHubPollInterval); err != nil {
		return domain.OrgSettings{}, fmt.Errorf("upsert github source config: %w", err)
	}
	if err := upsertSourceOverride(ctx, s.app, orgID, "jira", u.JiraBaseURL, u.JiraPollInterval); err != nil {
		return domain.OrgSettings{}, fmt.Errorf("upsert jira source config: %w", err)
	}
	stored.GitHubBaseURL = u.GitHubBaseURL
	stored.GitHubPollInterval = u.GitHubPollInterval
	stored.JiraBaseURL = u.JiraBaseURL
	stored.JiraPollInterval = u.JiraPollInterval
	return stored, nil
}

// SetGitHubCredentialClass upserts ONLY org_settings.github_credential_class —
// which credential system the org's GitHub access belongs to. See the
// OrgsStore interface doc for why this is a separate writer from
// UpdateSettings.
//
// App pool, unlike the team-settings cap writer it otherwise mirrors: every
// caller is an org-admin-gated handler already running inside a claims-bound
// transaction alongside the credential write this class describes, which is
// exactly what org_settings_insert / org_settings_update ask for. Reaching for
// the admin pool here would take the write out of that transaction's RLS
// context for no reason.
//
// The partial INSERT relies on the schema DEFAULT clauses for every other
// org_settings column when no row exists yet, and ON CONFLICT touches only the
// class, so the org's other settings are never clobbered.
func (s *orgsStore) SetGitHubCredentialClass(ctx context.Context, orgID string, class domain.GitHubCredentialClass) (domain.OrgSettings, error) {
	stored, err := db.ScanOrgSettingsCore(s.app.QueryRowContext(ctx, `
		INSERT INTO org_settings (org_id, github_credential_class, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (org_id) DO UPDATE SET
			github_credential_class = EXCLUDED.github_credential_class,
			updated_at = now()
		RETURNING `+orgSettingsColumns,
		orgID, string(class)).Scan)
	if err != nil {
		return domain.OrgSettings{}, fmt.Errorf("set org github credential class: %w", err)
	}
	// This writer doesn't touch org_event_sources — read the org's current
	// base_url / poll_interval rather than leave them zero, so the row this
	// hands back still matches what a follow-up GetSettings finds.
	github, jira, err := readSourceOverrides(ctx, s.app, orgID)
	if err != nil {
		return domain.OrgSettings{}, fmt.Errorf("read org_event_sources overrides: %w", err)
	}
	db.ApplyOrgSourceOverrides(&stored, github, jira)
	return stored, nil
}

// secondsToDuration converts a Postgres EXTRACT(EPOCH FROM interval)
// reading (seconds, double precision) to time.Duration. The naive
// time.Duration(secs * float64(time.Second)) truncates the float-to-int
// conversion, drifting by up to a nanosecond per round-trip. Rounding
// to the nearest nanosecond pins the value at the precision Go's
// Duration actually represents — and stays exact for the
// minute-granularity poll intervals we round-trip in practice.
func secondsToDuration(secs float64) time.Duration {
	return time.Duration(math.Round(secs * float64(time.Second)))
}
