package app

import (
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
)

// TestBuildClaims_LossBudget pins TF_MAX_CLAIM_ATTEMPTS's parse: empty is the
// default, a positive integer is taken as given, and anything else refuses
// to boot rather than running with a budget nobody chose.
func TestBuildClaims_LossBudget(t *testing.T) {
	cases := []struct {
		name, raw   string
		wantRefused bool
	}{
		{name: "default", raw: ""},
		{name: "explicit", raw: "4"},
		{name: "padded", raw: " 3 "},
		{name: "zero", raw: "0", wantRefused: true},
		{name: "negative", raw: "-1", wantRefused: true},
		{name: "not a number", raw: "two", wantRefused: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &App{spawner: delegate.NewSpawner(nil, db.Stores{}, nil, nil, "")}
			t.Setenv("TF_MAX_CLAIM_ATTEMPTS", tc.raw)

			err := a.buildClaims()
			if !tc.wantRefused {
				if err != nil {
					t.Fatalf("buildClaims = %v, want a clean boot", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("buildClaims accepted TF_MAX_CLAIM_ATTEMPTS=%q", tc.raw)
			}
			if !strings.Contains(err.Error(), "TF_MAX_CLAIM_ATTEMPTS") {
				t.Errorf("refusal %q does not name the setting", err)
			}
		})
	}
}

func TestParseMaxClaimLosses_Values(t *testing.T) {
	if n, err := delegate.ParseMaxClaimLosses(""); err != nil || n != delegate.DefaultMaxClaimLosses {
		t.Errorf("ParseMaxClaimLosses(\"\") = (%d, %v), want (%d, nil)", n, err, delegate.DefaultMaxClaimLosses)
	}
	if n, err := delegate.ParseMaxClaimLosses("5"); err != nil || n != 5 {
		t.Errorf("ParseMaxClaimLosses(\"5\") = (%d, %v), want (5, nil)", n, err)
	}
}
