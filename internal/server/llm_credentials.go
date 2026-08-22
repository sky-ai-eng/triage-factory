package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/llmcred"
	"github.com/sky-ai-eng/triage-factory/internal/modelcatalog"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// The org's LLM provider credentials, one addressable resource per credential
// SHAPE. Two rules produced this layout, and both were violations before it:
//
//   - A blank secret never means anything. Rotation is a PUT carrying the new
//     value; removal is a DELETE. The Anthropic route used to read a blank key
//     as "clear this key AND every Bedrock secret", while the Bedrock route read
//     a blank secret as the opposite ("keep what's stored") — so the same empty
//     string was destructive on one route and inert on the other, and neither
//     credential had a DELETE at all.
//   - A flavor is a route, not a field. Bedrock's three authentication shapes
//     (an assumed IAM role, a Bedrock API key, an IAM access-key pair) need
//     different required fields, different validation, and in one case a live
//     probe; as one route behind an auth_method discriminator, nine
//     flavor-conditional fields shared one body and the other flavors' fields
//     were silently dropped. As three routes, strict decoding makes sending the
//     wrong flavor's field a 400 that names it.
//
// An org holds BOTH providers at once. Nothing here clears the other one,
// because there is no longer a losing provider to clean up after: a run resolves
// whichever provider serves its own model, so an Anthropic key and Bedrock
// material are both live configuration. What a bind still replaces is material
// of its OWN provider that it supersedes — a different Bedrock shape — and it
// enumerates that in its `cleared` list.

// llmCredentialResponse is the shape every bind on this surface answers with.
// Cleared names the stored material this write removed — a different Bedrock
// flavor's — using the same identifiers the read surface uses ("anthropic",
// "bedrock:role"). It is always present, empty list included, so a client
// renders "we also disconnected X" without having to distinguish absent from
// empty. The other provider is never in it: binding one no longer disturbs the
// other.
type llmCredentialResponse struct {
	Status  string   `json:"status"`
	Cleared []string `json:"cleared"`
}

// The `cleared` vocabulary. "anthropic" is the org's Anthropic API key;
// "bedrock:<method>" is Bedrock material under one of its three shapes, sharing
// the method names the settings read reports in bedrock_auth_method.
const llmCredentialAnthropic = "anthropic"

func llmCredentialBedrock(method string) string { return "bedrock:" + method }

// errLLMAuthMethodHasCredentials is a settings write moving the org to
// domain.LLMAuthSystem while it still holds provider material. Lives here
// rather than beside that handler because what makes it true is this file's
// subject — which credentials are bound — and the fix is a DELETE on one of
// this file's routes.
var errLLMAuthMethodHasCredentials = errors.New("an organization running on the host's Claude credentials can hold none of its own")

// onOwnCredentials records that the org's Claude credentials are now its own.
// Every bind stamps it, because holding provider material and running on the
// host's environment are mutually exclusive and the bind is the act that
// settles which one this org is doing — there is nothing left to ask an admin
// to confirm. The refs still say WHICH provider is bound; this says the org is
// no longer on the host's.
func onOwnCredentials(o *domain.OrgSettings) { o.LLMAuthMethod = domain.LLMAuthBYOK }

// boundLLMProviders names the providers an org holds material for, in the words
// an admin would use to go find them. Empty means the org has bound nothing,
// which is the only state domain.LLMAuthSystem is true in.
func boundLLMProviders(o domain.OrgSettings) []string {
	var bound []string
	if o.AnthropicAPIKeyRef != "" {
		bound = append(bound, modelcatalog.ProviderDisplayName(modelcatalog.ProviderAnthropic))
	}
	if o.BedrockCredentialsRef != "" {
		bound = append(bound, modelcatalog.ProviderDisplayName(modelcatalog.ProviderBedrock))
	}
	return bound
}

