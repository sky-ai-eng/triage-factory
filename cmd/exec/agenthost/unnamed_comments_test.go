package agenthost

import (
	"context"
	"maps"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// unnamedComments points the package's unnamed-comment counter at a manual
// reader for the test and returns a reader of every series it recorded, keyed
// "provider/action".
func unnamedComments(t *testing.T) func() map[string]int64 {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	prev := auditUnnamedComments
	auditUnnamedComments = newUnnamedCommentCounter(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { auditUnnamedComments = prev })
	return func() map[string]int64 {
		t.Helper()
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect metrics: %v", err)
		}
		out := map[string]int64{}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != "audit.comments.unnamed" {
					continue
				}
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("audit.comments.unnamed is %T, want an int64 sum", m.Data)
				}
				for _, dp := range sum.DataPoints {
					provider, _ := dp.Attributes.Value(attribute.Key("provider"))
					action, _ := dp.Attributes.Value(attribute.Key("action"))
					out[provider.AsString()+"/"+action.AsString()] += dp.Value
				}
			}
		}
		return out
	}
}

// TestRecordExternalWrite_CountsAPostedCommentNamingNoObject: a posted comment
// whose row names no comment is counted under its provider and action. One
// that names its comment, and an unnamed row of another action, count nothing.
func TestRecordExternalWrite_CountsAPostedCommentNamingNoObject(t *testing.T) {
	counted := unnamedComments(t)
	stores, info := newCaptureStores(t, true)
	ctx := context.Background()

	named := &domain.ExternalAction{
		Provider: domain.ProviderGitHub, Action: domain.ActionCommentPosted,
		Target: "acme/widgets#7", ExternalID: "77", Credential: domain.CredentialGitHubApp,
	}
	named.SetObject(domain.ObjectKindComment, domain.GitHubCommentKey(domain.GitHubIssueComment, 77))
	RecordExternalWrite(ctx, stores, info, nil, named)
	RecordExternalWrite(ctx, stores, info, nil, &domain.ExternalAction{
		Provider: domain.ProviderGitHub, Action: domain.ActionCommentPosted,
		Target: "acme/widgets#7", Credential: domain.CredentialGitHubApp,
	})
	RecordExternalWrite(ctx, stores, info, nil, &domain.ExternalAction{
		Provider: domain.ProviderJira, Action: domain.ActionIssueCommentPosted,
		Target: "SKY-1", Credential: domain.CredentialJiraOrg,
	})
	RecordExternalWrite(ctx, stores, info, nil, &domain.ExternalAction{
		Provider: domain.ProviderGitHub, Action: domain.ActionGHChannelWrite,
		Target: "acme/widgets", Credential: domain.CredentialGitHubApp,
	})

	if acts := listExternalActions(t, stores); len(acts) != 4 {
		t.Fatalf("want all four rows written, got %d", len(acts))
	}
	want := map[string]int64{"github/comment_posted": 1, "jira/issue_comment_posted": 1}
	if got := counted(); !maps.Equal(got, want) {
		t.Errorf("unnamed comments = %v, want %v", got, want)
	}
}

// TestRecordExternalWrite_UnnamedCommentThatFailsToLandCountsNothing: the
// counter counts rows in the ledger, so a row the write lost is the drop
// counter's alone.
func TestRecordExternalWrite_UnnamedCommentThatFailsToLandCountsNothing(t *testing.T) {
	counted := unnamedComments(t)
	conn, stores, info := newCaptureStoresConn(t, true)
	if err := conn.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	RecordExternalWrite(context.Background(), stores, info, nil, &domain.ExternalAction{
		Provider: domain.ProviderGitHub, Action: domain.ActionCommentPosted,
		Target: "acme/widgets#7", Credential: domain.CredentialGitHubApp,
	})

	if got := counted(); len(got) != 0 {
		t.Errorf("unnamed comments = %v, want none for a row that never landed", got)
	}
}
