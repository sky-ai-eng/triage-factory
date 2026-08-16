package server

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// Bedrock connect — the AWS analog of handleAnthropicConnect and the SINGLE
// write path for the org's Bedrock configuration (the bulk org-settings POST
// accepts none of these fields, so nothing reaches the vault unvalidated).
//
// Two auth methods, mirroring what the credential resolver
// (internal/agentproc) consumes:
//
//   - "bearer": a Bedrock API key (AWS_BEARER_TOKEN_BEDROCK).
//   - "access_keys": the IAM access-key triple (id + secret + optional
//     session token), served at run time by the SigV4 re-signing proxy.
//
// Alongside the credential ride three non-secret config values: the region
// (required — it names both the endpoint and the SigV4 signing scope), an
// optional model ID (inference profile), and an optional endpoint override
// (base_url) for VPC interface endpoints / GovCloud / the China partition,
// whose hostnames the standard regional formula can't produce.
//
// Unlike the Anthropic key there is NO live credential probe here, by
// decision rather than omission: an IAM key scoped to bedrock:InvokeModel
// only (the recommended posture) fails every free control-plane call, a
// runtime invoke probe costs real money and presumes model access, and a
// VPC-endpoint org's Bedrock may not be reachable from the TF server at
// all. Validation is shape-only; a bad credential surfaces on the first
// run with the AWS error attached.
//
// Provider exclusivity: the resolver prefers an Anthropic key over Bedrock,
// so a Bedrock connect that left an Anthropic key in place would be a
// silent no-op. Connecting Bedrock therefore clears the Anthropic key in
// the same transaction (and handleAnthropicConnect symmetrically clears the
// Bedrock set) — the UI frames these as switching provider, never as
// holding two.

// bedrockAuthMethodBearer / bedrockAuthMethodAccessKeys are the wire values
// for the connect request's auth_method and the GET response's
// bedrock_auth_method. The stored marker (org_settings.bedrock_credentials_ref)
// is the primary secret's key name, like AnthropicAPIKeyRef.
const (
	bedrockAuthMethodBearer     = "bearer"
	bedrockAuthMethodAccessKeys = "access_keys"
	// bedrockAuthMethodRole is the short-lived-credential method (TFAC-616):
	// the org stores no Bedrock secret at all — only the customer role ARN
	// and a TF-generated External ID — and the brain mints STS session creds
	// per run. It is the only Bedrock method with a live connect-time probe.
	bedrockAuthMethodRole = "role"
	// bedrockAuthMethodNone is request-only: the explicit clear.
	bedrockAuthMethodNone = "none"
)

// bedrockAuthMethodFromRef maps the stored ref marker back to the wire
// auth-method value ("" when Bedrock isn't configured).
func bedrockAuthMethodFromRef(ref string) string {
	switch ref {
	case integrations.KeyAWSBearerTokenBedrock:
		return bedrockAuthMethodBearer
	case integrations.KeyAWSAccessKeyID:
		return bedrockAuthMethodAccessKeys
	case integrations.KeyAWSRoleARN:
		return bedrockAuthMethodRole
	}
	return ""
}

// awsRegionRe matches AWS region identifiers: a two-letter partition
// prefix, one or more lowercase words, and a numeric suffix — us-east-1,
// us-gov-west-1, eu-central-1, cn-north-1.
var awsRegionRe = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-\d+$`)

// validateBedrockBaseURL enforces the same rules the proxy applies to its
// upstream (agentproc's validateProxyUpstream, kept in lockstep): https
// with a host and no path / query / fragment, because the proxy forwards
// the incoming request path verbatim. Loopback http is allowed for tests.
// A trailing "/" is the caller's to strip before calling.
func validateBedrockBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("endpoint URL: %v", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("endpoint URL must include scheme and host: %q", raw)
	}
	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("endpoint URL must not include a path: %q", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("endpoint URL must not include query or fragment: %q", raw)
	}
	if u.Scheme != "https" {
		host, _, _ := net.SplitHostPort(u.Host)
		if host == "" {
			host = u.Host
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("endpoint URL must use https: %q", raw)
		}
	}
	return nil
}

// bedrockConnectRequest is the wire shape of POST /api/bedrock/connect.
// Secret fields follow the repo's "leave blank to keep current" convention:
// when auth_method matches what's stored and every secret field is blank,
// the stored credential is kept and only the non-secret config (region /
// model_id / base_url) is updated. Any non-blank secret means "replace" and
// the full set for the method is required.
type bedrockConnectRequest struct {
	AuthMethod      string `json:"auth_method"` // "role" | "bearer" | "access_keys" | "none" (clear)
	BearerToken     string `json:"bearer_token"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token"`
	// RoleARN is the customer IAM role the control service assumes (role
	// mode). Plain text, non-secret. The External ID is TF-generated (never
	// taken from the request — see decision 3) and returned by the role-setup
	// endpoint.
	RoleARN string `json:"role_arn"`
	Region  string `json:"region"`   // required for every auth method
	ModelID string `json:"model_id"` // optional; blank clears
	BaseURL string `json:"base_url"` // optional endpoint override; blank clears
}

