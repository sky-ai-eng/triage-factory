package llmcred

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultTTL is the requested lifetime of a minted STS session credential
// when TF_LLM_CRED_TTL_SEC is unset — the AWS sts:AssumeRole default and a
// sane one-hour ceiling on a stolen-bundle's usefulness.
const DefaultTTL = time.Hour

// MinTTL is the floor TF_LLM_CRED_TTL_SEC clamps up to. AWS's own
// AssumeRole minimum is 900s; a shorter request is rejected, so we clamp
// rather than forward an always-failing duration.
const MinTTL = 15 * time.Minute

// MaxTTL is the ceiling TF_LLM_CRED_TTL_SEC clamps down to. The feature's
// whole security value is a short-lived credential — the blast radius of an
// exfiltrated session cred is "under an hour" — so TF caps the requested
// AssumeRole duration at one hour regardless of the operator's value or the
// role's higher MaxSessionDuration. (AWS still caps below this when the
// role's own max is shorter, surfaced as an actionable AssumeRole error.)
const MaxTTL = time.Hour

// NetworkBinding is the executor egress binding minted into the session
// policy of executor-bound (bundle) role mints: an exfiltrated session
// credential is unusable from anywhere but the executor's egress. Both
// fields are optional (unset → no network condition). Brain-bound mints
// never carry this — they execute from the control-process egress.
type NetworkBinding struct {
	// SourceCIDRs feeds aws:SourceIp (TF_EXECUTOR_EGRESS_CIDRS).
	SourceCIDRs []string
	// VPCEIDs feeds aws:SourceVpce (TF_EXECUTOR_VPCE_IDS).
	VPCEIDs []string
}

// Empty reports whether the binding carries no network condition.
func (b NetworkBinding) Empty() bool {
	return len(b.SourceCIDRs) == 0 && len(b.VPCEIDs) == 0
}

// TTLFromEnv resolves the mint TTL from TF_LLM_CRED_TTL_SEC. Empty →
// DefaultTTL; a positive integer number of seconds otherwise, clamped up to
// MinTTL (AWS rejects a shorter AssumeRole duration). A non-integer or
// non-positive value is a boot-time error.
func TTLFromEnv() (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv("TF_LLM_CRED_TTL_SEC"))
	if raw == "" {
		return DefaultTTL, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid TF_LLM_CRED_TTL_SEC=%q (want a positive integer number of seconds)", raw)
	}
	ttl := time.Duration(n) * time.Second
	if ttl < MinTTL {
		ttl = MinTTL
	}
	if ttl > MaxTTL {
		ttl = MaxTTL
	}
	return ttl, nil
}

// NetworkBindingFromEnv resolves the executor egress binding from
// TF_EXECUTOR_EGRESS_CIDRS (comma-separated CIDRs, each validated) and
// TF_EXECUTOR_VPCE_IDS (comma-separated VPC endpoint ids). Both optional;
// an empty binding means role mints carry no network condition. A
// malformed CIDR is a boot-time error so a typo doesn't silently ship an
// unbound session credential.
func NetworkBindingFromEnv() (NetworkBinding, error) {
	var b NetworkBinding
	for _, cidr := range splitList(os.Getenv("TF_EXECUTOR_EGRESS_CIDRS")) {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return NetworkBinding{}, fmt.Errorf("invalid TF_EXECUTOR_EGRESS_CIDRS entry %q (want CIDR notation, e.g. 203.0.113.0/24): %w", cidr, err)
		}
		b.SourceCIDRs = append(b.SourceCIDRs, cidr)
	}
	for _, id := range splitList(os.Getenv("TF_EXECUTOR_VPCE_IDS")) {
		if !strings.HasPrefix(id, "vpce-") {
			return NetworkBinding{}, fmt.Errorf("invalid TF_EXECUTOR_VPCE_IDS entry %q (want a VPC endpoint id, e.g. vpce-0123456789abcdef0)", id)
		}
		b.VPCEIDs = append(b.VPCEIDs, id)
	}
	return b, nil
}

func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
