package commitsigner

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Budgets are the two limits on how many commit signatures TF makes: per org
// over any OrgWindow, and per claim in total. They bound what one runaway or
// compromised run, or one org, can get signed. A rebase is the case they are
// sized for: it re-signs every commit it rewrites, so a long branch is
// hundreds of signatures in seconds where a typical run makes a handful.
//
// Both are counted over commit_signatures (CommitSignatureStore's
// OrgSignaturesSinceSystem and ClaimSignaturesSystem), so no counter is
// stored, and both are read where signatures are made: the local process,
// and every control pod in multi mode.
type Budgets struct {
	// PerOrgMinute is the most signatures one org may be given in any
	// OrgWindow. TF_MAX_COMMIT_SIGNATURES_PER_ORG_MINUTE.
	PerOrgMinute int
	// PerClaim is the most signatures one claim may be given; once spent,
	// signing is off for the rest of that claim.
	// TF_MAX_COMMIT_SIGNATURES_PER_CLAIM.
	PerClaim int
}

// OrgWindow is the span the per-org budget counts over.
const OrgWindow = time.Minute

const (
	// DefaultMaxSignaturesPerOrgMinute and DefaultMaxSignaturesPerClaim are
	// the budgets with neither setting set.
	DefaultMaxSignaturesPerOrgMinute = 600
	DefaultMaxSignaturesPerClaim     = 5000

	envMaxPerOrgMinute = "TF_MAX_COMMIT_SIGNATURES_PER_ORG_MINUTE"
	envMaxPerClaim     = "TF_MAX_COMMIT_SIGNATURES_PER_CLAIM"
)

// ParseBudgets reads both budgets through getenv. Unset or blank is the
// default; anything else must be a positive integer, and is refused rather
// than replaced by the default, because a limit nobody can see is in force is
// no limit to reason about.
func ParseBudgets(getenv func(string) string) (Budgets, error) {
	perOrg, err := parseBudget(getenv, envMaxPerOrgMinute, DefaultMaxSignaturesPerOrgMinute)
	if err != nil {
		return Budgets{}, err
	}
	perClaim, err := parseBudget(getenv, envMaxPerClaim, DefaultMaxSignaturesPerClaim)
	if err != nil {
		return Budgets{}, err
	}
	return Budgets{PerOrgMinute: perOrg, PerClaim: perClaim}, nil
}

func parseBudget(getenv func(string) string, name string, def int) (int, error) {
	raw := getenv(name)
	s := strings.TrimSpace(raw)
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid %s=%q (want a positive integer)", name, raw)
	}
	return n, nil
}