// storedLLMCredentials lists the LLM material an org currently holds, read off
// the settings row's two refs rather than by probing the vault: the refs are the
// record of which provider is actually configured, so a bind that enumerates
// them can never claim to have revoked something that was never set.
func storedLLMCredentials(o domain.OrgSettings) (anthropic string, bedrock string) {
	if o.AnthropicAPIKeyRef != "" {
		anthropic = llmCredentialAnthropic
	}
	if method := bedrockAuthMethodFromRef(o.BedrockCredentialsRef); method != "" {
		bedrock = llmCredentialBedrock(method)
	}
	return anthropic, bedrock
}

// --------------------------------------------------------------------
// Anthropic
// --------------------------------------------------------------------

// handleAnthropicPut validates and stores the org's Anthropic API key — the
// single validated write path for the anthropic_api_key vault secret (the org
// settings PATCH accepts none of these fields), so a key reaching the vault has
// always passed ValidateAnthropicAPIKey here.
//
// PUT /api/orgs/{org_id}/llm/anthropic
func (se *settingsHandler) handleAnthropicPut(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := se.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	var req struct {
		APIKey string `json:"api_key"`
	}
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	key := strings.TrimSpace(req.APIKey)
	if key == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonMissingField,
			Message: "api_key is required; to remove the stored key, DELETE this resource",
			Field:   "api_key",
		})
		return
	}

	// Validate before storing. Both failure modes (rejected key, unreachable)
	// are surfaced verbatim as a 422 — mirroring the Jira credential bind, which
	// also folds "bad credential" and "couldn't reach the host" into one 422.
	if err := auth.ValidateAnthropicAPIKey(r.Context(), key); err != nil {
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
			Reason: httpx.ReasonUpstreamRejected, Message: err.Error(), Field: "api_key",
		})
		return
	}

	if err := se.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		orgSet, err := tx.Orgs.GetSettings(r.Context(), orgID)
		if err != nil {
			return fmt.Errorf("load org settings: %w", err)
		}
		if err := tx.Secrets.Put(r.Context(), orgID, secretKeyAnthropicAPIKey, key, "Org's Anthropic API key"); err != nil {
			return fmt.Errorf("store Anthropic key: %w", err)
		}
		// The org's Bedrock material, if any, is untouched: both providers are
		// live at once, and each run resolves the one its model is served by.
		orgSet.AnthropicAPIKeyRef = secretKeyAnthropicAPIKey
		onOwnCredentials(&orgSet)
		if _, err := tx.Orgs.UpdateSettings(r.Context(), orgID, orgSet); err != nil {
			return err
		}
		return tx.AccessChangeLog.Record(r.Context(), orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialSet,
			DetailJSON:  accessDetailCredential(domain.CredentialKindAnthropicKey, ""),
		})
	}); err != nil {
		internalError(w, "llm-credentials", fmt.Errorf("persist anthropic credentials: %w", err))
		return
	}

	writeJSON(w, http.StatusOK, llmCredentialResponse{Status: "connected", Cleared: []string{}})
}

