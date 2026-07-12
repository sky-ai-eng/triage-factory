package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/llmcred"
)

// Bedrock role mode (TFAC-616): the short-lived-credential Bedrock method.
// The org stores no Bedrock secret — only the customer role ARN and a
// TF-generated External ID — and the brain mints STS session creds per run.
// This file owns role mode's two live-AWS endpoints (both absent from the
// other Bedrock methods, which are shape-only): the role-setup GET (caller
// ARN + trust-policy snippet) and the connect probe (a real AssumeRole).

// bedrockRoleARNRe matches an IAM role ARN: arn:<partition>:iam::<12-digit
// account>:role/<path+name>. Covers aws, aws-us-gov, aws-cn partitions.
var bedrockRoleARNRe = regexp.MustCompile(`^arn:aws[a-z-]*:iam::\d{12}:role/.+`)

// operatorRemediationMsg is the "no ambient AWS identity" message — an
// OPERATOR problem the org admin can't fix from the UI. Surfaced by both the
// role-setup endpoint and the connect probe when the control service has no
// AWS identity.
const operatorRemediationMsg = "the control service has no AWS identity; set AWS_* on the control service or attach an instance role — see docs/self-host-setup.md"

// handleBedrockRoleSetup returns the control service's caller identity ARN
// (sts:GetCallerIdentity) and the org's TF-generated External ID, so the UI
// can render a filled, copyable trust-policy JSON — the setup documentation
// itself. Generates + persists the External ID on first call (stable
// thereafter). When the control service has no ambient AWS identity it
// returns the operator-remediation error, so the org admin sees it BEFORE
// building anything in AWS.
func (se *settingsHandler) handleBedrockRoleSetup(w http.ResponseWriter, r *http.Request) {
	userID := ClaimsFrom(r.Context()).Subject
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	resolver := se.bedrockRoleResolver()
	if resolver == nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "Bedrock role mode requires the multi-mode control service (this deployment has no ambient AWS identity to assume roles with)"})
		return
	}

	callerARN, err := resolver.CallerIdentityARN(r.Context())
	if err != nil {
		if errors.Is(err, llmcred.ErrNoAmbientIdentity) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": operatorRemediationMsg})
			return
		}
		settingsLog.Error("bedrock role-setup caller identity failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to resolve the control service's AWS identity"})
		return
	}

	var externalID string
	if err := se.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		externalID, err = ensureBedrockExternalID(r.Context(), tx, orgID)
		return err
	}); err != nil {
		settingsLog.Error("bedrock role-setup external id failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to prepare the External ID"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"caller_identity_arn": callerARN,
		"external_id":         externalID,
		"trust_policy":        bedrockTrustPolicyJSON(callerARN, externalID),
	})
}

