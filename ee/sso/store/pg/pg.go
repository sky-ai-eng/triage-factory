// Package pg holds the Postgres implementations of the Enterprise Edition
// SSO stores and registers them with core's store-extension registry for
// the "postgres" dialect: same app/admin pool split and RLS posture as the
// rest of the Postgres data layer, reading core's transaction handle as a
// db.Execer and the domain/interface types from ssostore.
//
// Enterprise Edition — governed by the repository-root LICENSE (Triage Factory
// License 1.0); enabling its features requires a valid license key.
package pg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	ssostore "github.com/sky-ai-eng/triage-factory/ee/sso/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
)

func init() {
	db.RegisterStoreExtension("postgres", ssostore.ExtKey, func(app, admin db.Execer) any {
		return &ssostore.Bundle{
			Connections: newSSOConnectionStore(app, admin),
			Domains:     newSSODomainStore(app, admin),
			BreakGlass:  newSSOBreakGlassStore(app, admin),
		}
	})
}

// rowScanner is the read shape shared by *sql.Row and *sql.Rows, so
// scanSSODomain + scanSSOConnection serve both the single-row GetByID and
// the iterating ListByOrg without duplicating the column list.
type rowScanner interface {
	Scan(dest ...any) error
}

// ssoConnectionStore is the Postgres impl of ssostore.SSOConnectionStore.
//
//   - app: Create, GetByID, ListByOrg, Update, Delete. Request-handler
//     reads/writes gated by the sso_connections_* RLS policies.
//   - admin: GetByProviderID / MarkTestedByProviderID. The login/test
//     actor has no membership; the provider_id is the authorization.
type ssoConnectionStore struct {
	app   db.Execer
	admin db.Execer
}

func newSSOConnectionStore(app, admin db.Execer) ssostore.SSOConnectionStore {
	return &ssoConnectionStore{app: app, admin: admin}
}

var _ ssostore.SSOConnectionStore = (*ssoConnectionStore)(nil)

func (s *ssoConnectionStore) Create(ctx context.Context, p ssostore.CreateSSOConnectionParams) (string, error) {
	// Mirror the schema DEFAULTs in Go so an unset field still produces a
	// valid row; enabled isn't a param (new connections are always created
	// enabled), so it falls to the column DEFAULT.
	kind := p.Kind
	if kind == "" {
		kind = "saml"
	}
	role := p.DefaultRole
	if role == "" {
		role = "member"
	}
	var id string
	// An unknown IdP is NULL, not '': the CHECK admits only the named values,
	// and "" is the Go spelling of the column being absent.
	err := s.app.QueryRowContext(ctx, `
		INSERT INTO sso_connections (org_id, kind, provider_id, default_role, idp)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''))
		RETURNING id::text
	`, p.OrgID, kind, p.ProviderID, role, p.IdP).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("insert sso_connection: %w", err)
	}
	return id, nil
}

