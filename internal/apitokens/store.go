// Package apitokens owns public.user_api_tokens: the per-user, org-scoped
// bearer credentials a client authenticates with when it has no browser to
// drive a session cookie through.
//
// A token is a sealed session cursor. A session is (user, movable active_org);
// a token is the same pair with the org fixed at mint, which is why every read
// here is keyed by both. The plaintext exists in one place — the return value of
// MintSystem — and nowhere after that; the row holds sha256 of the full literal
// and eleven characters of it for display.
//
// Postgres-only, like sessions: local mode's synthetic identity is already
// headless, so nothing there needs a token and no SQLite twin exists. That is
// also why this store deliberately stays outside db.Stores, whose contract is
// that every method has an implementation in both dialects.
package apitokens

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// Store wraps the public.user_api_tokens table. The DB handle must be the
// admin (BYPASSRLS) pool — a token lookup happens BEFORE the request has any
// claims to install via SET LOCAL request.jwt.claims, so the table's RLS
// policies cannot be what scopes the lookup. (They still stand, defending the
// app-pool reader a "list my tokens" surface would use; same posture as
// internal/sessions.)
//
// Every method carries the `…System` suffix to make the admin-pool routing
// explicit at call sites, matching the dual-pool convention in internal/db. The
// whole type is admin-only by construction, so the suffix advertises the
// contract rather than disambiguating two pools; non-System counterparts do not
// exist. user_id is bound by argument everywhere, and it is the only thing
// standing between one user's tokens and another's.
type Store struct {
	db *sql.DB
}

// NewStore wires the store. Caller owns the *sql.DB lifecycle.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// ErrNoSuchToken reports a token id that names no row the caller owns, or one
// already revoked. Both collapse to the same answer on purpose: a revoke is
// idempotent from the caller's side only in that the token ends up dead, and a
// second attempt should say so rather than report fresh success.
var ErrNoSuchToken = errors.New("no such API token")

// ErrTokenLimit reports that the (user, org) pair already holds MaxPerUserOrg
// live tokens.
var ErrTokenLimit = fmt.Errorf("at most %d active API tokens per user per org", MaxPerUserOrg)

// Token is one row, as everything but the mint call ever sees it: no secret,
// only the prefix.
type Token struct {
	ID     string
	UserID string
	OrgID  string
	Name   string
	// Prefix is the first eleven characters of the plaintext ("tf_" + 8).
	Prefix string
	// AllowedCIDRs is the optional IP allowlist, canonical form, nil when the
	// token carries none (which means no restriction, not "deny all").
	AllowedCIDRs []string
	CreatedAt    time.Time
	LastUsedAt   *time.Time
	// ExpiresAt is the expiry stored on the row, nil when the minter asked for
	// none.
	ExpiresAt *time.Time
	// EffectiveExpiresAt is when this token actually stops working:
	// min(ExpiresAt, CreatedAt + the org's api_token_max_age_days), nil when
	// neither bound applies. Derived against the cap AS IT IS NOW, so it moves
	// when an admin moves the cap — reading it is the only honest way to answer
	// "how long does this token have left".
	EffectiveExpiresAt *time.Time
	RevokedAt          *time.Time
}

// Identity is what a valid token resolves to: who the bearer acts as, and the
// one org they act in. It is the token's answer to the question a session
// cookie answers with (user, active_org).
type Identity struct {
	TokenID string
	UserID  string
	OrgID   string
	// Email is the address the principal's login identity carries, empty when
	// none does. Display and claims material only; the token, not the address,
	// is what authenticated.
	Email string
	// AllowedCIDRs is the token's IP allowlist, nil when it has none. Lookup
	// carries the ranges and decides nothing about them: matching a request's
	// address against them belongs where that address is resolved.
	AllowedCIDRs []string
}

// mintLockSalt is this package's hashtextextended salt (see the registry in
// internal/server/advisorylock.go). Spelled as the ASCII of "TOKN" so it cannot
// collide with the small integers the other keyspaces use.
const mintLockSalt = 0x544f4b4e