func (se *settingsHandler) handleBedrockConnect(w http.ResponseWriter, r *http.Request) {
	userID := ClaimsFrom(r.Context()).Subject
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	var req bedrockConnectRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	req.AuthMethod = strings.TrimSpace(req.AuthMethod)
	req.BearerToken = strings.TrimSpace(req.BearerToken)
	req.AccessKeyID = strings.TrimSpace(req.AccessKeyID)
	req.SecretAccessKey = strings.TrimSpace(req.SecretAccessKey)
	req.SessionToken = strings.TrimSpace(req.SessionToken)
	req.RoleARN = strings.TrimSpace(req.RoleARN)
	req.Region = strings.TrimSpace(req.Region)
	req.ModelID = strings.TrimSpace(req.ModelID)
	req.BaseURL = strings.TrimRight(strings.TrimSpace(req.BaseURL), "/")

	// Explicit clear: remove every Bedrock key + the ref. Mirrors the
	// Anthropic empty-key arm; idempotent.
	if req.AuthMethod == bedrockAuthMethodNone {
		if err := se.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			// Settings first — its ref tells us whether there was anything to
			// revoke, which the idempotent vault deletes cannot.
			orgSet, err := tx.Orgs.GetSettings(r.Context(), orgID)
			if err != nil {
				return fmt.Errorf("load org settings: %w", err)
			}
			hadBedrock := orgSet.BedrockCredentialsRef != ""
			if err := clearBedrockSecrets(r, tx, orgID); err != nil {
				return err
			}
			orgSet.BedrockCredentialsRef = ""
			if err := tx.Orgs.UpdateSettings(r.Context(), orgID, orgSet); err != nil {
				return err
			}
			if !hadBedrock {
				return nil
			}
			return recordCredentialRemovals(r.Context(), tx, orgID, userID,
				[]string{domain.CredentialKindBedrock})
		}); err != nil {
			internalError(w, "settings", fmt.Errorf("clear bedrock credentials: %w", err))
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
		return
	}

	if req.AuthMethod != bedrockAuthMethodBearer && req.AuthMethod != bedrockAuthMethodAccessKeys && req.AuthMethod != bedrockAuthMethodRole {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonInvalidField,
			Message: `auth_method must be "role", "bearer", "access_keys", or "none"`,
			Field:   "auth_method",
		})
		return
	}
	if req.Region == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonMissingField,
			Message: "region is required (it names the Bedrock endpoint and the SigV4 signing scope)",
			Field:   "region",
		})
		return
	}
	if !awsRegionRe.MatchString(req.Region) {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonInvalidField,
			Message: fmt.Sprintf("region %q does not look like an AWS region (e.g. us-east-1, us-gov-west-1)", req.Region),
			Field:   "region",
		})
		return
	}
	if req.BaseURL != "" {
		if err := validateBedrockBaseURL(req.BaseURL); err != nil {
			httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{Reason: httpx.ReasonInvalidField, Message: err.Error()})
			return
		}
	}

	// Role mode (TFAC-616) is the only Bedrock method with a live connect
	// probe and stores no secret credential — handled on its own path.
	if req.AuthMethod == bedrockAuthMethodRole {
		se.handleBedrockRoleConnect(w, r, orgID, userID, req)
		return
	}
	// Partial access-key pairs are always a mistake — catch before the
	// keep-current logic can misread them.
	if req.AuthMethod == bedrockAuthMethodAccessKeys &&
		(req.AccessKeyID == "") != (req.SecretAccessKey == "") {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonMissingField,
			Message: "provide both the access key ID and the secret access key (or neither, to keep the stored pair)",
			Field:   "access_key_id",
		})
		return
	}
	secretsProvided := req.BearerToken != "" || req.AccessKeyID != "" || req.SecretAccessKey != "" || req.SessionToken != ""

	if err := se.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		orgSet, err := tx.Orgs.GetSettings(r.Context(), orgID)
		if err != nil {
			return fmt.Errorf("load org settings: %w", err)
		}
		storedMethod := bedrockAuthMethodFromRef(orgSet.BedrockCredentialsRef)

		// Keep-current is only valid when the stored credential is the
		// same method — otherwise there is nothing usable to keep.
		keepCurrent := !secretsProvided && storedMethod == req.AuthMethod
		if !secretsProvided && !keepCurrent {
			return errBedrockMissingSecret(req.AuthMethod)
		}

		if !keepCurrent {
			switch req.AuthMethod {
			case bedrockAuthMethodBearer:
				if req.BearerToken == "" {
					return errBedrockMissingSecret(req.AuthMethod)
				}
				if err := tx.Secrets.Put(r.Context(), orgID, integrations.KeyAWSBearerTokenBedrock, req.BearerToken, "Org's Bedrock API key"); err != nil {
					return fmt.Errorf("store Bedrock bearer token: %w", err)
				}
				// The other method's keys must not linger — the resolver
				// prefers the bearer, but stale IAM material in the vault
				// is wrong regardless of precedence.
				for _, k := range []string{integrations.KeyAWSAccessKeyID, integrations.KeyAWSSecretAccessKey, integrations.KeyAWSSessionToken} {
					if _, err := tx.Secrets.Delete(r.Context(), orgID, k); err != nil {
						return fmt.Errorf("clear stale %s: %w", k, err)
					}
				}
			case bedrockAuthMethodAccessKeys:
				if req.AccessKeyID == "" || req.SecretAccessKey == "" {
					return errBedrockMissingSecret(req.AuthMethod)
				}
				if err := tx.Secrets.Put(r.Context(), orgID, integrations.KeyAWSAccessKeyID, req.AccessKeyID, "Org's AWS access key ID"); err != nil {
					return fmt.Errorf("store AWS access key ID: %w", err)
				}
				if err := tx.Secrets.Put(r.Context(), orgID, integrations.KeyAWSSecretAccessKey, req.SecretAccessKey, "Org's AWS secret access key"); err != nil {
					return fmt.Errorf("store AWS secret access key: %w", err)
				}
				// The session token is part of the replaced set: a blank
				// here means "no session token" (long-lived keys), not
				// "keep the old one" — a stale token would 403 every call.
				if req.SessionToken != "" {
					if err := tx.Secrets.Put(r.Context(), orgID, integrations.KeyAWSSessionToken, req.SessionToken, "Org's AWS session token"); err != nil {
						return fmt.Errorf("store AWS session token: %w", err)
					}
				} else if _, err := tx.Secrets.Delete(r.Context(), orgID, integrations.KeyAWSSessionToken); err != nil {
					return fmt.Errorf("clear AWS session token: %w", err)
				}
				if _, err := tx.Secrets.Delete(r.Context(), orgID, integrations.KeyAWSBearerTokenBedrock); err != nil {
					return fmt.Errorf("clear stale Bedrock bearer token: %w", err)
				}
			}
		}

		// Non-secret config is taken literally on every connect: the form
		// always submits current values, so blank means "unset" (model /
		// endpoint fall back to defaults), never "keep".
		if err := tx.Secrets.Put(r.Context(), orgID, integrations.KeyAWSRegion, req.Region, "Org's Bedrock region"); err != nil {
			return fmt.Errorf("store Bedrock region: %w", err)
		}
		if err := putOrClearSecret(r, tx, orgID, integrations.KeyBedrockModelID, req.ModelID, "Org's Bedrock model ID"); err != nil {
			return err
		}
		if err := putOrClearSecret(r, tx, orgID, integrations.KeyBedrockBaseURL, req.BaseURL, "Org's Bedrock endpoint override"); err != nil {
			return err
		}

		// Provider exclusivity: an Anthropic key would win in the
		// resolver, silently ignoring everything stored above.
		if _, err := tx.Secrets.Delete(r.Context(), orgID, secretKeyAnthropicAPIKey); err != nil {
			return fmt.Errorf("clear Anthropic key on provider switch: %w", err)
		}
		// A stale role ARN would make the llmcred resolver treat this org as
		// role mode (it detects role by aws_role_arn presence) and try to mint
		// instead of using the bearer/access-key material stored above — clear
		// the role keys when switching to a static Bedrock method (TFAC-616).
		for _, k := range []string{integrations.KeyAWSRoleARN, integrations.KeyAWSExternalID} {
			if _, err := tx.Secrets.Delete(r.Context(), orgID, k); err != nil {
				return fmt.Errorf("clear stale %s on provider switch: %w", k, err)
			}
		}
		droppedAnthropic := orgSet.AnthropicAPIKeyRef != ""
		orgSet.AnthropicAPIKeyRef = ""
		if req.AuthMethod == bedrockAuthMethodBearer {
			orgSet.BedrockCredentialsRef = integrations.KeyAWSBearerTokenBedrock
		} else {
			orgSet.BedrockCredentialsRef = integrations.KeyAWSAccessKeyID
		}
		if err := tx.Orgs.UpdateSettings(r.Context(), orgID, orgSet); err != nil {
			return err
		}
		// Audit the bind/rotate in the same tx, like the Anthropic arm. The
		// endpoint override is the closest thing to a host and is recorded when set.
		if err := tx.AccessChangeLog.Record(r.Context(), orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialSet,
			DetailJSON:  accessDetailCredential(domain.CredentialKindBedrock, req.BaseURL),
		}); err != nil {
			return err
		}
		// The exclusivity sweep above revoked the org's Anthropic key — an
		// admin auditing "when did we stop using the Anthropic key" should find
		// it here, not have to infer it from the Bedrock bind.
		if !droppedAnthropic {
			return nil
		}
		return recordCredentialRemovals(r.Context(), tx, orgID, userID,
			[]string{domain.CredentialKindAnthropicKey})
	}); err != nil {
		if missing, ok := err.(bedrockMissingSecretError); ok {
			httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
				Reason: httpx.ReasonMissingField, Message: string(missing),
			})
			return
		}
		internalError(w, "settings", fmt.Errorf("persist bedrock credentials: %w", err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "connected"})
}

