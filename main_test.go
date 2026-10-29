package main

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestCheckDefaultGitHubHostForSignIn pins the boot refusal: multi mode passes
// TF_DEFAULT_GITHUB_HOST to GoTrue, which builds a wrong API URL for an
// explicit github.com, so multi refuses every spelling of it (case, trailing
// slash, scheme, port) and accepts unset, an Enterprise Server and a GHE.com
// tenant. Local mode has no GoTrue and accepts all of them.
func TestCheckDefaultGitHubHostForSignIn(t *testing.T) {
	tests := []struct {
		mode    runmode.Mode
		raw     string
		wantErr bool
	}{
		{runmode.ModeMulti, "", false},
		{runmode.ModeMulti, "   ", false},
		{runmode.ModeMulti, "https://ghe.example.com", false},
		{runmode.ModeMulti, "https://github.com", true},
		{runmode.ModeMulti, "https://GitHub.com/", true},
		{runmode.ModeMulti, "http://github.com", true},
		{runmode.ModeMulti, "https://github.com:443", true},
		{runmode.ModeMulti, "https://acme.ghe.com", false},
		{runmode.ModeLocal, "https://github.com", false},
		{runmode.ModeLocal, "", false},
	}
	for _, tc := range tests {
		err := checkDefaultGitHubHostForSignIn(tc.mode, tc.raw)
		if (err != nil) != tc.wantErr {
			t.Errorf("checkDefaultGitHubHostForSignIn(%s, %q) = %v, want error: %v", tc.mode, tc.raw, err, tc.wantErr)
		}
	}
}