func (s *ssoConnectionStore) GetByID(ctx context.Context, orgID, id string) (*ssostore.SSOConnection, error) {
	c, err := scanSSOConnection(s.app.QueryRowContext(ctx, `
		SELECT id::text, org_id::text, kind, provider_id, default_role::text,
		       enabled, enforced, last_tested_at, COALESCE(idp, ''), created_at, updated_at
		FROM sso_connections
		WHERE id = $1 AND org_id = $2
	`, id, orgID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get sso_connection by id: %w", err)
	}
	return &c, nil
}

func (s *ssoConnectionStore) ListByOrg(ctx context.Context, orgID string) ([]ssostore.SSOConnection, error) {
	rows, err := s.app.QueryContext(ctx, `
		SELECT id::text, org_id::text, kind, provider_id, default_role::text,
		       enabled, enforced, last_tested_at, COALESCE(idp, ''), created_at, updated_at
		FROM sso_connections
		WHERE org_id = $1
		ORDER BY created_at DESC, id ASC
	`, orgID)
	if err != nil {
		return nil, fmt.Errorf("list sso_connections: %w", err)
	}
	defer rows.Close()

	out := []ssostore.SSOConnection{}
	for rows.Next() {
		c, err := scanSSOConnection(rows)
		if err != nil {
			return nil, fmt.Errorf("scan sso_connection: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// scanSSOConnection reads the full connection column list (shared by
// GetByID's single-row read and ListByOrg's iteration) into a
// ssostore.SSOConnection, unwrapping the nullable last_tested_at. idp
// arrives COALESCEd to ” by the SELECT, which is the type's own spelling of
// "not known".
func scanSSOConnection(sc rowScanner) (ssostore.SSOConnection, error) {
	var (
		c          ssostore.SSOConnection
		lastTested sql.NullTime
	)
	if err := sc.Scan(
		&c.ID, &c.OrgID, &c.Kind, &c.ProviderID, &c.DefaultRole,
		&c.Enabled, &c.Enforced, &lastTested, &c.IdP, &c.CreatedAt, &c.UpdatedAt,
	); err != nil {
		return ssostore.SSOConnection{}, err
	}
	if lastTested.Valid {
		c.LastTestedAt = &lastTested.Time
	}
	return c, nil
}

func (s *ssoConnectionStore) Update(ctx context.Context, orgID, id string, p ssostore.UpdateSSOConnectionParams) error {
	// An empty DefaultRole means "leave the role unchanged" — COALESCE
	// preserves the stored value. A non-empty 'owner' still trips the
	// sso_connections_role_not_owner CHECK.
	//
	// IdP is three-valued, and the two nils are told apart by a separate
	// flag: a nil pointer keeps the column, a pointer to "" writes NULL, and
	// anything else is written as-is (the CHECK refuses a value outside the
	// vocabulary, which the handler has already validated).
	var (
		setIdP bool
		idp    string
	)
	if p.IdP != nil {
		setIdP, idp = true, *p.IdP
	}
	if _, err := s.app.ExecContext(ctx, `
		UPDATE sso_connections
		SET default_role = COALESCE(NULLIF($1, '')::public.org_role, default_role),
		    enabled = $2,
		    idp = CASE WHEN $3 THEN NULLIF($4, '') ELSE idp END
		WHERE id = $5 AND org_id = $6
	`, p.DefaultRole, p.Enabled, setIdP, idp, id, orgID); err != nil {
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

func (s *ssoConnectionStore) GetByProviderID(ctx context.Context, providerID string) (*ssostore.SSOProviderBinding, error) {
	var b ssostore.SSOProviderBinding
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

func (s *ssoConnectionStore) IdPsByProviderIDs(ctx context.Context, providerIDs []string) (map[string]string, error) {
	out := map[string]string{}
	if len(providerIDs) == 0 {
		return out, nil
	}
	// Admin pool, like GetByProviderID: the ids come from identity rows the
	// principal owns, and a connection's IdP is not an org secret. Rows with
	// no idp are filtered here rather than mapped to "", so the map answers
	// only what is known.
	rows, err := s.admin.QueryContext(ctx, `
		SELECT provider_id, idp
		FROM sso_connections
		WHERE provider_id = ANY($1) AND idp IS NOT NULL
	`, providerIDs)
	if err != nil {
		return nil, fmt.Errorf("idps by provider_id: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var pid, idp string
		if err := rows.Scan(&pid, &idp); err != nil {
			return nil, fmt.Errorf("scan idp by provider_id: %w", err)
		}
		out[pid] = idp
	}
	return out, rows.Err()
}

func (s *ssoConnectionStore) SetEnforced(ctx context.Context, orgID, id string, enforced bool) error {
	if _, err := s.app.ExecContext(ctx, `
		UPDATE sso_connections SET enforced = $1
		WHERE id = $2 AND org_id = $3
	`, enforced, id, orgID); err != nil {
		return fmt.Errorf("set sso_connection enforced: %w", err)
	}
	return nil
}

func (s *ssoConnectionStore) MarkTestedByProviderID(ctx context.Context, providerID string) error {
	// Admin pool: the verify-before-enforce Test callback carries TF's
	// signed state (the provider_id) but no membership claims, so this
	// can't go through RLS — the provider_id is the lookup authorization,
	// mirroring GetByProviderID.
	if _, err := s.admin.ExecContext(ctx, `
		UPDATE sso_connections SET last_tested_at = now()
		WHERE provider_id = $1
	`, providerID); err != nil {
		return fmt.Errorf("mark sso_connection tested: %w", err)
	}
	return nil
}

// ssoDomainStore is the Postgres impl of ssostore.SSODomainStore.
//
//   - app: Create, ListByOrg, GetByID, SetVerified, Delete (sso_domains_*
//     RLS, org-admin).
//   - admin: GetVerifiedByDomain. The routing actor is pre-login; the
//     verified domain is the authorization.
type ssoDomainStore struct {
	app   db.Execer
	admin db.Execer
}

func newSSODomainStore(app, admin db.Execer) ssostore.SSODomainStore {
	return &ssoDomainStore{app: app, admin: admin}
}

var _ ssostore.SSODomainStore = (*ssoDomainStore)(nil)

func (s *ssoDomainStore) Create(ctx context.Context, p ssostore.CreateSSODomainParams) (string, error) {
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

func (s *ssoDomainStore) ListByOrg(ctx context.Context, orgID string) ([]ssostore.SSODomain, error) {
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

	out := []ssostore.SSODomain{}
	for rows.Next() {
		d, err := scanSSODomain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *ssoDomainStore) GetByID(ctx context.Context, orgID, id string) (*ssostore.SSODomain, error) {
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
	// surrounding tx rolls back. Re-verifying an already-verified own row
	// is idempotent against the WHERE guard.
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

func (s *ssoDomainStore) GetVerifiedByDomain(ctx context.Context, domainName string) (*ssostore.SSODomainRoute, error) {
	// Exact lower(domain) match, verified rows only — no suffix / longest-
	// match. The verified-global-unique index guarantees at most one row.
	// The join pins c.org_id = d.org_id (defense-in-depth on top of the
	// composite FK) so a mismatched row can never route a verified domain
	// to a connection in a different org.
	var r ssostore.SSODomainRoute
	err := s.admin.QueryRowContext(ctx, `
		SELECT d.connection_id::text, d.org_id::text, c.provider_id, c.enabled, c.enforced
		FROM sso_domains d
		JOIN sso_connections c ON c.id = d.connection_id AND c.org_id = d.org_id
		WHERE lower(d.domain) = lower($1) AND d.verified_at IS NOT NULL
	`, domainName).Scan(&r.ConnectionID, &r.OrgID, &r.ProviderID, &r.Enabled, &r.Enforced)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get verified sso_domain: %w", err)
	}
	return &r, nil
}

func scanSSODomain(sc rowScanner) (ssostore.SSODomain, error) {
	var (
		d          ssostore.SSODomain
		verifiedAt sql.NullTime
	)
	if err := sc.Scan(
		&d.ID, &d.ConnectionID, &d.OrgID, &d.Domain, &d.Token,
		&verifiedAt, &d.CreatedAt, &d.UpdatedAt,
	); err != nil {
		return ssostore.SSODomain{}, err
	}
	if verifiedAt.Valid {
		d.VerifiedAt = &verifiedAt.Time
	}
	return d, nil
}

// ssoBreakGlassStore is the Postgres impl of ssostore.SSOBreakGlassStore.
//
//   - app: Add, RemoveGuarded, SeedOwnerIfEmpty (sso_break_glass_* RLS).
//   - admin: List, IsBreakGlass, ResolveVerifiedEmailMember — read
//     cross-principal identity data behind an admin-gated/login-time caller.
type ssoBreakGlassStore struct {
	app   db.Execer
	admin db.Execer
}

func newSSOBreakGlassStore(app, admin db.Execer) ssostore.SSOBreakGlassStore {
	return &ssoBreakGlassStore{app: app, admin: admin}
}

var _ ssostore.SSOBreakGlassStore = (*ssoBreakGlassStore)(nil)

func (s *ssoBreakGlassStore) List(ctx context.Context, orgID string) ([]ssostore.SSOBreakGlassPrincipal, error) {
	// Admin pool: the email comes from user_identities, admin-pool-only by
	// the principal-model design. The handler has gated org-admin and this
	// read is strictly scoped to bg.org_id = $1, so admin leaks nothing
	// cross-org. Newest designation first.
	rows, err := s.admin.QueryContext(ctx, `
		SELECT bg.user_id::text,
		       COALESCE(u.display_name, ''),
		       COALESCE((
		           SELECT i.email FROM user_identities i
		            WHERE i.user_id = bg.user_id AND i.email IS NOT NULL
		            ORDER BY i.email_verified DESC, i.updated_at DESC
		            LIMIT 1
		       ), ''),
		       bg.created_at
		FROM sso_break_glass bg
		JOIN users u ON u.id = bg.user_id
		WHERE bg.org_id = $1
		ORDER BY bg.created_at DESC, bg.user_id ASC
	`, orgID)
	if err != nil {
		return nil, fmt.Errorf("list sso_break_glass: %w", err)
	}
	defer rows.Close()

	out := []ssostore.SSOBreakGlassPrincipal{}
	for rows.Next() {
		var p ssostore.SSOBreakGlassPrincipal
		if err := rows.Scan(&p.UserID, &p.DisplayName, &p.Email, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan sso_break_glass: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *ssoBreakGlassStore) Add(ctx context.Context, orgID, userID string) (bool, error) {
	// RETURNING yields no row on the DO NOTHING path, so sql.ErrNoRows IS the
	// "already a principal" answer — the insert's own outcome, not a separate
	// read that a concurrent Add could slip past.
	var added bool
	err := s.app.QueryRowContext(ctx, `
		INSERT INTO sso_break_glass (org_id, user_id) VALUES ($1, $2)
		ON CONFLICT (org_id, user_id) DO NOTHING
		RETURNING true
	`, orgID, userID).Scan(&added)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("add sso_break_glass: %w", err)
	}
	return added, nil
}

func (s *ssoBreakGlassStore) RemoveGuarded(ctx context.Context, orgID, userID string) (removed, blockedLast bool, err error) {
	// One statement so the enforced-check, count, presence, and delete all
	// read a SINGLE snapshot — the lockout-safety invariant ("never
	// enforced with zero break-glass") can't be raced. The DELETE fires
	// UNLESS the org has an enforced connection AND this would drop the last
	// principal. removed is true exactly when a row went away; blocked_last is
	// true exactly when the row was present but the guard prevented its
	// removal. Both false is the no-such-principal case, which the caller must
	// distinguish so it records no phantom revocation. App pool — RLS-gated to
	// org-admin.
	if err := s.app.QueryRowContext(ctx, `
		WITH del AS (
			DELETE FROM sso_break_glass
			WHERE org_id = $1 AND user_id = $2
			  AND NOT (
			      EXISTS (SELECT 1 FROM sso_connections WHERE org_id = $1 AND enforced)
			      AND (SELECT count(*) FROM sso_break_glass WHERE org_id = $1) <= 1
			  )
			RETURNING 1
		)
		SELECT EXISTS (SELECT 1 FROM del),
		       NOT EXISTS (SELECT 1 FROM del)
		   AND EXISTS (SELECT 1 FROM sso_break_glass WHERE org_id = $1 AND user_id = $2)
	`, orgID, userID).Scan(&removed, &blockedLast); err != nil {
		return false, false, fmt.Errorf("remove sso_break_glass (guarded): %w", err)
	}
	return removed, blockedLast, nil
}

func (s *ssoBreakGlassStore) SeedOwnerIfEmpty(ctx context.Context, orgID string) error {
	// Insert the org owner only when the set is empty — the "default: the
	// owner" behavior. The NOT EXISTS guard keeps it from re-adding an owner
	// an admin deliberately removed (while another principal stands in).
	if _, err := s.app.ExecContext(ctx, `
		INSERT INTO sso_break_glass (org_id, user_id)
		SELECT o.id, o.owner_user_id
		FROM orgs o
		WHERE o.id = $1
		  AND NOT EXISTS (SELECT 1 FROM sso_break_glass bg WHERE bg.org_id = o.id)
		ON CONFLICT (org_id, user_id) DO NOTHING
	`, orgID); err != nil {
		return fmt.Errorf("seed owner sso_break_glass: %w", err)
	}
	return nil
}

func (s *ssoBreakGlassStore) ResolveVerifiedEmailMember(ctx context.Context, orgID, email string) (string, error) {
	// Admin pool: resolving an arbitrary email to its principal reads
	// user_identities rows that don't belong to the caller. The
	// org_memberships join confines the result to a member of orgID.
	var userID string
	err := s.admin.QueryRowContext(ctx, `
		SELECT om.user_id::text
		FROM org_memberships om
		WHERE om.org_id = $1
		  AND om.user_id IN (
		      SELECT i.user_id FROM user_identities i
		      WHERE lower(i.email) = lower($2) AND i.email_verified
		  )
		LIMIT 1
	`, orgID, email).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve verified-email member: %w", err)
	}
	return userID, nil
}

func (s *ssoBreakGlassStore) IsBreakGlass(ctx context.Context, orgID, userID string) (bool, error) {
	var ok bool
	if err := s.admin.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM sso_break_glass WHERE org_id = $1 AND user_id = $2)
	`, orgID, userID).Scan(&ok); err != nil {
		return false, fmt.Errorf("is sso_break_glass: %w", err)
	}
	return ok, nil
}
