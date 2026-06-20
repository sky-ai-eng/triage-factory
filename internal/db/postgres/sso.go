package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ssoConnectionStore is the Postgres impl of db.SSOConnectionStore. Holds
// both pools — see the SSOConnectionStore interface comment for the
// pool-split rationale.
//
//   - app: Create, GetByID, ListByOrg, Update, Delete. Request-handler
//     reads/writes gated by the sso_connections_* RLS policies (org-admin
//     in the current org).
//   - admin: GetByProviderID. The login-time JIT actor has no membership in
//     the target org, so RLS can't express the lookup; the provider_id is
//     the authorization.
type ssoConnectionStore struct {
	app   queryer
	admin queryer
}

func newSSOConnectionStore(app, admin queryer) db.SSOConnectionStore {
	return &ssoConnectionStore{app: app, admin: admin}
}

var _ db.SSOConnectionStore = (*ssoConnectionStore)(nil)

func (s *ssoConnectionStore) Create(ctx context.Context, p domain.CreateSSOConnectionParams) (string, error) {
	// Mirror the schema DEFAULTs in Go so an unset field still produces a
	// valid row; enabled isn't a param (new connections are always created
	// enabled — see CreateSSOConnectionParams), so it falls to the column
	// DEFAULT.
	kind := p.Kind
	if kind == "" {
		kind = "saml"
	}
	role := p.DefaultRole
	if role == "" {
		role = "member"
	}
	var id string
	err := s.app.QueryRowContext(ctx, `
		INSERT INTO sso_connections (org_id, kind, provider_id, default_role)
		VALUES ($1, $2, $3, $4)
		RETURNING id::text
	`, p.OrgID, kind, p.ProviderID, role).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("insert sso_connection: %w", err)
	}
	return id, nil
}

func (s *ssoConnectionStore) GetByID(ctx context.Context, orgID, id string) (*domain.SSOConnection, error) {
	var c domain.SSOConnection
	err := s.app.QueryRowContext(ctx, `
		SELECT id::text, org_id::text, kind, provider_id, default_role::text,
		       enabled, created_at, updated_at
		FROM sso_connections
		WHERE id = $1 AND org_id = $2
	`, id, orgID).Scan(
		&c.ID, &c.OrgID, &c.Kind, &c.ProviderID, &c.DefaultRole,
		&c.Enabled, &c.CreatedAt, &c.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get sso_connection by id: %w", err)
	}
	return &c, nil
}