// tokenColumns is the canonical projection of a user_api_tokens row, in the
// order scanToken reads them. Mint RETURNs it and List SELECTs it, so the write
// shape cannot drift from the read shape.
//
// The row alias is `t` and the org's settings row is `os`; every statement
// using this projection provides both. array_to_json(...)::text round-trips
// cidr[] as a JSON literal — database/sql + pgx stdlib ships no scanner for
// *[]string, the same detour team_settings takes for its text[] columns.
//
// LEAST ignores NULLs, which is exactly the arithmetic the effective expiry
// wants: an absent stored expiry and an absent cap each mean "no bound from
// this side", and only both absent means never.
const tokenColumns = `t.id::text, t.user_id::text, t.org_id::text, t.name, t.token_prefix,
		array_to_json(t.allowed_cidrs)::text,
		t.created_at, t.last_used_at, t.expires_at,
		LEAST(t.expires_at, t.created_at + make_interval(days => os.api_token_max_age_days)),
		t.revoked_at`

// tokenFrom is the FROM clause tokenColumns is written against. The join to
// org_settings is LEFT because an org may hold no settings row at all, which
// reads as an uncapped org rather than a token nobody can see.
const tokenFrom = `public.user_api_tokens t
		  LEFT JOIN public.org_settings os ON os.org_id = t.org_id`

// liveTokenPredicate is what "this token still authenticates" means: not
// revoked, not past its own stored expiry, and not past the org's max-age cap
// as that cap reads RIGHT NOW. Written against the `t`/`os` aliases tokenFrom
// provides, and shared by every read that asks the question, so a tightened cap
// takes effect identically wherever it is asked.
const liveTokenPredicate = `t.revoked_at IS NULL
		   AND (t.expires_at IS NULL OR t.expires_at > now())
		   AND (os.api_token_max_age_days IS NULL
		        OR t.created_at + make_interval(days => os.api_token_max_age_days) > now())`

// scanToken decodes one row in tokenColumns order.
func scanToken(scan func(...any) error) (Token, error) {
	var (
		t         Token
		cidrsJSON sql.NullString
	)
	if err := scan(
		&t.ID, &t.UserID, &t.OrgID, &t.Name, &t.Prefix,
		&cidrsJSON,
		&t.CreatedAt, &t.LastUsedAt, &t.ExpiresAt,
		&t.EffectiveExpiresAt,
		&t.RevokedAt,
	); err != nil {
		return Token{}, err
	}
	if cidrsJSON.Valid {
		if err := json.Unmarshal([]byte(cidrsJSON.String), &t.AllowedCIDRs); err != nil {
			return Token{}, fmt.Errorf("decode allowed_cidrs: %w", err)
		}
	}
	return t, nil
}

