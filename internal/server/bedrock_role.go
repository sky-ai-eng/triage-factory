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
	"strings"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/llmcred"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// Bedrock role mode (TFAC-616): the short-lived-credential Bedrock method.
// The org stores no Bedrock secret — only the customer role ARN and a
// TF-generated External ID — and the brain mints STS session creds per run.
// This file owns the role-setup endpoint (caller ARN + trust-policy snippet)
// and the External-ID machinery both it and the bind depend on. The bind
// itself — including the live AssumeRole probe, the only such probe on any
// Bedrock method — lives with its sibling flavors in llm_credentials.go.

// bedrockRoleARNRe matches an IAM role ARN: arn:<partition>:iam::<12-digit
// account>:role/<path+name>. Covers aws, aws-us-gov, aws-cn partitions.
var bedrockRoleARNRe = regexp.MustCompile(`^arn:aws[a-z-]*:iam::\d{12}:role/.+`)

// operatorRemediationMsg is the "no ambient AWS identity" message — an
// OPERATOR problem the org admin can't fix from the UI. Surfaced by both the
// role-setup endpoint and the connect probe when the control service has no
// AWS identity.
const operatorRemediationMsg = "the control service has no AWS identity; set AWS_* on the control service or attach an instance role — see docs/self-hosting/bedrock-role-mode.md"

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
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
			Reason:  httpx.ReasonNotConfigured,
			Message: "Bedrock role mode requires the multi-mode control service (this deployment has no ambient AWS identity to assume roles with)",
		})
		return
	}

	callerARN, err := resolver.CallerIdentityARN(r.Context())
	if err != nil {
		if errors.Is(err, llmcred.ErrNoAmbientIdentity) {
			httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
				Reason: httpx.ReasonNotConfigured, Message: operatorRemediationMsg,
			})
			return
		}
		internalError(w, "settings", fmt.Errorf("resolve control-service AWS identity: %w", err))
		return
	}

	externalID, err := se.resolveExternalID(r.Context(), orgID, userID)
	if err != nil {
		internalError(w, "settings", fmt.Errorf("prepare bedrock external id: %w", err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"caller_identity_arn": callerARN,
		"external_id":         externalID,
		"trust_policy":        bedrockTrustPolicyJSON(callerARN, externalID),
	})
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

// externalIDGenMu serializes the read-generate-write of an org's External ID
// on this process. The generation is a read-modify-write over the secret bag
// with no atomic insert-if-absent primitive, and re-reading inside the same
// transaction only sees that transaction's own write (READ COMMITTED) — so
// without this lock two concurrent role-setup requests both observe empty,
// both write, and the response can hand back an id that lost the last-writer
// race, breaking the "stable thereafter" guarantee. A keyed lock (per org)
// makes same-pod generation single-flight; the connect probe re-reads the
// stored value regardless, so the confused-deputy defense holds even in the
// (extraordinarily rare) cross-pod simultaneous-setup case, which self-heals
// on the next role-setup fetch. It's held across a fast, rare DB tx — role
// mode is configured once per org.
var externalIDGenMu keyedMutex

// resolveExternalID reads (generating + persisting if absent) the org's
// stable External ID under the per-org generation lock — the single entry
// point both the role-setup endpoint and the connect probe use so a
// concurrent double-submit can't mint two.
func (se *settingsHandler) resolveExternalID(ctx context.Context, orgID, userID string) (string, error) {
	unlock := externalIDGenMu.lock(orgID)
	defer unlock()
	var id string
	err := se.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		id, e = ensureBedrockExternalID(ctx, tx, orgID)
		return e
	})
	return id, err
}

// ensureBedrockExternalID reads the org's stored External ID, generating and
// persisting a fresh random one if absent (idempotent; stable thereafter —
// decision 3, the confused-deputy defense). Runs inside the caller's tx and
// under the per-org generation lock (resolveExternalID).
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
// attaches to their IAM role: it allows the control service's caller to
// AssumeRole only when the request carries the TF-generated External ID. This
// snippet IS the setup documentation — no prose walkthrough.
func bedrockTrustPolicyJSON(callerARN, externalID string) string {
	policy := map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{{
			"Effect":    "Allow",
			"Principal": map[string]any{"AWS": trustPolicyPrincipalARN(callerARN)},
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

// trustPolicyPrincipalARN turns the caller's identity ARN into a value valid
// as a trust-policy Principal. When the control service runs under an assumed
// role — an EC2 instance profile, ECS task role, or EKS IRSA, i.e. the
// recommended setup — sts:GetCallerIdentity returns an STS *session* ARN
//
//	arn:<partition>:sts::<account>:assumed-role/<RoleName>/<session>
//
// which is NOT a valid Principal; a trust policy must name the underlying IAM
// role
//
//	arn:<partition>:iam::<account>:role/<RoleName>
//
// so we rewrite it. An IAM user/role ARN (or anything unrecognized) is already
// valid and passes through unchanged. Note: a role with an IAM path loses that
// path in the session ARN, so the rewritten Principal drops it too — rare for
// instance/IRSA roles, and the customer can add the path back if their role
// uses one (the caller_identity_arn field shows the real identity).
func trustPolicyPrincipalARN(callerARN string) string {
	parts := strings.SplitN(callerARN, ":", 6)
	if len(parts) < 6 || parts[2] != "sts" {
		return callerARN
	}
	role, ok := strings.CutPrefix(parts[5], "assumed-role/")
	if !ok {
		return callerARN
	}
	roleName, _, _ := strings.Cut(role, "/")
	if roleName == "" {
		return callerARN
	}
	return fmt.Sprintf("arn:%s:iam::%s:role/%s", parts[1], parts[4], roleName)
}

// keyedMutex is a small per-key lock: lock(k) returns an unlock func; two
// callers with the same key serialize while different keys proceed
// concurrently. Entries are never pruned — the key space is org ids in one
// deployment (bounded and tiny), and the lock is only taken during Bedrock
// role-mode setup.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*sync.Mutex{}
	}
	m, ok := k.locks[key]
	if !ok {
		m = &sync.Mutex{}
		k.locks[key] = m
	}
	k.mu.Unlock()
	m.Lock()
	return m.Unlock
}