// handleBedrockRoleConnect performs the live AssumeRole probe and, on
// success, stores the role config (no secret credential). It is the SINGLE
// write path into role mode: switching in requires a valid ARN + probe
// success, and the "leave blank to keep current" masking never applies (role
// mode holds no secret to keep). Called from handleBedrockConnect after the
// shared region / base-url validation.
func (se *settingsHandler) handleBedrockRoleConnect(w http.ResponseWriter, r *http.Request, orgID, userID string, req bedrockConnectRequest) {
	if req.RoleARN == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "role ARN is required"})
		return
	}
	if !bedrockRoleARNRe.MatchString(req.RoleARN) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": fmt.Sprintf("%q does not look like an IAM role ARN (e.g. arn:aws:iam::123456789012:role/tf-bedrock)", req.RoleARN)})
		return
	}
	resolver := se.bedrockRoleResolver()
	if resolver == nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "Bedrock role mode requires the multi-mode control service (this deployment has no ambient AWS identity to assume roles with)"})
		return
	}

	// Ensure the External ID exists (generate + persist if the admin skipped
	// the role-setup GET) BEFORE the probe — the probe must present the same
	// value the customer's trust policy references. It is stable thereafter.
	var externalID string
	if err := se.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var err error
		externalID, err = ensureBedrockExternalID(r.Context(), tx, orgID)
		return err
	}); err != nil {
		settingsLog.Error("bedrock role connect external id failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to prepare the External ID"})
		return
	}

	// Live probe — the real AssumeRole round-trip. The two failure classes
	// have different audiences (see below), so they surface distinct messages.
	if err := resolver.ProbeRole(r.Context(), req.RoleARN, externalID); err != nil {
		switch {
		case errors.Is(err, llmcred.ErrNoAmbientIdentity):
			// Operator problem — the org admin can't fix it from the UI.
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": operatorRemediationMsg})
		case errors.Is(err, llmcred.ErrAssumeRoleDenied):
			// Org-admin problem — trust policy or External ID mismatch.
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": fmt.Sprintf("AssumeRole was denied — check the role's trust policy allows the control service and the External ID matches %q (re-open the setup to copy the trust-policy snippet)", externalID)})
		case errors.Is(err, llmcred.ErrRoleMisconfigured):
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		default:
			settingsLog.Error("bedrock role connect probe failed", "error", err)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": fmt.Sprintf("AssumeRole failed: %v", err)})
		}
		return
	}

	// Probe passed — persist the role config. No secret is stored; role mode's
	// whole point is that the org's stored config contains no credential.
	if err := se.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		orgSet, err := tx.Orgs.GetSettings(r.Context(), orgID)
		if err != nil {
			return fmt.Errorf("load org settings: %w", err)
		}
		if err := tx.Secrets.Put(r.Context(), orgID, integrations.KeyAWSRoleARN, req.RoleARN, "Org's Bedrock IAM role ARN"); err != nil {
			return fmt.Errorf("store role ARN: %w", err)
		}
		if err := tx.Secrets.Put(r.Context(), orgID, integrations.KeyAWSRegion, req.Region, "Org's Bedrock region"); err != nil {
			return fmt.Errorf("store Bedrock region: %w", err)
		}
		if err := putOrClearSecret(r, tx, orgID, integrations.KeyBedrockModelID, req.ModelID, "Org's Bedrock model ID"); err != nil {
			return err
		}
		if err := putOrClearSecret(r, tx, orgID, integrations.KeyBedrockBaseURL, req.BaseURL, "Org's Bedrock endpoint override"); err != nil {
			return err
		}
		// Provider exclusivity: role mode holds the credential trust; every
		// other provider's stored material must go, or the resolver could pick
		// a stale key over the mint.
		for _, k := range []string{
			integrations.KeyAWSBearerTokenBedrock,
			integrations.KeyAWSAccessKeyID, integrations.KeyAWSSecretAccessKey, integrations.KeyAWSSessionToken,
			secretKeyAnthropicAPIKey,
		} {
			if _, err := tx.Secrets.Delete(r.Context(), orgID, k); err != nil {
				return fmt.Errorf("clear stale %s: %w", k, err)
			}
		}
		orgSet.AnthropicAPIKeyRef = ""
		orgSet.BedrockCredentialsRef = integrations.KeyAWSRoleARN
		if err := tx.Orgs.UpdateSettings(r.Context(), orgID, orgSet); err != nil {
			return err
		}
		return tx.AccessChangeLog.Record(r.Context(), orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialSet,
			DetailJSON:  accessDetailCredential(domain.CredentialKindBedrock, req.BaseURL),
		})
	}); err != nil {
		settingsLog.Error("bedrock role connect persist failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to store Bedrock role configuration"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "connected"})
}

// bedrockRoleResolver returns the wired resolver, or nil (local mode / not
// yet injected). Nil-safe: se.bedrockRole (the getter) is itself nil in unit
// tests that don't set it.
func (se *settingsHandler) bedrockRoleResolver() bedrockRoleResolver {
	if se.bedrockRole == nil {
		return nil
	}
	return se.bedrockRole()
}

// ensureBedrockExternalID reads the org's stored External ID, generating and
// persisting a fresh random one if absent (idempotent; stable thereafter —
// decision 3, the confused-deputy defense). Runs inside the caller's tx.
func ensureBedrockExternalID(ctx context.Context, tx db.TxStores, orgID string) (string, error) {
	existing, err := tx.Secrets.Get(ctx, orgID, integrations.KeyAWSExternalID)
	if err != nil {
		return "", fmt.Errorf("read external id: %w", err)
	}
	if existing != "" {
		return existing, nil
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate external id: %w", err)
	}
	externalID := "tf-" + hex.EncodeToString(buf)
	if err := tx.Secrets.Put(ctx, orgID, integrations.KeyAWSExternalID, externalID, "TF-generated Bedrock role External ID"); err != nil {
		return "", fmt.Errorf("store external id: %w", err)
	}
	return externalID, nil
}

// bedrockTrustPolicyJSON renders the copyable trust-policy the customer
// attaches to their IAM role: it allows the control service's caller ARN to
// AssumeRole only when the request carries the TF-generated External ID. This
// snippet IS the setup documentation — no prose walkthrough.
func bedrockTrustPolicyJSON(callerARN, externalID string) string {
	policy := map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{{
			"Effect":    "Allow",
			"Principal": map[string]any{"AWS": callerARN},
			"Action":    "sts:AssumeRole",
			"Condition": map[string]any{"StringEquals": map[string]any{"sts:ExternalId": externalID}},
		}},
	}
	b, err := json.MarshalIndent(policy, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}
