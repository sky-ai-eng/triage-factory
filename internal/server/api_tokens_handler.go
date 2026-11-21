package server

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/auth/apitokens"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// --------------------------------------------------------------------
// /api/me/tokens — the caller's own API tokens: mint, list, revoke.
//
// Under /api/me because the subject is the caller. A token acts AS the
// principal who minted it, so nobody may address another's — there is no id to
// put in the path, and no org segment either: the org a token is sealed to is a
// field of the token, chosen per row, not the scope the route is addressed at.
//
// Multi-mode only, like every session-auth route: local mode's synthetic
// identity is already headless, so there is nothing there for a token to be and
// these answer 404.
//
// Both credentials reach this surface, which is what makes headless rotation
// work — a token mints its replacement, deploys it, then revokes itself. What a
// token cannot do is leave its own org: the same-org rules below are the
// difference between rotating a credential and escalating one.
// --------------------------------------------------------------------

// apiTokenJSON is one token as every read returns it. The secret is absent by
// construction rather than by omission at each call site: the plaintext exists
// only as MintSystem's return value, and only apiTokenCreateResponse carries a
// field that could hold it.
type apiTokenJSON struct {
	ID    string `json:"id" doc:"The token's id."`
	Name  string `json:"name" doc:"The token's display name."`
	OrgID string `json:"org_id" doc:"The id of the organization the token acts in, fixed when it was created."`
	// TokenPrefix is the first eleven characters of the plaintext — enough for
	// its owner to tell two of their own tokens apart, far short of a search.
	TokenPrefix string     `json:"token_prefix" doc:"The first eleven characters of the token, which tell two tokens apart."`
	CreatedAt   time.Time  `json:"created_at" doc:"When the token was created, as an RFC 3339 timestamp."`
	LastUsedAt  *time.Time `json:"last_used_at" doc:"When the token last authenticated a request, as an RFC 3339 timestamp, or null when it never has."`
	// ExpiresAt is what the minter asked for; EffectiveExpiresAt is when the
	// token actually stops working, folding in the org's current max-age cap.
	// Both are carried because they answer different questions, and a cap
	// tightened after the mint makes them disagree.
	ExpiresAt          *time.Time `json:"expires_at" doc:"The expiry asked for when the token was created, as an RFC 3339 timestamp, or null when none was."`
	EffectiveExpiresAt *time.Time `json:"effective_expires_at" doc:"When the token stops working, as an RFC 3339 timestamp: the earlier of expires_at and the organization's current cap on token age, or null when neither applies."`
	// AllowedCIDRs is never null — [] is the answer "no restriction", so a
	// client iterates the field without a nil check, same contract as items.
	AllowedCIDRs []string `json:"allowed_cidrs" doc:"The address ranges, in CIDR notation, that requests with the token must come from; empty when it is not restricted."`
}

// apiTokenCreateResponse is the mint answer: the row, plus the plaintext that
// will never be returned again by anything.
type apiTokenCreateResponse struct {
	apiTokenJSON
	Token string `json:"token" doc:"The token itself, sent as a Bearer credential. It is returned only here, never by a later read."`
}

func apiTokenToJSON(t apitokens.Token) apiTokenJSON {
	cidrs := t.AllowedCIDRs
	if cidrs == nil {
		cidrs = []string{}
	}
	return apiTokenJSON{
		ID:                 t.ID,
		Name:               t.Name,
		OrgID:              t.OrgID,
		TokenPrefix:        t.Prefix,
		CreatedAt:          t.CreatedAt,
		LastUsedAt:         t.LastUsedAt,
		ExpiresAt:          t.ExpiresAt,
		EffectiveExpiresAt: t.EffectiveExpiresAt,
		AllowedCIDRs:       cidrs,
	}
}