// handleAnthropicDelete removes the org's Anthropic API key. Idempotent — a
// delete with nothing stored is a 200 and no audit row, because a removal that
// removed nothing is not an access change.
//
// It removes the Anthropic key ALONE, and it does not move the org onto the
// host's credentials: a delete says this key is gone, not where the next run
// should get one, and an org that still holds Bedrock material plainly has an
// answer already. Folding the Bedrock sweep in here is how a route that says
// "anthropic" ended up wiping AWS material. After it, runs whose model is
// served by Bedrock keep working and runs on an Anthropic model are refused by
// name — the org disconnected the one, not both.
//
// Selecting "use the host's Claude Code credentials" is therefore a delete per
// bound credential and then the settings write that records the selection,
// which is what makes it a stored choice rather than an inference from what is
// left behind.
//
// DELETE /api/orgs/{org_id}/llm/anthropic
func (se *settingsHandler) handleAnthropicDelete(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := se.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	if err := se.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		// Settings first: its ref is the record of whether a key was actually
		// configured, which the idempotent vault delete cannot report.
		orgSet, err := tx.Orgs.GetSettings(r.Context(), orgID)
		if err != nil {
			return fmt.Errorf("load org settings: %w", err)
		}
		had := orgSet.AnthropicAPIKeyRef != ""
		if _, err := tx.Secrets.Delete(r.Context(), orgID, secretKeyAnthropicAPIKey); err != nil {
			return fmt.Errorf("clear Anthropic key: %w", err)
		}
		orgSet.AnthropicAPIKeyRef = ""
		if _, err := tx.Orgs.UpdateSettings(r.Context(), orgID, orgSet); err != nil {
			return err
		}
		if !had {
			return nil
		}
		return recordCredentialRemovals(r.Context(), tx, orgID, userID,
			[]string{domain.CredentialKindAnthropicKey})
	}); err != nil {
		internalError(w, "llm-credentials", fmt.Errorf("clear anthropic credentials: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "disconnected"})
}

// --------------------------------------------------------------------
// Bedrock — one route per authentication shape
// --------------------------------------------------------------------

// bedrockConfig is the non-secret configuration every Bedrock flavor carries.
// The region is required: it names both the endpoint and the SigV4 signing
// scope. The model id (an inference profile) and the endpoint override (for VPC
// interface endpoints / GovCloud / the China partition, whose hostnames the
// standard regional formula can't produce) are optional.
//
// These are PUTs, so the body IS the resource: an optional field the caller
// omits is unset, not kept. That is a replacement, not a blank-means-something
// sentinel — there is no arm here that reads "" as "leave the stored value".
type bedrockConfig struct {
	Region  string `json:"region"`
	ModelID string `json:"model_id"`
	BaseURL string `json:"base_url"`
}

// bedrockAccessKeysRequest is PUT …/llm/bedrock/access-keys: the IAM
// access-key pair the SigV4 re-signing proxy serves at run time, plus an
// optional session token for temporary credentials.
type bedrockAccessKeysRequest struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token"`
	bedrockConfig
}

// bedrockBearerRequest is PUT …/llm/bedrock/bearer: a Bedrock API key
// (AWS_BEARER_TOKEN_BEDROCK).
type bedrockBearerRequest struct {
	BearerToken string `json:"bearer_token"`
	bedrockConfig
}

// bedrockRoleRequest is PUT …/llm/bedrock/role: the customer IAM role the
// control service assumes to mint per-run session credentials. Plain text,
// non-secret. The External ID is TF-generated and never taken from the request
// — see handleBedrockRoleSetup, which hands the admin the trust policy that
// pins it.
type bedrockRoleRequest struct {
	RoleARN string `json:"role_arn"`
	bedrockConfig
}

// handleBedrockAccessKeysPut binds the IAM access-key pair.
//
// PUT /api/orgs/{org_id}/llm/bedrock/access-keys
func (se *settingsHandler) handleBedrockAccessKeysPut(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := se.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	var req bedrockAccessKeysRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	req.AccessKeyID = strings.TrimSpace(req.AccessKeyID)
	req.SecretAccessKey = strings.TrimSpace(req.SecretAccessKey)
	req.SessionToken = strings.TrimSpace(req.SessionToken)

	v := validateBedrockConfig(&req.bedrockConfig)
	if req.AccessKeyID == "" {
		v.Missing("access_key_id")
	}
	if req.SecretAccessKey == "" {
		v.Missing("secret_access_key")
	}
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	// There is deliberately NO live probe here, and none for the bearer flavor
	// either: an IAM key scoped to bedrock:InvokeModel only (the recommended
	// posture) fails every free control-plane call, a runtime invoke probe costs
	// real money and presumes model access, and a VPC-endpoint org's Bedrock may
	// not be reachable from the TF server at all. Validation is shape-only; a
	// bad credential surfaces on the first run with the AWS error attached.
	se.bindBedrock(w, r, orgID, userID, bedrockBinding{
		ref:    integrations.KeyAWSAccessKeyID,
		config: req.bedrockConfig,
		// The session token is part of the replaced set: absent means "no
		// session token" (long-lived keys), never "keep the old one" — a stale
		// token would 403 every call.
		clear: []string{integrations.KeyAWSBearerTokenBedrock, integrations.KeyAWSSessionToken},
		write: func(tx db.TxStores) error {
			if err := tx.Secrets.Put(r.Context(), orgID, integrations.KeyAWSAccessKeyID, req.AccessKeyID, "Org's AWS access key ID"); err != nil {
				return fmt.Errorf("store AWS access key ID: %w", err)
			}
			if err := tx.Secrets.Put(r.Context(), orgID, integrations.KeyAWSSecretAccessKey, req.SecretAccessKey, "Org's AWS secret access key"); err != nil {
				return fmt.Errorf("store AWS secret access key: %w", err)
			}
			if req.SessionToken == "" {
				return nil
			}
			if err := tx.Secrets.Put(r.Context(), orgID, integrations.KeyAWSSessionToken, req.SessionToken, "Org's AWS session token"); err != nil {
				return fmt.Errorf("store AWS session token: %w", err)
			}
			return nil
		},
	})
}

// handleBedrockBearerPut binds a Bedrock API key.
//
// PUT /api/orgs/{org_id}/llm/bedrock/bearer
func (se *settingsHandler) handleBedrockBearerPut(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := se.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	var req bedrockBearerRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	req.BearerToken = strings.TrimSpace(req.BearerToken)

	v := validateBedrockConfig(&req.bedrockConfig)
	if req.BearerToken == "" {
		v.Missing("bearer_token")
	}
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	se.bindBedrock(w, r, orgID, userID, bedrockBinding{
		ref:    integrations.KeyAWSBearerTokenBedrock,
		config: req.bedrockConfig,
		clear: []string{
			integrations.KeyAWSAccessKeyID, integrations.KeyAWSSecretAccessKey, integrations.KeyAWSSessionToken,
		},
		write: func(tx db.TxStores) error {
			if err := tx.Secrets.Put(r.Context(), orgID, integrations.KeyAWSBearerTokenBedrock, req.BearerToken, "Org's Bedrock API key"); err != nil {
				return fmt.Errorf("store Bedrock bearer token: %w", err)
			}
			return nil
		},
	})
}

// handleBedrockRolePut binds role mode (TFAC-616) — the only Bedrock shape with
// a live connect probe and the only one that stores no secret at all: the org
// holds the role ARN and a TF-generated External ID, and the brain mints STS
// session credentials per run.
//
// PUT /api/orgs/{org_id}/llm/bedrock/role
func (se *settingsHandler) handleBedrockRolePut(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := se.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	var req bedrockRoleRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	req.RoleARN = strings.TrimSpace(req.RoleARN)

	v := validateBedrockConfig(&req.bedrockConfig)
	switch {
	case req.RoleARN == "":
		v.Missing("role_arn")
	case !bedrockRoleARNRe.MatchString(req.RoleARN):
		v.Invalid("role_arn", fmt.Sprintf("%q does not look like an IAM role ARN (e.g. arn:aws:iam::123456789012:role/tf-bedrock)", req.RoleARN))
	}
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	resolver := se.bedrockRoleResolver()
	if resolver == nil {
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
			Reason:  httpx.ReasonNotConfigured,
			Message: "Bedrock role mode requires the multi-mode control service (this deployment has no ambient AWS identity to assume roles with)",
		})
		return
	}

	// Ensure the External ID exists (generate + persist if the admin skipped the
	// role-setup fetch) BEFORE the probe — the probe must present the same value
	// the customer's trust policy references. It is stable thereafter, which is
	// why the sweep below never clears it.
	externalID, err := se.resolveExternalID(r.Context(), orgID, userID)
	if err != nil {
		internalError(w, "llm-credentials", fmt.Errorf("prepare bedrock external id: %w", err))
		return
	}
	if !se.probeBedrockRole(w, resolver, r, req.RoleARN, externalID) {
		return
	}

	se.bindBedrock(w, r, orgID, userID, bedrockBinding{
		ref:    integrations.KeyAWSRoleARN,
		config: req.bedrockConfig,
		clear: []string{
			integrations.KeyAWSBearerTokenBedrock,
			integrations.KeyAWSAccessKeyID, integrations.KeyAWSSecretAccessKey, integrations.KeyAWSSessionToken,
		},
		write: func(tx db.TxStores) error {
			if err := tx.Secrets.Put(r.Context(), orgID, integrations.KeyAWSRoleARN, req.RoleARN, "Org's Bedrock IAM role ARN"); err != nil {
				return fmt.Errorf("store role ARN: %w", err)
			}
			return nil
		},
	})
}

