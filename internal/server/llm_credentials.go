package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/cred/llmcred"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/llm/modelcatalog"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
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
	Status  string   `json:"status" doc:"Always connected."`
	Cleared []string `json:"cleared" doc:"The organization's stored Bedrock credential of another kind that this write removed, as bedrock:role, bedrock:bearer, or bedrock:access_keys; empty when it removed none, and always empty for the Anthropic key."`
}

// credentialDisconnectedResponse is what an LLM or Jira credential unbind
// answers.
type credentialDisconnectedResponse struct {
	Status string `json:"status" doc:"Always disconnected."`
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

// anthropicCredentialRequest is the body of PUT …/llm/anthropic.
type anthropicCredentialRequest struct {
	APIKey string `json:"api_key" doc:"The Anthropic API key, which must not be empty; surrounding whitespace is trimmed."`
}

var opAnthropicPut = httpx.NewJSONOp[anthropicCredentialRequest, llmCredentialResponse](httpx.OpSpec{
	Pattern:     "PUT /api/orgs/{org_id}/llm/anthropic",
	ID:          "replace_anthropic_credential",
	Summary:     "Set the organization's Anthropic API key",
	Description: "Checks the key with Anthropic, then stores it as the organization's Anthropic API key, replacing any stored one, and moves the organization off the host's Claude credentials. The organization's Bedrock credential is left as it is." + llmRetrySentence,
	PathParams:  map[string]string{"org_id": "The organization's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintYes},
})

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
	req, ok := opAnthropicPut.Decode(w, r)
	if !ok {
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

	// In local mode the key lands in the keychain, outside the transaction, so
	// a failure later in it puts the prior key back rather than leaving one the
	// org never committed to.
	restore, unlock, err := se.guardAnthropicWrite(r.Context(), orgID)
	if err != nil {
		internalError(w, "llm-credentials", err)
		return
	}
	defer unlock()

	if err := se.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if err := tx.Secrets.Put(r.Context(), orgID, secretKeyAnthropicAPIKey, key, "Org's Anthropic API key"); err != nil {
			return fmt.Errorf("store Anthropic key: %w", err)
		}
		// The ref write also moves the org onto its own credentials: holding
		// provider material and running on the host's are mutually exclusive,
		// and the bind is what settles which one the org is doing. The org's
		// Bedrock material, if any, is untouched: both providers are live at
		// once, and each run resolves the one its model is served by.
		if _, err := tx.Orgs.SetAnthropicKeyRef(r.Context(), orgID, secretKeyAnthropicAPIKey); err != nil {
			return fmt.Errorf("set anthropic key ref: %w", err)
		}
		return tx.AccessChangeLog.Record(r.Context(), orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialSet,
			DetailJSON:  accessDetailCredential(domain.CredentialKindAnthropicKey, ""),
		})
	}); err != nil {
		if restore != nil {
			restore()
		}
		internalError(w, "llm-credentials", fmt.Errorf("persist anthropic credentials: %w", err))
		return
	}

	se.kickMemoryBacklog(orgID)
	opAnthropicPut.Write(w, llmCredentialResponse{Status: "connected", Cleared: []string{}})
}

var opAnthropicDelete = httpx.NewJSONOp[httpx.NoBody, credentialDisconnectedResponse](httpx.OpSpec{
	Pattern:     "DELETE /api/orgs/{org_id}/llm/anthropic",
	ID:          "delete_anthropic_credential",
	Summary:     "Remove the organization's Anthropic API key",
	Description: "Deletes the organization's Anthropic API key, and succeeds when none is stored. The organization's Bedrock credential is left as it is, and the organization does not move onto the host's Claude credentials, so runs on Anthropic models are refused until a key is set again.",
	PathParams:  map[string]string{"org_id": "The organization's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintNo},
})

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

	// In local mode the keychain sits outside the transaction, so a failure
	// after the delete puts the key back: the request reports the disconnect
	// failed, so the org must still be connected.
	restore, unlock, err := se.guardAnthropicWrite(r.Context(), orgID)
	if err != nil {
		internalError(w, "llm-credentials", err)
		return
	}
	defer unlock()

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
		if _, err := tx.Orgs.SetAnthropicKeyRef(r.Context(), orgID, ""); err != nil {
			return fmt.Errorf("clear anthropic key ref: %w", err)
		}
		if !had {
			return nil
		}
		return recordCredentialRemovals(r.Context(), tx, orgID, userID,
			[]string{domain.CredentialKindAnthropicKey})
	}); err != nil {
		if restore != nil {
			restore()
		}
		internalError(w, "llm-credentials", fmt.Errorf("clear anthropic credentials: %w", err))
		return
	}
	opAnthropicDelete.Write(w, credentialDisconnectedResponse{Status: "disconnected"})
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
	Region  string `json:"region" doc:"The AWS region Bedrock is called in, such as us-east-1."`
	ModelID string `json:"model_id" doc:"A Bedrock model id or inference profile ARN for runs whose own model is not a Bedrock model; absent or empty stores none."`
	BaseURL string `json:"base_url" doc:"An https URL with a host and no path that replaces the region's Bedrock endpoint, for a VPC endpoint or another partition; absent or empty uses the region's endpoint."`
}