// MintSystem creates one token for (userID, orgID) and returns it alongside the
// plaintext — the only time the plaintext exists outside the caller's hand.
//
// expiresAt is stored verbatim. The org's max-age cap is NOT folded into it:
// the cap is applied at use against whatever it says then, so a stored expiry
// that outlives the current cap is a fact about what was asked for, and the cap
// in force at this moment is recorded in the audit row instead.
//
// The whole thing is one transaction, and the audit row is inside it: a token
// that exists without a log line is a credential nobody can account for. The
// count-then-insert is serialized deployment-wide by an advisory lock on
// (user, org) — without it two concurrent mints both read 49 and both insert.
func (s *Store) MintSystem(
	ctx context.Context,
	userID, orgID, name string,
	expiresAt *time.Time,
	allowedCIDRs []string,
	actorForAudit string,
) (Token, string, error) {
	cidrs, err := normalizeCIDRs(allowedCIDRs)
	if err != nil {
		return Token{}, "", err
	}
	plaintext, hash, prefix, err := generate()
	if err != nil {
		return Token{}, "", err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Token{}, "", fmt.Errorf("begin mint tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit succeeds

	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, $2))`,
		userID+":"+orgID, mintLockSalt,
	); err != nil {
		return Token{}, "", fmt.Errorf("acquire mint lock: %w", err)
	}

	// Live means "could still authenticate": not revoked, not past its own
	// stored expiry. The org cap deliberately doesn't narrow this — it moves,
	// and a token it currently hides is still a row its owner has to clean up
	// and one a loosened cap would bring back.
	var live int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM public.user_api_tokens
		 WHERE user_id = $1 AND org_id = $2
		   AND revoked_at IS NULL
		   AND (expires_at IS NULL OR expires_at > now())
	`, userID, orgID).Scan(&live); err != nil {
		return Token{}, "", fmt.Errorf("count live tokens: %w", err)
	}
	if live >= MaxPerUserOrg {
		return Token{}, "", ErrTokenLimit
	}

	// The cap in force right now, for the audit row. An org with no settings
	// row is uncapped.
	var capDays sql.NullInt64
	switch err := tx.QueryRowContext(ctx,
		`SELECT api_token_max_age_days FROM public.org_settings WHERE org_id = $1`, orgID,
	).Scan(&capDays); {
	case err == nil, errors.Is(err, sql.ErrNoRows):
	default:
		return Token{}, "", fmt.Errorf("read token max age: %w", err)
	}

	// A data-modifying CTE, not a follow-up read: the row still comes from the
	// INSERT's own RETURNING, and the outer SELECT only reaches the org's cap
	// so the returned row carries the same effective expiry a list read would.
	var cidrArg any
	if len(cidrs) > 0 {
		cidrArg = cidrs
	}
	tok, err := scanToken(tx.QueryRowContext(ctx, `
		WITH ins AS (
			INSERT INTO public.user_api_tokens
				(user_id, org_id, name, token_hash, token_prefix, allowed_cidrs, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6::text[]::cidr[], $7)
			RETURNING *
		)
		SELECT `+tokenColumns+`
		  FROM ins t
		  LEFT JOIN public.org_settings os ON os.org_id = t.org_id
	`, userID, orgID, name, hash, prefix, cidrArg, expiresAt).Scan)
	if err != nil {
		return Token{}, "", fmt.Errorf("insert api token: %w", err)
	}

	var capForAudit *int
	if capDays.Valid {
		days := int(capDays.Int64)
		capForAudit = &days
	}
	if err := recordAccessChange(ctx, tx, orgID, domain.AccessChange{
		ActorUserID: actorForAudit,
		Action:      domain.AccessActionAPITokenCreated,
		DetailJSON: domain.AccessDetailAPITokenCreated(
			tok.ID, tok.Name, tok.Prefix, tok.ExpiresAt, capForAudit, tok.AllowedCIDRs),
	}); err != nil {
		return Token{}, "", fmt.Errorf("audit token creation: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Token{}, "", fmt.Errorf("commit mint: %w", err)
	}
	return tok, plaintext, nil
}

// LookupSystem resolves a plaintext token to the identity it authenticates as,
// or (nil, nil) when it matches nothing usable — unknown, revoked, past its own
// expiry, or past the org's current max-age cap. Those collapse to one answer
// on purpose, so a caller can refuse without disclosing which.
//
// An error return means the database failed, and is not that answer: a caller
// must not read it as a token that didn't match.
func (s *Store) LookupSystem(ctx context.Context, raw string) (*Identity, error) {
	var (
		id        Identity
		cidrsJSON sql.NullString
	)
	// The email comes from the principal's login identities, not from
	// public.users, which stores none. A principal may hold several; the
	// verified one wins, then the oldest, so the answer is stable rather than
	// whichever row the planner reached first.
	err := s.db.QueryRowContext(ctx, `
		SELECT t.id::text, t.user_id::text, t.org_id::text,
		       COALESCE(em.email, ''),
		       array_to_json(t.allowed_cidrs)::text
		  FROM public.user_api_tokens t
		  LEFT JOIN public.org_settings os ON os.org_id = t.org_id
		  LEFT JOIN LATERAL (
		       SELECT ui.email
		         FROM public.user_identities ui
		        WHERE ui.user_id = t.user_id AND COALESCE(ui.email, '') <> ''
		        ORDER BY ui.email_verified DESC, ui.created_at ASC
		        LIMIT 1
		  ) em ON true
		 WHERE t.token_hash = $1
		   AND `+liveTokenPredicate+`
	`, hashOf(raw)).Scan(&id.TokenID, &id.UserID, &id.OrgID, &id.Email, &cidrsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup api token: %w", err)
	}
	if cidrsJSON.Valid {
		if err := json.Unmarshal([]byte(cidrsJSON.String), &id.AllowedCIDRs); err != nil {
			return nil, fmt.Errorf("decode allowed_cidrs: %w", err)
		}
	}
	return &id, nil
}

