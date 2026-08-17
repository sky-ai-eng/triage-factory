package llmcred

import (
	"encoding/json"
	"strings"
)

// The two Bedrock runtime actions the per-run LLM proxy actually calls
// upstream: InvokeModel (buffered) and InvokeModelWithResponseStream
// (streaming responses). Verified against internal/llmproxy/sigv4.go — the
// proxy re-signs and forwards exactly these; it makes no control-plane
// Bedrock calls. The inline session policy grants only these, so a stolen
// session credential can do nothing but invoke the org's model.
const (
	actionInvokeModel             = "bedrock:InvokeModel"
	actionInvokeModelWithStream   = "bedrock:InvokeModelWithResponseStream"
	sessionPolicyVersion          = "2012-10-17"
	sessionPolicyResourceWildcard = "*"
)

// sessionPolicyStatement / sessionPolicy are the minimal JSON shapes AWS's
// inline session policy accepts. Built by hand (not a struct-heavy policy
// library) because the shape is fixed and small; marshaling a typed value
// keeps the output stable and quotes correct.
type sessionPolicyStatement struct {
	Effect    string         `json:"Effect"`
	Action    []string       `json:"Action"`
	Resource  []string       `json:"Resource"`
	Condition map[string]any `json:"Condition,omitempty"`
}

type sessionPolicy struct {
	Version   string                   `json:"Version"`
	Statement []sessionPolicyStatement `json:"Statement"`
}

// buildSessionPolicy renders the inline session policy for an AssumeRole
// mint. It always scopes the session to the two invoke actions; the
// resource is the org's configured model/inference-profile ARN when one is
// pinned (tightest), else "*" (still action-scoped). When netCondition is
// non-nil the Allow carries it — the network binding that makes an
// exfiltrated session credential unusable off the executor's egress.
//
// modelID is the org's bedrock_model_id. Only an ARN ("arn:...") narrows
// the resource — a bare model id (e.g. "anthropic.claude-...") can't be
// turned into a precise ARN without the account, so it falls back to "*"
// (the action scope is the real ceiling either way).
func buildSessionPolicy(modelID string, netCondition map[string]any) string {
	resources := []string{sessionPolicyResourceWildcard}
	if arn := strings.TrimSpace(modelID); strings.HasPrefix(arn, "arn:") {
		resources = []string{arn}
	}
	pol := sessionPolicy{
		Version: sessionPolicyVersion,
		Statement: []sessionPolicyStatement{{
			Effect:    "Allow",
			Action:    []string{actionInvokeModel, actionInvokeModelWithStream},
			Resource:  resources,
			Condition: netCondition,
		}},
	}
	// json.Marshal of this fixed shape cannot fail.
	b, _ := json.Marshal(pol)
	return string(b)
}

// networkCondition builds the aws:SourceIp / aws:SourceVpce condition block
// from the executor egress binding. Returns nil when the binding is empty
// (no condition) — which is ALSO what brain-bound mints always pass, since
// they execute from the control-process egress and must not be network-
// bound. Both operators are ANDed when both are set (a deployment that
// reaches Bedrock through a VPC endpoint from a known IP range); the common
// case sets exactly one.
func networkCondition(b NetworkBinding) map[string]any {
	if len(b.SourceCIDRs) == 0 && len(b.VPCEIDs) == 0 {
		return nil
	}
	cond := map[string]any{}
	if len(b.SourceCIDRs) > 0 {
		cond["IpAddress"] = map[string]any{"aws:SourceIp": b.SourceCIDRs}
	}
	if len(b.VPCEIDs) > 0 {
		cond["StringEquals"] = map[string]any{"aws:SourceVpce": b.VPCEIDs}
	}
	return cond
}

// sessionNameForConversation turns a run id into a valid RoleSessionName: AWS
// restricts it to 2–64 chars of [\w+=,.@-]. A run id (a uuid) already
// satisfies the charset; the "tf-" prefix marks the session as TF-minted in
// the customer's CloudTrail. Truncated to 64 for safety against non-uuid ids.
func sessionNameForConversation(conversationID string) string {
	return clampSessionName("tf-" + sanitizeSessionName(conversationID))
}

func sanitizeSessionName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' || r == '=' || r == ',' || r == '.' || r == '@' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

func clampSessionName(s string) string {
	if len(s) > 64 {
		return s[:64]
	}
	if s == "" {
		return "tf-run"
	}
	return s
}