// bedrockAccessKeysRequest is PUT …/llm/bedrock/access-keys: the IAM
// access-key pair the SigV4 re-signing proxy serves at run time, plus an
// optional session token for temporary credentials.
type bedrockAccessKeysRequest struct {
	AccessKeyID     string `json:"access_key_id" doc:"The IAM access key id."`
	SecretAccessKey string `json:"secret_access_key" doc:"The IAM secret access key."`
	// An empty token deletes a stored one, the same as an absent token: the
	// PUT replaces the whole credential.
	SessionToken string `json:"session_token" doc:"The session token of temporary credentials; absent or empty stores none and deletes a stored one."`
	bedrockConfig
}

// bedrockBearerRequest is PUT …/llm/bedrock/bearer: a Bedrock API key
// (AWS_BEARER_TOKEN_BEDROCK).
type bedrockBearerRequest struct {
	BearerToken string `json:"bearer_token" doc:"The Bedrock API key."`
	bedrockConfig
}

// bedrockRoleRequest is PUT …/llm/bedrock/role: the customer IAM role the
// control service assumes to mint per-run session credentials. Plain text,
// non-secret. The External ID is TF-generated and never taken from the request
// — see handleBedrockRoleSetup, which hands the admin the trust policy that
// pins it.
type bedrockRoleRequest struct {
	RoleARN string `json:"role_arn" doc:"The ARN of the IAM role to assume, such as arn:aws:iam::123456789012:role/tf-bedrock."`
	bedrockConfig
}

// bedrockReplaceSentence and llmRetrySentence are the sentences the LLM
// credential binds' descriptions share.
const (
	bedrockReplaceSentence = " It replaces any Bedrock credential the organization holds, of any kind, and lists a different kind it removed in cleared; the organization's Anthropic key is left as it is."
	llmRetrySentence       = " It also retries the memory generations still owed in the organization, which call the model provider."
)