// IsLiveSystem answers LookupSystem's question for a token already known by
// id: is this row still one that would authenticate. It exists because the
// websocket revalidation sweep holds a token ID and not the secret — a
// connection stashes the id at handshake, never the plaintext, which exists in
// exactly one place and this is not it — and so cannot ask the question the way
// the request path does.
//
// False means the row is gone, revoked, expired, or past the org's current cap;
// an error means the database failed and is not that answer, so a caller
// deciding whether to disconnect somebody should treat it as "ask again later"
// rather than as a revocation.
func (s *Store) IsLiveSystem(ctx context.Context, tokenID string) (bool, error) {
	if !isValidUUID(tokenID) {
		return false, nil
	}
	var live bool
	err := s.db.QueryRowContext(ctx, `
		SELECT true
		  FROM `+tokenFrom+`
		 WHERE t.id = $1
		   AND `+liveTokenPredicate+`
	`, tokenID).Scan(&live)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("probe api token liveness: %w", err)
	}
	return live, nil
}

// ListForUserSystem returns the user's un-revoked tokens, newest first, with
// the filtered total under the same filters. orgFilter narrows to one org when
// set; nil means every org the user holds tokens in.
//
// Revoked rows are gone from this read — a revoked token is not a thing anyone
// can act on, and the reaper deletes it outright once the audit window passes.
// Expired ones stay: they still occupy a name their owner may want to reuse,
// and only a list that shows them makes them deletable.
func (s *Store) ListForUserSystem(ctx context.Context, userID string, orgFilter *string, opts db.ListOpts) ([]Token, int, error) {
	if !isValidUUID(userID) {
		return []Token{}, 0, nil
	}
	var org any
	if orgFilter != nil {
		if !isValidUUID(*orgFilter) {
			return []Token{}, 0, nil
		}
		org = *orgFilter
	}
	const where = `
		 WHERE t.user_id = $1
		   AND t.revoked_at IS NULL
		   AND ($2::uuid IS NULL OR t.org_id = $2)`

	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM public.user_api_tokens t`+where, userID, org,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count api tokens: %w", err)
	}
	if opts.CountOnly {
		return []Token{}, total, nil
	}

	// The id tiebreaker is what makes offset paging total: without it two rows
	// minted in the same instant can swap places between pages.
	query := `SELECT ` + tokenColumns + `
		  FROM ` + tokenFrom + where + `
		 ORDER BY t.created_at DESC, t.id`
	args := []any{userID, org}
	if opts.Limit > 0 {
		query += `
		 LIMIT $3 OFFSET $4`
		args = append(args, opts.Limit, opts.Offset)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list api tokens: %w", err)
	}
	defer rows.Close()

	out := []Token{}
	for rows.Next() {
		t, err := scanToken(rows.Scan)
		if err != nil {
			return nil, 0, fmt.Errorf("scan api token: %w", err)
		}
		out = append(out, t)
	}
	return out, total, rows.Err()
}

// GetForUserSystem returns one of the user's own un-revoked tokens, or
// (nil, nil) when the id names no such row — unknown, someone else's, or
// already revoked. Those collapse to one answer for the same reason the list
// hides revoked rows: a token the caller cannot act on is a token that, from
// where they stand, is not there.
//
// It exists because a revoke has two refusals to tell apart and RevokeSystem,
// which does its check inside the UPDATE's predicate, can only report that
// nothing matched. A token-authenticated caller revoking its own token in
// another org must hear 403 rather than 404 — the row IS theirs, it is the
// credential that may not reach it — and only a read that resolves the row's
// org before the write can say so.
func (s *Store) GetForUserSystem(ctx context.Context, userID, tokenID string) (*Token, error) {
	if !isValidUUID(userID) || !isValidUUID(tokenID) {
		return nil, nil
	}
	tok, err := scanToken(s.db.QueryRowContext(ctx, `
		SELECT `+tokenColumns+`
		  FROM `+tokenFrom+`
		 WHERE t.id = $1 AND t.user_id = $2 AND t.revoked_at IS NULL
	`, tokenID, userID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get api token: %w", err)
	}
	return &tok, nil
}

// RenameSystem changes the display name of one of the user's own live tokens
// and returns the row as it now stands. The name is the only editable field on
// a token by design — its org, expiry and allowlist are the credential, and a
// different credential is a new token — so this is the whole of "update".
//
// Not audited: the access-change log records credential events, and a label
// changing is not one. userID is in the predicate, as in RevokeSystem, so the
// statement itself refuses another user's token; a miss is ErrNoSuchToken for
// every reason the caller may not act on it. The row rides the UPDATE's own
// RETURNING through the same projection every read uses, joined to the org's
// cap so the effective expiry is the one a list would show.
func (s *Store) RenameSystem(ctx context.Context, userID, tokenID, name string) (Token, error) {
	if !isValidUUID(tokenID) || !isValidUUID(userID) {
		return Token{}, ErrNoSuchToken
	}
	tok, err := scanToken(s.db.QueryRowContext(ctx, `
		WITH upd AS (
			UPDATE public.user_api_tokens
			   SET name = $3
			 WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL
			RETURNING *
		)
		SELECT `+tokenColumns+`
		  FROM upd t
		  LEFT JOIN public.org_settings os ON os.org_id = t.org_id
	`, tokenID, userID, name).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, ErrNoSuchToken
	}
	if err != nil {
		return Token{}, fmt.Errorf("rename api token: %w", err)
	}
	return tok, nil
}

// RevokeSystem kills one of the user's own tokens. userID is part of the
// predicate rather than a check before it: one statement, and no way for a
// caller to revoke a token it does not own even by racing.
//
// A token id that names nothing, belongs to someone else, or is already revoked
// is ErrNoSuchToken — one answer for every reason the caller may not act on it.
// The audit row rides the same transaction as the revocation.
func (s *Store) RevokeSystem(ctx context.Context, userID, tokenID, actorForAudit string) error {
	if !isValidUUID(tokenID) || !isValidUUID(userID) {
		return ErrNoSuchToken
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin revoke tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit succeeds

	var orgID, name, prefix string
	err = tx.QueryRowContext(ctx, `
		UPDATE public.user_api_tokens
		   SET revoked_at = now()
		 WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL
		RETURNING org_id::text, name, token_prefix
	`, tokenID, userID).Scan(&orgID, &name, &prefix)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNoSuchToken
	}
	if err != nil {
		return fmt.Errorf("revoke api token: %w", err)
	}

	if err := recordAccessChange(ctx, tx, orgID, domain.AccessChange{
		ActorUserID: actorForAudit,
		Action:      domain.AccessActionAPITokenRevoked,
		DetailJSON:  domain.AccessDetailAPITokenRevoked(tokenID, name, prefix, ""),
	}); err != nil {
		return fmt.Errorf("audit token revocation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit revoke: %w", err)
	}
	return nil
}

// RevokeForUserInOrgSystem is the deprovisioning revoke: it kills every token
// the user holds IN ONE ORG, which is what removing them from that org has to
// do to their headless credentials. Returns how many were newly revoked.
//
// Org-scoped on purpose, the same shape as the session revoke it belongs
// beside — a token the same user holds in a different org names an access
// relationship such a removal did not touch.
//
// One audit row per token, each naming the token owner as its target and
// carrying the membership_removed source: the actor here is the admin, so
// without the target the log would say an admin revoked a token and never whose.
func (s *Store) RevokeForUserInOrgSystem(ctx context.Context, userID, orgID, actorUserID string) (int, error) {
	if !isValidUUID(userID) || !isValidUUID(orgID) {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin revoke-all tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit succeeds

	rows, err := tx.QueryContext(ctx, `
		UPDATE public.user_api_tokens
		   SET revoked_at = now()
		 WHERE user_id = $1 AND org_id = $2 AND revoked_at IS NULL
		RETURNING id::text, name, token_prefix
	`, userID, orgID)
	if err != nil {
		return 0, fmt.Errorf("revoke api tokens for user %s in org %s: %w", userID, orgID, err)
	}
	type revoked struct{ id, name, prefix string }
	var killed []revoked
	for rows.Next() {
		var r revoked
		if err := rows.Scan(&r.id, &r.name, &r.prefix); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan revoked token: %w", err)
		}
		killed = append(killed, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("revoke api tokens: %w", err)
	}
	rows.Close()

	for _, r := range killed {
		if err := recordAccessChange(ctx, tx, orgID, domain.AccessChange{
			ActorUserID:  actorUserID,
			Action:       domain.AccessActionAPITokenRevoked,
			TargetUserID: userID,
			DetailJSON: domain.AccessDetailAPITokenRevoked(
				r.id, r.name, r.prefix, domain.AccessSourceMembershipRemoved),
		}); err != nil {
			return 0, fmt.Errorf("audit token revocation: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit revoke-all: %w", err)
	}
	return len(killed), nil
}

// TouchLastUsedSystem stamps last_used_at. Best-effort by contract, so it is
// safe to fire off the request path: a token that authenticated is no less
// valid because the bookkeeping write lost a race.
func (s *Store) TouchLastUsedSystem(ctx context.Context, tokenID string) error {
	if !isValidUUID(tokenID) {
		return nil
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE public.user_api_tokens SET last_used_at = now() WHERE id = $1`, tokenID,
	); err != nil {
		return fmt.Errorf("touch last_used_at: %w", err)
	}
	return nil
}

