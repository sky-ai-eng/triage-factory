package worktree

import (
	"slices"
	"testing"
)

func TestLayerGitEnv_NoPairsNoVarsIsIdentity(t *testing.T) {
	env := []string{"PATH=/bin", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=a.b", "GIT_CONFIG_VALUE_0=c"}
	if got := layerGitEnv(env, nil, nil); !slices.Equal(got, env) {
		t.Errorf("layerGitEnv with nothing to add = %v, want %v", got, env)
	}
}

func TestLayerGitEnv_AppendsAtNextFreeIndexAndReplacesVars(t *testing.T) {
	env := []string{
		"PATH=/bin",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://h/.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: x",
		"GCM_INTERACTIVE=always",
	}
	got := layerGitEnv(env, [][2]string{{"core.longpaths", "true"}, {"credential.helper", ""}}, []string{"GCM_INTERACTIVE=never"})
	want := []string{
		"PATH=/bin",
		"GIT_CONFIG_KEY_0=http.https://h/.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: x",
		"GIT_CONFIG_KEY_1=core.longpaths", "GIT_CONFIG_VALUE_1=true",
		"GIT_CONFIG_KEY_2=credential.helper", "GIT_CONFIG_VALUE_2=",
		"GIT_CONFIG_COUNT=3",
		"GCM_INTERACTIVE=never",
	}
	if !slices.Equal(got, want) {
		t.Errorf("layerGitEnv =\n%v\nwant\n%v", got, want)
	}
}

// gitBaseEnv carries the platform's pairs, and the http extraHeader
// gitConfigEnviron adds afterwards lands after them without clobbering.
func TestGitBaseEnv_PlatformPairsComposeWithCloneAuth(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "")
	base := gitBaseEnv()
	n := len(platformGitConfigPairs())
	if n == 0 {
		if gitConfigCount(base) != 0 {
			t.Errorf("GIT_CONFIG_COUNT = %d off Windows, want unset", gitConfigCount(base))
		}
		return
	}
	if got := gitConfigCount(base); got != n {
		t.Fatalf("gitBaseEnv GIT_CONFIG_COUNT = %d, want %d", got, n)
	}
	if !slices.Contains(base, "GIT_CONFIG_KEY_0=core.longpaths") || !slices.Contains(base, "GIT_CONFIG_VALUE_0=true") {
		t.Errorf("core.longpaths missing from gitBaseEnv: %v", base)
	}
	if !slices.Contains(base, "GIT_CONFIG_KEY_1=credential.helper") || !slices.Contains(base, "GIT_CONFIG_VALUE_1=") {
		t.Errorf("empty credential.helper missing from gitBaseEnv: %v", base)
	}
	if !slices.Contains(base, "GCM_INTERACTIVE=never") {
		t.Errorf("GCM_INTERACTIVE=never missing from gitBaseEnv: %v", base)
	}
	auth := CloneAuthFor("https://github.com/", "tok")
	env, ok := auth.gitConfigEnviron(base)
	if !ok {
		t.Fatal("active CloneAuth produced no env")
	}
	if got := gitConfigCount(env); got != n+1 {
		t.Errorf("GIT_CONFIG_COUNT with extraHeader = %d, want %d", got, n+1)
	}
}