var opBedrockAccessKeysPut = httpx.NewJSONOp[bedrockAccessKeysRequest, llmCredentialResponse](httpx.OpSpec{
	Pattern:     "PUT /api/orgs/{org_id}/llm/bedrock/access-keys",
	ID:          "replace_bedrock_access_keys",
	Summary:     "Connect the organization to Bedrock with IAM access keys",
	Description: "Stores an IAM access key pair as the organization's Bedrock credential, after checking only its shape." + bedrockReplaceSentence + llmRetrySentence,
	PathParams:  map[string]string{"org_id": "The organization's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintYes},
})

var opBedrockBearerPut = httpx.NewJSONOp[bedrockBearerRequest, llmCredentialResponse](httpx.OpSpec{
	Pattern:     "PUT /api/orgs/{org_id}/llm/bedrock/bearer",
	ID:          "replace_bedrock_bearer_token",
	Summary:     "Connect the organization to Bedrock with an API key",
	Description: "Stores a Bedrock API key as the organization's Bedrock credential, after checking only its shape." + bedrockReplaceSentence + llmRetrySentence,
	PathParams:  map[string]string{"org_id": "The organization's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintYes},
})

var opBedrockRolePut = httpx.NewJSONOp[bedrockRoleRequest, llmCredentialResponse](httpx.OpSpec{
	Pattern:     "PUT /api/orgs/{org_id}/llm/bedrock/role",
	ID:          "replace_bedrock_role",
	Summary:     "Connect the organization to Bedrock with an IAM role",
	Description: "Checks the IAM role by assuming it with the organization's External ID and stores it as the organization's Bedrock credential, from which each run gets short-lived credentials; the route works in multi mode only." + bedrockReplaceSentence + llmRetrySentence,
	PathParams:  map[string]string{"org_id": "The organization's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintYes},
})

// handleBedrockAccessKeysPut binds the IAM access-key pair.
//
// PUT /api/orgs/{org_id}/llm/bedrock/access-keys
func (se *settingsHandler) handleBedrockAccessKeysPut(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := se.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	req, ok := opBedrockAccessKeysPut.Decode(w, r)
	if !ok {
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
	resp, ok := se.bindBedrock(w, r, orgID, userID, bedrockBinding{
		ref:    integrations.KeyAWSAccessKeyID,
		config: req.bedrockConfig,
		secrets: []bedrockSecret{
			{integrations.KeyAWSAccessKeyID, req.AccessKeyID, "Org's AWS access key ID"},
			{integrations.KeyAWSSecretAccessKey, req.SecretAccessKey, "Org's AWS secret access key"},
			// Absent means "no session token" (long-lived keys), never "keep
			// the old one" — a stale token would 403 every call.
			{integrations.KeyAWSSessionToken, req.SessionToken, "Org's AWS session token"},
		},
	})
	if !ok {
		return
	}
	opBedrockAccessKeysPut.Write(w, resp)
}

// handleBedrockBearerPut binds a Bedrock API key.
//
// PUT /api/orgs/{org_id}/llm/bedrock/bearer
func (se *settingsHandler) handleBedrockBearerPut(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := se.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	req, ok := opBedrockBearerPut.Decode(w, r)
	if !ok {
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

	resp, ok := se.bindBedrock(w, r, orgID, userID, bedrockBinding{
		ref:    integrations.KeyAWSBearerTokenBedrock,
		config: req.bedrockConfig,
		secrets: []bedrockSecret{
			{integrations.KeyAWSBearerTokenBedrock, req.BearerToken, "Org's Bedrock API key"},
		},
	})
	if !ok {
		return
	}
	opBedrockBearerPut.Write(w, resp)
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
	req, ok := opBedrockRolePut.Decode(w, r)
	if !ok {
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
	// why the role bind never clears it.
	externalID, err := se.resolveExternalID(r.Context(), orgID, userID)
	if err != nil {
		internalError(w, "llm-credentials", fmt.Errorf("prepare bedrock external id: %w", err))
		return
	}
	if !se.probeBedrockRole(w, resolver, r, req.RoleARN, externalID) {
		return
	}

	resp, ok := se.bindBedrock(w, r, orgID, userID, bedrockBinding{
		ref:    integrations.KeyAWSRoleARN,
		config: req.bedrockConfig,
		secrets: []bedrockSecret{
			{integrations.KeyAWSRoleARN, req.RoleARN, "Org's Bedrock IAM role ARN"},
		},
	})
	if !ok {
		return
	}
	opBedrockRolePut.Write(w, resp)
}

var opBedrockDelete = httpx.NewJSONOp[httpx.NoBody, credentialDisconnectedResponse](httpx.OpSpec{
	Pattern:     "DELETE /api/orgs/{org_id}/llm/bedrock",
	ID:          "delete_bedrock_credential",
	Summary:     "Remove the organization's Bedrock credential",
	Description: "Deletes the organization's Bedrock credential of whatever kind, with its region, model id, endpoint, and the role's External ID, and succeeds when none is stored. The organization's Anthropic key is left as it is.",
	PathParams:  map[string]string{"org_id": "The organization's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleBedrockDelete removes every piece of Bedrock material the org holds —
// credential, non-secret config, role ARN and External ID alike. Idempotent.
//
// DELETE /api/orgs/{org_id}/llm/bedrock
func (se *settingsHandler) handleBedrockDelete(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := se.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}

	// In local mode the keychain sits outside the transaction, so a failure
	// after the deletes puts every key back: the request reports the
	// disconnect failed, so the org must still be connected.
	restore, unlock, err := se.guardBedrockWrite(r.Context(), orgID)
	if err != nil {
		internalError(w, "llm-credentials", err)
		return
	}
	defer unlock()

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
		if _, err := tx.Orgs.SetBedrockCredentialsRef(r.Context(), orgID, ""); err != nil {
			return fmt.Errorf("clear bedrock credentials ref: %w", err)
		}
		if !had {
			return nil
		}
		return recordCredentialRemovals(r.Context(), tx, orgID, userID,
			[]string{domain.CredentialKindBedrock})
	}); err != nil {
		if restore != nil {
			restore()
		}
		internalError(w, "llm-credentials", fmt.Errorf("clear bedrock credentials: %w", err))
		return
	}
	opBedrockDelete.Write(w, credentialDisconnectedResponse{Status: "disconnected"})
}

// bedrockBinding is what one flavor's PUT contributes to the shared write: the
// ref marker that records which shape is live, the non-secret config, and the
// flavor's own material.
type bedrockBinding struct {
	ref     string
	config  bedrockConfig
	secrets []bedrockSecret
}

// bedrockSecret is one Bedrock key a bind leaves in the vault. An empty value
// is an optional key the request left out, which the bind deletes.
type bedrockSecret struct{ key, value, desc string }

// bindBedrock performs the parts every Bedrock flavor shares: the flavor's own
// writes, the non-secret config, the sweep of the Bedrock material this bind
// replaces, the settings ref, and the audit row — all in one transaction, so a
// half-bound org is not a state that exists.
//
// On a failure the response is already written and ok is false; on success the
// caller writes resp through its own operation.
func (se *settingsHandler) bindBedrock(w http.ResponseWriter, r *http.Request, orgID, userID string, b bedrockBinding) (resp llmCredentialResponse, ok bool) {
	set := map[string]bedrockSecret{}
	for _, sec := range append(b.secrets,
		bedrockSecret{integrations.KeyAWSRegion, b.config.Region, "Org's Bedrock region"},
		bedrockSecret{integrations.KeyBedrockModelID, b.config.ModelID, "Org's Bedrock model ID"},
		bedrockSecret{integrations.KeyBedrockBaseURL, b.config.BaseURL, "Org's Bedrock endpoint override"},
	) {
		set[sec.key] = sec
	}

	// In local mode the keys land in the keychain, outside the transaction, so
	// a failure later in it puts every prior key back rather than leaving the
	// org half-bound, or bound to a shape it never committed to.
	restore, unlock, err := se.guardBedrockWrite(r.Context(), orgID)
	if err != nil {
		internalError(w, "llm-credentials", err)
		return llmCredentialResponse{}, false
	}
	defer unlock()

	var cleared []string
	if err := se.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		orgSet, err := tx.Orgs.GetSettings(r.Context(), orgID)
		if err != nil {
			return fmt.Errorf("load org settings: %w", err)
		}
		_, bedrock := storedLLMCredentials(orgSet)

		// One pass over integrations.BedrockKeys, the order the unbind deletes
		// them in: each key is written if this bind sets it and deleted
		// otherwise, so the other Bedrock shapes' material does not linger. The
		// resolver detects role mode by aws_role_arn presence, so a stale ARN
		// would make it try to mint instead of using the material stored here,
		// and stale IAM keys in the vault are wrong regardless of precedence.
		// Leaving role mode drops the role ARN and its External ID together;
		// keeping an orphan ID would name a trust policy nothing presents. Role
		// mode itself keeps the ID, which is stable once generated. The sweep
		// stops at Bedrock: the org's Anthropic key, if any, stays — it serves
		// that provider's models and this write says nothing about them.
		for _, k := range integrations.BedrockKeys() {
			if k == integrations.KeyAWSExternalID && b.ref == integrations.KeyAWSRoleARN {
				continue
			}
			sec := set[k]
			if err := putOrClearSecret(r, tx, orgID, k, sec.value, sec.desc); err != nil {
				return err
			}
		}

		// The ref write also moves the org onto its own credentials, as the
		// Anthropic bind's does.
		if _, err := tx.Orgs.SetBedrockCredentialsRef(r.Context(), orgID, b.ref); err != nil {
			return fmt.Errorf("set bedrock credentials ref: %w", err)
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
		if restore != nil {
			restore()
		}
		internalError(w, "llm-credentials", fmt.Errorf("persist bedrock credentials: %w", err))
		return llmCredentialResponse{}, false
	}

	// The org-wide memory doorbell, for all three Bedrock shapes at once —
	// they differ in what they store, not in what a bind means for a memory
	// generation that had no provider to reach. After the commit: a nudge the
	// transaction then rolled back would re-run a backlog against the same
	// missing credential.
	se.kickMemoryBacklog(orgID)
	return llmCredentialResponse{Status: "connected", Cleared: nonNil(cleared)}, true
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
		if err := validateBedrockBaseURL(c.BaseURL, runmode.Current() == runmode.ModeMulti); err != nil {
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