// meTokens resolves the shared preamble of all three routes: the store, and the
// principal whose tokens they are. It answers 404 in local mode (the route does
// not exist in this deployment mode) and 401 when no real principal resolved.
func (s *Server) meTokens(w http.ResponseWriter, r *http.Request) (*apitokens.Store, string, bool) {
	if s.authDeps == nil {
		notFound(w, "route")
		return nil, "", false
	}
	if s.authDeps.apiTokens == nil {
		// A wiring gap, not a caller fault — SetAuthDeps always provides the
		// store, so reaching here means the server was assembled wrong.
		internalError(w, "me/tokens", errors.New("api token store not wired"))
		return nil, "", false
	}
	claims := ClaimsFrom(r.Context())
	if claims == nil || claims.Subject == runmode.LocalDefaultUserID {
		// The sentinel subject is local mode's shim identity; it cannot own a
		// token, and authDeps being non-nil above already means multi.
		writeUnauth(w)
		return nil, "", false
	}
	return s.authDeps.apiTokens, claims.Subject, true
}

// --------------------------------------------------------------------
// create
// --------------------------------------------------------------------

// apiTokenCreateRequest is the body of POST /api/me/tokens.
//
// expires_at is an absolute RFC3339 instant and there is deliberately no
// expires_in_days beside it: a duration and a deadline are two spellings of one
// field, and the one that survives a client clock being wrong is the deadline.
// Day-style presets are display math the form does before it calls.
type apiTokenCreateRequest struct {
	Name         string   `json:"name" doc:"The token's display name, required, at most 100 bytes."`
	OrgID        string   `json:"org_id" doc:"The id of the organization the token acts in, which must be one the caller belongs to; a request made with an API token may name only that token's organization."`
	ExpiresAt    string   `json:"expires_at" doc:"When the token stops working, as an RFC 3339 timestamp in the future and within the organization's cap on token age; absent or empty creates a token with no expiry of its own."`
	AllowedCIDRs []string `json:"allowed_cidrs" doc:"Up to 20 address ranges in CIDR notation, a bare address meaning that one host, that requests with the token must come from; absent or empty leaves it unrestricted. A range with bits set below its prefix length is refused."`
}

var opAPITokenCreate = httpx.NewJSONOp[apiTokenCreateRequest, apiTokenCreateResponse](httpx.OpSpec{
	Pattern:     "POST /api/me/tokens",
	ID:          "create_api_token",
	Summary:     "Create an API token",
	Description: "Creates an API token that acts as the caller in one organization they belong to, and returns it with its secret, which is shown only in this response. A request made with an API token may create one only in that token's own organization. Multi mode only.",
	Status:      http.StatusCreated,
	BodyLimit:   4 << 10,
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintNo, Idempotent: httpx.HintNo, OpenWorld: httpx.HintNo},
})

