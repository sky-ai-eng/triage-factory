package commitsigner

import (
	"context"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// CheckVerify is the check an org admin runs with Verify: is the signer's key
// on its account, and does the commit that broke it verify now.
const CheckVerify = "verify"

// The check results that are not a verification reason GitHub gave.
const (
	// CheckResultKeyMissing: the key was not on the account.
	CheckResultKeyMissing = domain.CommitSignerReasonKeyMissing
	// CheckResultError: GitHub could not be read.
	CheckResultError = "error"
	// checkResultOther stands in for any verification reason not named in
	// checkResults, so a reason GitHub adds cannot mint series.
	checkResultOther = "other"
)

// checkResults are the verification reasons a check result keeps as they are.
var checkResults = map[string]bool{
	domain.CommitVerificationValid: true,
	"unknown_key":                  true,
	"no_user":                      true,
	"unverified_email":             true,
	CheckResultKeyMissing:          true,
	CheckResultError:               true,
}

// checkStats owns the health-check counter. Package-level because a metric
// instrument is process-global by nature, and swapped atomically so a test can
// back it with a manual reader.
var checkStats atomic.Pointer[stats]

func init() {
	checkStats.Store(newStats(otel.GetMeterProvider()))
}

type stats struct {
	checks metric.Int64Counter
}

// newStats builds the instrument against mp — production passes the global
// provider, which telemetry.Init installs. An instrument-creation error can
// only be a programmer error, and the API hands back a usable no-op alongside
// it, so it is logged rather than propagated.
func newStats(mp metric.MeterProvider) *stats {
	counter, err := mp.Meter("internal/commitsigner").Int64Counter("commit_signer.checks",
		metric.WithDescription("Commit signer health checks, by check and result."))
	if err != nil {
		signerLog.Warn("commit signer check counter setup failed", "error", err)
	}
	return &stats{checks: counter}
}

// CountCheck counts one run of check that ended in result: GitHub's
// verification reason, CheckResultKeyMissing or CheckResultError. A reason
// outside the ones the metric names is counted as other.
func CountCheck(ctx context.Context, check, result string) {
	s := checkStats.Load()
	if s == nil || s.checks == nil {
		return
	}
	if !checkResults[result] {
		result = checkResultOther
	}
	s.checks.Add(ctx, 1, metric.WithAttributes(
		attribute.String("check", check), attribute.String("result", result)))
}

// TestT is the minimal slice of *testing.T that SetMeterProviderForTest needs,
// so the standard library's testing package stays out of production builds.
type TestT interface {
	Helper()
	Cleanup(func())
}

// SetMeterProviderForTest points the check counter at provider for the
// duration of t and restores the previous one via t.Cleanup. It lives in a
// non-_test.go file so the packages that run checks can read what their tests
// recorded. It swaps a process global, so tests that call it must not run in
// parallel with each other.
func SetMeterProviderForTest(t TestT, provider metric.MeterProvider) {
	t.Helper()
	prev := checkStats.Swap(newStats(provider))
	t.Cleanup(func() { checkStats.Store(prev) })
}
