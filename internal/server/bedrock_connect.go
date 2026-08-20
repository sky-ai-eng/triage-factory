package server

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
)

// The Bedrock vocabulary and validators shared by the three flavor routes in
// llm_credentials.go, which own the writes.

// bedrockAuthMethodBearer / bedrockAuthMethodAccessKeys / bedrockAuthMethodRole
// name the three shapes an org's Bedrock access can take. They are the values
// the settings read reports in bedrock_auth_method and the last segment of each
// flavor's bind route; the stored marker
// (org_settings.bedrock_credentials_ref) is the primary secret's key name, like
// AnthropicAPIKeyRef.
const (
	bedrockAuthMethodBearer     = "bearer"
	bedrockAuthMethodAccessKeys = "access_keys"
	// bedrockAuthMethodRole is the short-lived-credential method (TFAC-616):
	// the org stores no Bedrock secret at all — only the customer role ARN
	// and a TF-generated External ID — and the brain mints STS session creds
	// per run. It is the only Bedrock method with a live connect-time probe.
	bedrockAuthMethodRole = "role"
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