// handleAPITokenCreate mints one token for the caller and returns the plaintext
// exactly once.
//
// POST /api/me/tokens
//
// org_id is a body field rather than a path segment because it is not the
// route's subject — the caller is — and it names which of the caller's orgs the
// new credential is sealed to. Its two refusals differ, and the difference is
// the disclosure rule: an org the caller does not belong to is 404 (it is not in
// their list either), while an org they can see but their CREDENTIAL cannot act
// in is 403 — a leaked token for org X must not be able to mint one for org Y,
// but pretending Y does not exist would be a lie the caller can disprove.
func (s *Server) handleAPITokenCreate(w http.ResponseWriter, r *http.Request) {
	store, userID, ok := s.meTokens(w, r)
	if !ok {
		return
	}

	// 4 KiB: a name, an org id, a timestamp and up to twenty CIDR strings.
	req, ok := opAPITokenCreate.Decode(w, r)
	if !ok {
		return
	}

	// Every field fault in one response — a caller fixing a bad expiry should
	// not have to make a second request to discover the bad CIDR beside it.
	var v httpx.Validation

	name := strings.TrimSpace(req.Name)
	switch {
	case name == "":
		v.Invalid("name", "name is required")
	case len(name) > apitokens.MaxNameLen:
		v.Invalid("name", fmt.Sprintf("name must be at most %d characters", apitokens.MaxNameLen))
	}

	orgID := ""
	if strings.TrimSpace(req.OrgID) == "" {
		v.Invalid("org_id", "org_id is required")
	} else if u, err := uuid.Parse(req.OrgID); err != nil {
		v.Invalid("org_id", "org_id must be a valid org id")
	} else {
		orgID = u.String()
	}

	var expiresAt *time.Time
	if req.ExpiresAt != "" {
		ts, err := time.Parse(time.RFC3339, req.ExpiresAt)
		switch {
		case err != nil:
			v.Invalid("expires_at", "expires_at must be an RFC3339 timestamp")
		case !ts.After(time.Now()):
			v.Invalid("expires_at", "expires_at must be in the future")
		default:
			ts = ts.UTC()
			expiresAt = &ts
		}
	}

	cidrs := s.validateTokenCIDRs(&v, req.AllowedCIDRs)

	// 422, not 400: the body parsed and every field is the type it claims to
	// be — what failed is what the values mean.
	if v.Flush(w, http.StatusUnprocessableEntity) {
		return
	}

	member, err := s.az.UserHasOrgAccess(r.Context(), userID, orgID)
	if err != nil {
		internalError(w, "me/tokens", fmt.Errorf("org membership %s/%s: %w", userID, orgID, err))
		return
	}
	if !member {
		notFound(w, "org")
		return
	}
	// Self-mint is same-org only. Checked after membership so an org the caller
	// is not in stays undisclosed whichever credential asked.
	if tok := httpx.TokenAuthFrom(r.Context()); tok != nil && tok.OrgID != orgID {
		forbidden(w, "an API token may only mint tokens in its own org; use a session, or a token issued for that org")
		return
	}
	// A credential barred from the org is refused here rather than handed a
	// token it could not use: the new token inherits this request's sign-in,
	// so it would be barred from the org on its first call.
	if s.refuseSSOBarred(w, r, orgID) {
		return
	}

	if !s.checkTokenAgeCap(w, r, orgID, expiresAt) {
		return
	}

	// The actor is the caller: a self-mint records the principal who asked,
	// which for this route is always the token's own owner. The new token is
	// signed in however the minting credential was — a session's provider, or
	// the minting token's own inherited one — so a token is never a way to
	// reach an org that requires SSO without having come through it.
	tokenRow, plaintext, err := store.MintSystem(
		r.Context(), userID, orgID, name, expiresAt, cidrs, httpx.SignInProviderFrom(r.Context()), userID)
	switch {
	case errors.Is(err, apitokens.ErrTokenLimit):
		conflict(w, err.Error())
		return
	case err != nil:
		internalError(w, "me/tokens", fmt.Errorf("mint api token for %s in %s: %w", userID, orgID, err))
		return
	}

	opAPITokenCreate.Write(w, apiTokenCreateResponse{
		apiTokenJSON: apiTokenToJSON(tokenRow),
		Token:        plaintext,
	})
}

// validateTokenCIDRs canonicalizes an allowlist, appending one fault per bad
// entry rather than stopping at the first, and returns the canonical list.
//
// The canonicalizer is the store's own, so "a valid range" has one definition:
// a bare address is a host, and a prefix with bits below its mask is REFUSED
// rather than masked, because silently widening 10.0.0.1/8 into 10.0.0.0/8 is
// how an allowlist ends up admitting addresses nobody meant.
//
// An absent or empty list means no restriction, which is what the column's NULL
// means; it is not a token that admits nobody.
func (s *Server) validateTokenCIDRs(v *httpx.Validation, in []string) []string {
	if len(in) == 0 {
		return nil
	}
	if len(in) > apitokens.MaxAllowedCIDRs {
		v.Invalid("allowed_cidrs", fmt.Sprintf(
			"allowed_cidrs may hold at most %d ranges, got %d", apitokens.MaxAllowedCIDRs, len(in)))
		return nil
	}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		c, err := apitokens.CanonicalCIDR(raw)
		if err != nil {
			v.Invalid("allowed_cidrs", err.Error())
			continue
		}
		// Compared after canonicalization, so two spellings of one range are
		// caught as the duplicate they are.
		if slices.Contains(out, c) {
			v.Invalid("allowed_cidrs", fmt.Sprintf("duplicate range %q", raw))
			continue
		}
		out = append(out, c)
	}
	return out
}