// RunReaper drives ReapExpiredSystem on a ticker until ctx is cancelled,
// spawned from the auth-wiring path beside the session reaper so its lifetime
// matches the multi-mode surface it serves. Errors are logged and the loop
// continues; a transient DB blip should not permanently strand dead rows.
func (s *Store) RunReaper(ctx context.Context, interval, retention time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := s.ReapExpiredSystem(ctx, retention)
			if err != nil && ctx.Err() == nil {
				tokensLog.Warn("reaper failed", "error", err)
				continue
			}
			if n > 0 {
				tokensLog.Info("reaper deleted rows", "count", n)
			}
		}
	}
}

// ReapExpiredSystem hard-deletes tokens that have been dead longer than
// retention — revoked that long ago, or that long past their effective expiry.
// Returns the number of rows deleted.
//
// Effective expiry means the same thing here as everywhere else, so a row can
// be reaped for having outlived a cap that was tightened after it was minted;
// loosening the cap afterwards does not bring back a row this deleted. That
// follows from the cap applying at use: such a token had already been refusing
// requests for the whole retention window.
func (s *Store) ReapExpiredSystem(ctx context.Context, retention time.Duration) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM public.user_api_tokens
		 WHERE id IN (
		     SELECT t.id
		       FROM `+tokenFrom+`
		      WHERE (t.revoked_at IS NOT NULL AND t.revoked_at < now() - $1::interval)
		         OR (t.revoked_at IS NULL
		             AND LEAST(t.expires_at,
		                       t.created_at + make_interval(days => os.api_token_max_age_days))
		             < now() - $1::interval)
		 )
	`, retention.String())
	if err != nil {
		return 0, fmt.Errorf("reap api tokens: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// execer is the write seam recordAccessChange composes into — a *sql.Tx here,
// always, since the whole point is that the audit row shares the caller's
// transaction.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// recordAccessChange writes one access_change_log row through the caller's
// transaction, so it commits or rolls back with the token write it describes —
// the invite-accept precedent, and the reason a token cannot exist without a
// log line naming it.
//
// The raw INSERT (rather than db.AccessChangeLogStore) is what makes that
// possible from here: the store binds to the app pool and cannot compose into
// this admin-pool transaction, and reaching for the server package's helper
// would be an import cycle. The column list is the same one.
func recordAccessChange(ctx context.Context, ex execer, orgID string, e domain.AccessChange) error {
	_, err := ex.ExecContext(ctx, `
		INSERT INTO public.access_change_log
			(org_id, actor_user_id, action, target_user_id, team_id, detail_json)
		VALUES ($1, NULLIF($2, '')::uuid, $3, NULLIF($4, '')::uuid, NULLIF($5, '')::uuid, NULLIF($6, ''))
	`, orgID, e.ActorUserID, e.Action, e.TargetUserID, e.TeamID, e.DetailJSON)
	return err
}

// isValidUUID short-circuits a caller-supplied id that Postgres would reject at
// the type layer, before any row filter runs — a 22P02 parse error surfacing as
// a 500 where the caller only asked whether a row exists. Reads degrade to an
// empty answer and revokes to ErrNoSuchToken, matching the convention the
// Postgres stores use. Mint is deliberately not covered: an id that reached it
// came from a resolved session, so a bad one is a bug that should fail loudly.
func isValidUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}