func (s *ssoConnectionStore) ListByOrg(ctx context.Context, orgID string) ([]domain.SSOConnection, error) {
	rows, err := s.app.QueryContext(ctx, `
		SELECT id::text, org_id::text, kind, provider_id, default_role::text,
		       enabled, created_at, updated_at
		FROM sso_connections
		WHERE org_id = $1
		ORDER BY created_at DESC, id ASC
	`, orgID)
	if err != nil {
		return nil, fmt.Errorf("list sso_connections: %w", err)
	}
	defer rows.Close()

	out := []domain.SSOConnection{}
	for rows.Next() {
		var c domain.SSOConnection
		if err := rows.Scan(
			&c.ID, &c.OrgID, &c.Kind, &c.ProviderID, &c.DefaultRole,
			&c.Enabled, &c.CreatedAt, &c.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan sso_connection: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *ssoConnectionStore) Update(ctx context.Context, orgID, id string, p domain.UpdateSSOConnectionParams) error {
	role := p.DefaultRole
	if role == "" {
		role = "member"
	}
	if _, err := s.app.ExecContext(ctx, `
		UPDATE sso_connections SET default_role = $1, enabled = $2
		WHERE id = $3 AND org_id = $4
	`, role, p.Enabled, id, orgID); err != nil {
		return fmt.Errorf("update sso_connection: %w", err)
	}
	return nil
}

func (s *ssoConnectionStore) Delete(ctx context.Context, orgID, id string) error {
	if _, err := s.app.ExecContext(ctx, `
		DELETE FROM sso_connections WHERE id = $1 AND org_id = $2
	`, id, orgID); err != nil {
		return fmt.Errorf("delete sso_connection: %w", err)
	}
	return nil
}

func (s *ssoConnectionStore) GetByProviderID(ctx context.Context, providerID string) (*domain.SSOProviderBinding, error) {
	var b domain.SSOProviderBinding
	err := s.admin.QueryRowContext(ctx, `
		SELECT org_id::text, default_role::text, enabled
		FROM sso_connections
		WHERE provider_id = $1
	`, providerID).Scan(&b.OrgID, &b.DefaultRole, &b.Enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get sso_connection by provider_id: %w", err)
	}
	return &b, nil
}

// ssoDomainStore is the Postgres impl of db.SSODomainStore. Holds both
// pools — see the SSODomainStore interface comment for the rationale.
//
//   - app: Create, ListByOrg, GetByID, SetVerified, Delete. Request-handler
//     reads/writes gated by the sso_domains_* RLS policies (org-admin).
//   - admin: GetVerifiedByDomain. The routing actor is pre-login with no
//     membership; the verified domain is the authorization.
type ssoDomainStore struct {
	app   queryer
	admin queryer
}

func newSSODomainStore(app, admin queryer) db.SSODomainStore {
	return &ssoDomainStore{app: app, admin: admin}
}

var _ db.SSODomainStore = (*ssoDomainStore)(nil)

func (s *ssoDomainStore) Create(ctx context.Context, p domain.CreateSSODomainParams) (string, error) {
	var id string
	err := s.app.QueryRowContext(ctx, `
		INSERT INTO sso_domains (connection_id, org_id, domain, token)
		VALUES ($1, $2, $3, $4)
		RETURNING id::text
	`, p.ConnectionID, p.OrgID, p.Domain, p.Token).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("insert sso_domain: %w", err)
	}
	return id, nil
}

func (s *ssoDomainStore) ListByOrg(ctx context.Context, orgID string) ([]domain.SSODomain, error) {
	rows, err := s.app.QueryContext(ctx, `
		SELECT id::text, connection_id::text, org_id::text, domain, token,
		       verified_at, created_at, updated_at
		FROM sso_domains
		WHERE org_id = $1
		ORDER BY created_at DESC, id ASC
	`, orgID)
	if err != nil {
		return nil, fmt.Errorf("list sso_domains: %w", err)
	}
	defer rows.Close()

	out := []domain.SSODomain{}
	for rows.Next() {
		d, err := scanSSODomain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *ssoDomainStore) GetByID(ctx context.Context, orgID, id string) (*domain.SSODomain, error) {
	d, err := scanSSODomain(s.app.QueryRowContext(ctx, `
		SELECT id::text, connection_id::text, org_id::text, domain, token,
		       verified_at, created_at, updated_at
		FROM sso_domains
		WHERE id = $1 AND org_id = $2
	`, id, orgID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get sso_domain by id: %w", err)
	}
	return &d, nil
}

func (s *ssoDomainStore) SetVerified(ctx context.Context, orgID, id string) error {
	// Only stamp a still-pending row. The verified-global-unique index
	// (sso_domains_verified_global_uniq) fires here if another org already
	// verified the same domain — the error propagates (opaquely) and the
	// surrounding tx rolls back. Re-verifying an already-verified own row is
	// idempotent against the WHERE guard (zero rows, no error).
	if _, err := s.app.ExecContext(ctx, `
		UPDATE sso_domains SET verified_at = now()
		WHERE id = $1 AND org_id = $2 AND verified_at IS NULL
	`, id, orgID); err != nil {
		return fmt.Errorf("set sso_domain verified: %w", err)
	}
	return nil
}

func (s *ssoDomainStore) Delete(ctx context.Context, orgID, id string) error {
	if _, err := s.app.ExecContext(ctx, `
		DELETE FROM sso_domains WHERE id = $1 AND org_id = $2
	`, id, orgID); err != nil {
		return fmt.Errorf("delete sso_domain: %w", err)
	}
	return nil
}

func (s *ssoDomainStore) GetVerifiedByDomain(ctx context.Context, domainName string) (*domain.SSODomainRoute, error) {
	// Exact lower(domain) match, verified rows only — no suffix / longest-
	// match (corp.com never matches eng.corp.com). The verified-global-
	// unique index guarantees at most one row, so no ordering/limit needed.
	//
	// The join also pins c.org_id = d.org_id: the sso_domains_connection_fkey
	// composite FK already guarantees the pair is consistent, so this is
	// defense-in-depth — a belt-and-suspenders guard that a mismatched row
	// (from a future bug or manual write) can never route a verified domain
	// to a connection in a different org.
	var r domain.SSODomainRoute
	err := s.admin.QueryRowContext(ctx, `
		SELECT d.connection_id::text, d.org_id::text, c.provider_id, c.enabled
		FROM sso_domains d
		JOIN sso_connections c ON c.id = d.connection_id AND c.org_id = d.org_id
		WHERE lower(d.domain) = lower($1) AND d.verified_at IS NOT NULL
	`, domainName).Scan(&r.ConnectionID, &r.OrgID, &r.ProviderID, &r.Enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get verified sso_domain: %w", err)
	}
	return &r, nil
}

// rowScanner is the read shape shared by *sql.Row and *sql.Rows, so
// scanSSODomain serves both the single-row GetByID and the iterating
// ListByOrg without duplicating the column list.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanSSODomain(sc rowScanner) (domain.SSODomain, error) {
	var (
		d          domain.SSODomain
		verifiedAt sql.NullTime
	)
	if err := sc.Scan(
		&d.ID, &d.ConnectionID, &d.OrgID, &d.Domain, &d.Token,
		&verifiedAt, &d.CreatedAt, &d.UpdatedAt,
	); err != nil {
		return domain.SSODomain{}, err
	}
	if verifiedAt.Valid {
		d.VerifiedAt = &verifiedAt.Time
	}
	return d, nil
}