// handleBedrockDelete removes every piece of Bedrock material the org holds —
// credential, non-secret config, role ARN and External ID alike. Idempotent.
//
// DELETE /api/orgs/{org_id}/llm/bedrock
func (se *settingsHandler) handleBedrockDelete(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := se.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	if err := se.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		// Settings first — its ref tells us whether there was anything to
		// revoke, which the idempotent vault deletes cannot.
		orgSet, err := tx.Orgs.GetSettings(r.Context(), orgID)
		if err != nil {
			return fmt.Errorf("load org settings: %w", err)
		}
		had := orgSet.BedrockCredentialsRef != ""
		if err := clearBedrockSecrets(r, tx, orgID); err != nil {
			return err
		}
		orgSet.BedrockCredentialsRef = ""
		if _, err := tx.Orgs.UpdateSettings(r.Context(), orgID, orgSet); err != nil {
			return err
		}
		if !had {
			return nil
		}
		return recordCredentialRemovals(r.Context(), tx, orgID, userID,
			[]string{domain.CredentialKindBedrock})
	}); err != nil {
		internalError(w, "llm-credentials", fmt.Errorf("clear bedrock credentials: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "disconnected"})
}

// bedrockBinding is what one flavor's PUT contributes to the shared write: the
// ref marker that records which shape is live, the non-secret config, the keys
// this flavor's material replaces, and the writes for its own material.
type bedrockBinding struct {
	ref    string
	config bedrockConfig
	clear  []string
	write  func(tx db.TxStores) error
}

// bindBedrock performs the parts every Bedrock flavor shares: the flavor's own
// writes, the non-secret config, the sweep of the Bedrock material this bind
// replaces, the settings ref, and the audit row — all in one transaction, so a
// half-bound org is not a state that exists.
func (se *settingsHandler) bindBedrock(w http.ResponseWriter, r *http.Request, orgID, userID string, b bedrockBinding) {
	var cleared []string
	if err := se.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		orgSet, err := tx.Orgs.GetSettings(r.Context(), orgID)
		if err != nil {
			return fmt.Errorf("load org settings: %w", err)
		}
		_, bedrock := storedLLMCredentials(orgSet)

		// Sweep BEFORE writing, not after. A shape's own optional key can be in
		// its clear set — the access-key pair's session token is, because an
		// absent token means "long-lived keys", never "keep the old one" — so a
		// sweep that ran second would delete the token this very request just
		// stored.
		//
		// The other Bedrock shapes' material must not linger either. The
		// resolver detects role mode by aws_role_arn presence, so a stale ARN
		// would make it try to mint instead of using the material stored here,
		// and stale IAM keys in the vault are wrong regardless of precedence.
		// The sweep stops at Bedrock: the org's Anthropic key, if any, stays —
		// it serves that provider's models and this write says nothing about
		// them.
		clear := b.clear
		if b.ref != integrations.KeyAWSRoleARN {
			// Leaving role mode drops the role ARN and its External ID together;
			// keeping an orphan ID would name a trust policy nothing presents.
			clear = append(append([]string{}, clear...), integrations.KeyAWSRoleARN, integrations.KeyAWSExternalID)
		}
		for _, k := range clear {
			if _, err := tx.Secrets.Delete(r.Context(), orgID, k); err != nil {
				return fmt.Errorf("clear stale %s: %w", k, err)
			}
		}

		if err := b.write(tx); err != nil {
			return err
		}

		if err := tx.Secrets.Put(r.Context(), orgID, integrations.KeyAWSRegion, b.config.Region, "Org's Bedrock region"); err != nil {
			return fmt.Errorf("store Bedrock region: %w", err)
		}
		if err := putOrClearSecret(r, tx, orgID, integrations.KeyBedrockModelID, b.config.ModelID, "Org's Bedrock model ID"); err != nil {
			return err
		}
		if err := putOrClearSecret(r, tx, orgID, integrations.KeyBedrockBaseURL, b.config.BaseURL, "Org's Bedrock endpoint override"); err != nil {
			return err
		}

		orgSet.BedrockCredentialsRef = b.ref
		onOwnCredentials(&orgSet)
		if _, err := tx.Orgs.UpdateSettings(r.Context(), orgID, orgSet); err != nil {
			return err
		}
		// Audit the bind/rotate in the same tx. The endpoint override is the
		// closest thing to a host and is recorded when set.
		if err := tx.AccessChangeLog.Record(r.Context(), orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialSet,
			DetailJSON:  accessDetailCredential(domain.CredentialKindBedrock, b.config.BaseURL),
		}); err != nil {
			return err
		}

		// A different Bedrock shape is material this write removed. It is the
		// same credential KIND for the change log, so it doesn't earn a removal
		// row beside its own bind — but the caller is told, because a settings
		// page that showed "IAM role" a moment ago has to stop.
		if bedrock != "" && bedrock != llmCredentialBedrock(bedrockAuthMethodFromRef(b.ref)) {
			cleared = append(cleared, bedrock)
		}
		return nil
	}); err != nil {
		internalError(w, "llm-credentials", fmt.Errorf("persist bedrock credentials: %w", err))
		return
	}

	writeJSON(w, http.StatusOK, llmCredentialResponse{Status: "connected", Cleared: nonNil(cleared)})
}