// checkTokenAgeCap refuses an explicit expiry that outlives the org's
// api_token_max_age_days, naming the cap so the caller can pick a date that
// fits. Rejected, never clamped: a token that silently expires earlier than
// asked is a wrong answer that looks like a right one, and the caller is the
// one who has to schedule the rotation.
//
// An omitted expiry is not a fault — the cap still binds it, computed at use
// from created_at, which is what effective_expires_at reports.
//
// The settings read is the admin-pool variant with the org bound by argument:
// the caller's membership in it was just established, and this runs outside any
// claims-carrying transaction.
func (s *Server) checkTokenAgeCap(w http.ResponseWriter, r *http.Request, orgID string, expiresAt *time.Time) bool {
	if expiresAt == nil {
		return true
	}
	set, err := s.orgs.GetSettingsSystem(r.Context(), orgID)
	if err != nil {
		internalError(w, "me/tokens", fmt.Errorf("org settings %s: %w", orgID, err))
		return false
	}
	if set.APITokenMaxAgeDays <= 0 {
		return true
	}
	limit := time.Now().AddDate(0, 0, set.APITokenMaxAgeDays)
	if expiresAt.After(limit) {
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidField,
			Field:  "expires_at",
			Message: fmt.Sprintf(
				"this org caps API tokens at %d days, so expires_at must be no later than %s",
				set.APITokenMaxAgeDays, limit.UTC().Format(time.RFC3339)),
		})
		return false
	}
	return true
}

// --------------------------------------------------------------------
// policy
// --------------------------------------------------------------------

// apiTokenPolicyJSON is the org's API-token policy as any member reads it.
// MaxAgeDays is null when the org sets no cap — null rather than 0, because
// "no cap" is an answer and 0 is not a number of days a token may live.
type apiTokenPolicyJSON struct {
	MaxAgeDays *int `json:"max_age_days" doc:"The most days an API token in the organization may live, counted from its creation, or null when there is no cap."`
}