// bedrockMissingSecretError carries a 422-worthy validation failure out of
// the WithTx closure, where the stored method (needed to distinguish
// keep-current from missing-credential) first becomes known.
type bedrockMissingSecretError string

func (e bedrockMissingSecretError) Error() string { return string(e) }

func errBedrockMissingSecret(method string) error {
	if method == bedrockAuthMethodBearer {
		return bedrockMissingSecretError("a Bedrock API key is required (no stored key to keep)")
	}
	return bedrockMissingSecretError("an AWS access key ID and secret access key are required (no stored pair to keep)")
}

// clearBedrockSecrets removes every Bedrock-managed key. Delete of an
// absent key is a no-op, so this is safe on an unconfigured org.
func clearBedrockSecrets(r *http.Request, tx db.TxStores, orgID string) error {
	for _, k := range integrations.BedrockKeys() {
		if _, err := tx.Secrets.Delete(r.Context(), orgID, k); err != nil {
			return fmt.Errorf("clear %s: %w", k, err)
		}
	}
	return nil
}

// putOrClearSecret writes value under key, or deletes the key when value
// is blank — the "taken literally" semantics of the non-secret config
// fields.
func putOrClearSecret(r *http.Request, tx db.TxStores, orgID, key, value, desc string) error {
	if value == "" {
		if _, err := tx.Secrets.Delete(r.Context(), orgID, key); err != nil {
			return fmt.Errorf("clear %s: %w", key, err)
		}
		return nil
	}
	if err := tx.Secrets.Put(r.Context(), orgID, key, value, desc); err != nil {
		return fmt.Errorf("store %s: %w", key, err)
	}
	return nil
}