// validateBedrockConfig trims and checks the shared non-secret config, seeding
// the caller's validation so one response reports the config faults and the
// flavor's own together.
func validateBedrockConfig(c *bedrockConfig) *httpx.Validation {
	c.Region = strings.TrimSpace(c.Region)
	c.ModelID = strings.TrimSpace(c.ModelID)
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")

	v := &httpx.Validation{}
	switch {
	case c.Region == "":
		v.Missing("region")
	case !awsRegionRe.MatchString(c.Region):
		v.Invalid("region", fmt.Sprintf("region %q does not look like an AWS region (e.g. us-east-1, us-gov-west-1)", c.Region))
	}
	if c.BaseURL != "" {
		if err := validateBedrockBaseURL(c.BaseURL); err != nil {
			v.Invalid("base_url", err.Error())
		}
	}
	return v
}

// probeBedrockRole runs the live AssumeRole round-trip and renders its failure
// classes. They have different audiences — an operator problem the org admin
// can't fix from the UI, versus a trust-policy mismatch they can — so they
// surface distinct messages. Returns false with the response already written.
func (se *settingsHandler) probeBedrockRole(w http.ResponseWriter, resolver bedrockRoleResolver, r *http.Request, roleARN, externalID string) bool {
	err := resolver.ProbeRole(r.Context(), roleARN, externalID)
	if err == nil {
		return true
	}
	switch {
	case errors.Is(err, llmcred.ErrNoAmbientIdentity):
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
			Reason: httpx.ReasonNotConfigured, Message: operatorRemediationMsg,
		})
	case errors.Is(err, llmcred.ErrAssumeRoleDenied):
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
			Reason:  httpx.ReasonUpstreamRejected,
			Message: fmt.Sprintf("AssumeRole was denied — check the role's trust policy allows the control service and the External ID matches %q (re-open the setup to copy the trust-policy snippet)", externalID),
			Field:   "role_arn",
		})
	case errors.Is(err, llmcred.ErrRoleMisconfigured):
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
			Reason: httpx.ReasonUpstreamRejected, Message: err.Error(), Field: "role_arn",
		})
	default:
		// The raw AWS error stays in the log; only local mode sees it in the
		// body (LocalDetail), because an SDK error can carry account ids, ARNs
		// and request ids that belong to the operator, not to another tenant's
		// browser.
		settingsLog.Error("bedrock role connect probe failed", "error", err)
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
			Reason:  httpx.ReasonUpstreamRejected,
			Message: "AssumeRole failed" + httpx.LocalDetail(err),
			Field:   "role_arn",
		})
	}
	return false
}

// nonNil renders an empty list as [] rather than null, so a client reading
// `cleared` never has to distinguish "nothing was cleared" from "this server
// doesn't say".
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