var opAPITokenPolicy = httpx.NewJSONOp[httpx.NoBody, apiTokenPolicyJSON](httpx.OpSpec{
	Pattern:     "GET /api/orgs/{org_id}/api-token-policy",
	ID:          "get_api_token_policy",
	Summary:     "Get an organization's API token policy",
	Description: "Returns the organization's cap on how long its API tokens may live, which binds every member's tokens. Multi mode only.",
	PathParams:  map[string]string{"org_id": "The organization's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleAPITokenPolicy answers the org's token policy to any of its members.
//
// GET /api/orgs/{org_id}/api-token-policy
//
// The cap is written through the org-settings surface, which only an admin
// may read — but the cap binds every member's tokens, and a member choosing
// an expiry needs to know it before the 422 tells them. It is its own node
// rather than a field on /api/me because it is a fact about the ORG, not the
// viewer: the org segment is what the answer depends on, so a member of three
// orgs reads all three here without touching a session cursor, and a token
// caller (sealed to one org) reads its own. Read-only, resource-pure, and
// multi-only like the token surface it describes — in local mode there is no
// token for a cap to bind, so the route is not there either.
func (s *Server) handleAPITokenPolicy(w http.ResponseWriter, r *http.Request) {
	if s.authDeps == nil {
		notFound(w, "route")
		return
	}
	orgID, userID, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	var maxAge *int
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		set, err := tx.Orgs.GetSettings(r.Context(), orgID)
		if err != nil {
			return err
		}
		if set.APITokenMaxAgeDays > 0 {
			v := set.APITokenMaxAgeDays
			maxAge = &v
		}
		return nil
	}); err != nil {
		internalError(w, "api-token-policy", fmt.Errorf("org settings %s: %w", orgID, err))
		return
	}
	opAPITokenPolicy.Write(w, apiTokenPolicyJSON{MaxAgeDays: maxAge})
}

// --------------------------------------------------------------------
// list
// --------------------------------------------------------------------

// apiTokenListRequest is the body of POST /api/me/tokens/list. A POST because
// the filter is a body, like every other list read; the read itself has no
// side effects.
type apiTokenListRequest struct {
	// OrgID narrows to one org. Empty means every org the caller holds tokens
	// in — except under a token, whose own org is the only set it may see.
	OrgID string `json:"org_id" doc:"Keeps only the tokens for this organization. Absent or empty keeps the tokens for every organization, except in a request made with an API token, which reads only its own organization's tokens."`
	// SortKey/SortDir order the list (apiTokenListSortKeys); absent is newest
	// first. A token never used sorts last under last_used_at in both
	// directions.
	SortKey string `json:"sort_key" doc:"What to order by: last_used_at or created_at, newest first by default. A token never used sorts last under last_used_at in either direction. Absent orders by creation, newest first."`
	SortDir string `json:"sort_dir" doc:"The order's direction, asc or desc; it needs sort_key."`

	httpx.PageRequest
}

var opAPITokenList = httpx.NewListOp[apiTokenListRequest, apiTokenJSON](httpx.OpSpec{
	Pattern:     "POST /api/me/tokens/list",
	ID:          "list_api_tokens",
	Summary:     "List the caller's API tokens",
	Description: "Returns one page of the caller's API tokens, expired ones included and revoked ones left out, without their secrets. A request made with an API token reads only its own organization's tokens. Multi mode only.",
	BodyLimit:   4 << 10,
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// apiTokenListSortKeys is the list's sort_key vocabulary. Each key's own
// direction is most recent first, which is what a reader ranking tokens by
// either time asks for first.
var apiTokenListSortKeys = []httpx.SortKey{
	{Name: apitokens.SortLastUsedAt, Default: db.SortDesc},
	{Name: apitokens.SortCreatedAt, Default: db.SortDesc},
}

// apiTokenListFilterKey is the canonicalized filter set the page token is
// fingerprinted against, so page 2 of one query cannot be requested with page
// 1's token of another. It carries the EFFECTIVE org — the one the store is
// asked for — so a token caller's tokens stay valid across the default.
type apiTokenListFilterKey struct {
	OrgID   string `json:"org_id"`
	SortKey string `json:"sort_key"`
	SortDir string `json:"sort_dir"`
}

// handleAPITokenList answers the caller's own tokens, newest first unless the
// body names a sort_key.
//
// POST /api/me/tokens/list
//
// Revoked tokens are absent, immediately: a revoked credential is not a thing
// anyone can act on, and where it went is the access-change log's business, not
// this list's. Expired ones stay, because they still hold a row their owner may
// want to clean up.
func (s *Server) handleAPITokenList(w http.ResponseWriter, r *http.Request) {
	store, userID, ok := s.meTokens(w, r)
	if !ok {
		return
	}

	req, ok := opAPITokenList.Decode(w, r)
	if !ok {
		return
	}

	var v httpx.Validation
	orgFilter := ""
	if raw := strings.TrimSpace(req.OrgID); raw != "" {
		if u, err := uuid.Parse(raw); err != nil {
			// Rejected rather than dropped: a filter that fails to parse must
			// never fall back to the unfiltered set, which would answer a
			// wider question than the caller asked.
			v.Invalid("org_id", "org_id must be a valid org id")
		} else {
			orgFilter = u.String()
		}
	}
	if tok := httpx.TokenAuthFrom(r.Context()); tok != nil {
		switch {
		case orgFilter == "":
			// A token sees its own org and no other, so the unfiltered read is
			// its org's read. Defaulting rather than refusing keeps `{}` — the
			// obvious "list my tokens" call — working under both credentials.
			orgFilter = tok.OrgID
		case orgFilter != tok.OrgID:
			forbidden(w, "an API token may only list tokens in its own org")
			return
		}
	}

	order := httpx.Sort(&v, req.SortKey, req.SortDir, apiTokenListSortKeys)
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(apiTokenListFilterKey{
		OrgID: orgFilter, SortKey: order.Key, SortDir: order.Dir,
	}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	var filter *string
	if orgFilter != "" {
		filter = &orgFilter
	}
	tokens, total, err := store.ListForUserSystem(r.Context(), userID, filter, order,
		db.ListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly})
	if err != nil {
		internalError(w, "me/tokens", fmt.Errorf("list api tokens for %s: %w", userID, err))
		return
	}
	items := make([]apiTokenJSON, len(tokens))
	for i, t := range tokens {
		items[i] = apiTokenToJSON(t)
	}
	opAPITokenList.Write(w, page, items, total)
}

// --------------------------------------------------------------------
// rename
// --------------------------------------------------------------------

// apiTokenRenameRequest is the body of PATCH /api/me/tokens/{id}. Name is the
// only field because it is the only thing on a token that is not the
// credential: a different org, expiry or allowlist is a different token, and
// rotation (mint, move, revoke) is how one gets it.
type apiTokenRenameRequest struct {
	Name httpx.Patch[string] `json:"name" doc:"The token's new display name, at most 100 bytes. A token always has a name, so null is refused."`
}

var opAPITokenRename = httpx.NewJSONOp[apiTokenRenameRequest, apiTokenJSON](httpx.OpSpec{
	Pattern:     "PATCH /api/me/tokens/{id}",
	ID:          "update_api_token",
	Summary:     "Rename an API token",
	Description: "Sets one of the caller's API tokens' display name and returns the token. A request made with an API token may rename only tokens in its own organization. Multi mode only.",
	PathParams:  map[string]string{"id": "The API token's id."},
	BodyLimit:   4 << 10,
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleAPITokenRename changes a token's display name in place.
//
// PATCH /api/me/tokens/{id}
//
// PATCH on the resource rather than a verb: a name is a column, and this is
// the column flip. The one field is required-when-present and cannot be
// cleared — a token with no name is a token its owner cannot tell from the
// next — so a body naming nothing is refused instead of answered as a write.
// The 404/403 split is revoke's: another user's token is invisible, the
// caller's own token in an org their credential may not reach is not.
func (s *Server) handleAPITokenRename(w http.ResponseWriter, r *http.Request) {
	store, userID, ok := s.meTokens(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidID, Message: "id must be a valid API token id", Field: "id",
		})
		return
	}

	req, ok := opAPITokenRename.Decode(w, r)
	if !ok {
		return
	}
	// A type fault is a malformed body, 400 like any other; what the value
	// means is checked after it, at 422.
	var types httpx.Validation
	raw, state := req.Name.Read(&types, "name")
	if state == httpx.PatchAbsent {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonMissingField, Message: "no fields to update: provide name", Field: "name",
		})
		return
	}
	if types.Flush(w, http.StatusBadRequest) {
		return
	}
	var v httpx.Validation
	name := strings.TrimSpace(raw)
	switch {
	case state == httpx.PatchClear:
		v.Invalid("name", "name cannot be cleared: a token always has a name")
	case name == "":
		v.Invalid("name", "name is required")
	case len(name) > apitokens.MaxNameLen:
		v.Invalid("name", fmt.Sprintf("name must be at most %d characters", apitokens.MaxNameLen))
	}
	if v.Flush(w, http.StatusUnprocessableEntity) {
		return
	}

	row, err := store.GetForUserSystem(r.Context(), userID, id)
	if err != nil {
		internalError(w, "me/tokens", fmt.Errorf("get api token %s: %w", id, err))
		return
	}
	if row == nil {
		notFound(w, "api token")
		return
	}
	if tok := httpx.TokenAuthFrom(r.Context()); tok != nil && tok.OrgID != row.OrgID {
		forbidden(w, "an API token may only rename tokens in its own org")
		return
	}

	renamed, err := store.RenameSystem(r.Context(), userID, id, name)
	switch {
	case errors.Is(err, apitokens.ErrNoSuchToken):
		// Revoked between the read and the write — the answer the read would
		// have given a moment later.
		notFound(w, "api token")
		return
	case err != nil:
		internalError(w, "me/tokens", fmt.Errorf("rename api token %s: %w", id, err))
		return
	}
	opAPITokenRename.Write(w, apiTokenToJSON(renamed))
}

// --------------------------------------------------------------------
// revoke
// --------------------------------------------------------------------

var opAPITokenRevoke = httpx.NewNoContentOp[httpx.NoBody](httpx.OpSpec{
	Pattern:     "DELETE /api/me/tokens/{id}",
	ID:          "delete_api_token",
	Summary:     "Revoke an API token",
	Description: "Revokes one of the caller's API tokens so it stops working at once, including the token that makes the request. A request made with an API token may revoke only tokens in its own organization. Multi mode only.",
	PathParams:  map[string]string{"id": "The API token's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleAPITokenRevoke kills one of the caller's tokens.
//
// DELETE /api/me/tokens/{id}
//
// A verb route would be the wrong shape here: revocation is the end of the
// resource, not a field on it, and the row stops being listable at once.
//
// Revoking the token that authenticated the request is allowed and is not a
// special case — it is how a rotation ends. The response still goes out; the
// next request with that token is a 401.
func (s *Server) handleAPITokenRevoke(w http.ResponseWriter, r *http.Request) {
	store, userID, ok := s.meTokens(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidID, Message: "id must be a valid API token id", Field: "id",
		})
		return
	}

	// Resolved before the write so the two refusals can be told apart: another
	// user's token is invisible (404, never 403 — its existence is not the
	// caller's business), while the caller's OWN token in another org is a row
	// they can see and their credential may not touch (403).
	row, err := store.GetForUserSystem(r.Context(), userID, id)
	if err != nil {
		internalError(w, "me/tokens", fmt.Errorf("get api token %s: %w", id, err))
		return
	}
	if row == nil {
		notFound(w, "api token")
		return
	}
	if tok := httpx.TokenAuthFrom(r.Context()); tok != nil && tok.OrgID != row.OrgID {
		forbidden(w, "an API token may only revoke tokens in its own org")
		return
	}

	switch err := store.RevokeSystem(r.Context(), userID, id, userID); {
	case errors.Is(err, apitokens.ErrNoSuchToken):
		// Revoked between the read above and here — the same answer the read
		// would have given a moment later.
		notFound(w, "api token")
		return
	case err != nil:
		internalError(w, "me/tokens", fmt.Errorf("revoke api token %s: %w", id, err))
		return
	}
	opAPITokenRevoke.Write(w)
}

func (s *Server) registerAPITokenRoutes() {
	// The caller's own API tokens — the headless credential surface.
	// Viewer-relative like the caller's settings row (/api/me/settings): a
	// token acts AS its minter, so nobody may address another's and there is no
	// id in the path. The org a token is sealed to is a field on the row, not
	// the route's scope, which is why create takes org_id in its body.
	// Multi-mode only (404 in local). Both credentials reach these: a session
	// mints the first token, and a token rotates itself within its own org.
	s.op(opAPITokenCreate, s.handleAPITokenCreate)
	s.op(opAPITokenList, s.handleAPITokenList)
	s.op(opAPITokenRename, s.handleAPITokenRename)
	s.op(opAPITokenRevoke, s.handleAPITokenRevoke)
	// The policy those tokens live under is the org's, so it is addressed at
	// the org and readable by any member — the cap binds everyone's tokens,
	// and a member picking an expiry should learn it before the 422 does.
	s.op(opAPITokenPolicy, s.handleAPITokenPolicy)
}
