package promptseed

import (
	"bytes"
	"testing"
)

// Embedded text must be byte-identical on every platform; a CR means a checkout converted line endings.
func TestEmbeddedText_NoCR(t *testing.T) {
	t.Parallel()
	embedded := map[string][]byte{
		"pr-review-security":    []byte(prReviewSecurity),
		"pr-review-correctness": []byte(prReviewCorrectness),
		"pr-review-aggregate":   []byte(prReviewAggregate),
		"pr-security-review":    []byte(prSecurityReview),
		"pr-correctness-review": []byte(prCorrectnessReview),
		"issue-implement":       []byte(issueImplement),
		"conflict-resolution":   []byte(conflictResolution),
		"ci-fix":                []byte(ciFix),
		"fix-review-feedback":   []byte(fixReviewFeedback),
	}
	for name, b := range embedded {
		if bytes.IndexByte(b, '\r') >= 0 {
			t.Errorf("embedded %s contains a carriage return", name)
		}
	}
}
