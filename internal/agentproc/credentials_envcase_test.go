package agentproc

import (
	"slices"
	"strings"
	"testing"
)

// On Windows an environment name is the same variable whatever its case, so a
// resolved credential must also clear an operator's lower- or mixed-case
// spelling of every credential name. Otherwise "anthropic_base_url" survives the
// guard and the child sends the org's stored key to the operator's gateway.
func TestMergeEnv_FoldsCaseWhereTheOSDoes(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin",
		"anthropic_base_url=https://operator-gateway.example",
		"Anthropic_Auth_Token=operator-bearer",
		"aws_profile=operator-account",
	}
	creds := map[string]string{"ANTHROPIC_API_KEY": "org-stored-key"}

	for _, tc := range []struct {
		fold bool
		want []string
	}{
		{fold: true, want: []string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=org-stored-key"}},
		// POSIX keeps names distinct, so a lower-case name is another variable
		// the child never reads as a credential, and it is left alone.
		{fold: false, want: append(slices.Clone(parent), "ANTHROPIC_API_KEY=org-stored-key")},
	} {
		prev := envNamesFoldCase
		envNamesFoldCase = tc.fold
		got := mergeEnv(parent, nil, creds)
		envNamesFoldCase = prev
		if !slices.Equal(got, tc.want) {
			t.Errorf("fold=%v: mergeEnv = %v, want %v", tc.fold, got, tc.want)
		}
	}
}

// filterEnv folds its own list as well as the environment, so a mixed-case
// entry on the list (BUN_JSC_useJIT) still matches every spelling on Windows.
func TestFilterEnv_FoldsBothSidesWhereTheOSDoes(t *testing.T) {
	prev := envNamesFoldCase
	envNamesFoldCase = true
	t.Cleanup(func() { envNamesFoldCase = prev })

	got := filterEnv([]string{"bun_jsc_usejit=1", "BUN_JSC_useJIT=1", "KEEP=1"}, []string{jscJITEnvKey})
	if len(got) != 1 || !strings.HasPrefix(got[0], "KEEP=") {
		t.Errorf("filterEnv = %v, want only KEEP", got)
	}
}
