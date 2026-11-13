package worktree

import (
	"strconv"
	"strings"
)

// layerGitEnv returns env with pairs appended as git env-config entries at the
// next free GIT_CONFIG indices (one past the inherited GIT_CONFIG_COUNT, the
// inherited COUNT line dropped and re-emitted bumped), and with each vars entry
// (KEY=value) replacing any inherited value of its key. With neither pairs nor
// vars it returns env unchanged, which is what keeps non-Windows environments
// byte-identical.
func layerGitEnv(env []string, pairs [][2]string, vars []string) []string {
	if len(pairs) == 0 && len(vars) == 0 {
		return env
	}
	idx := gitConfigCount(env)
	out := make([]string, 0, len(env)+1+2*len(pairs)+len(vars))
	for _, kv := range env {
		if len(pairs) > 0 && strings.HasPrefix(kv, "GIT_CONFIG_COUNT=") {
			continue // re-emitted below, bumped to include our entries
		}
		if envKeyIn(kv, vars) {
			continue
		}
		out = append(out, kv)
	}
	for i, p := range pairs {
		n := strconv.Itoa(idx + i)
		out = append(out, "GIT_CONFIG_KEY_"+n+"="+p[0], "GIT_CONFIG_VALUE_"+n+"="+p[1])
	}
	if len(pairs) > 0 {
		out = append(out, "GIT_CONFIG_COUNT="+strconv.Itoa(idx+len(pairs)))
	}
	return append(out, vars...)
}

// envKeyIn reports whether kv ("KEY=value") has the same KEY as any entry of vars.
func envKeyIn(kv string, vars []string) bool {
	key, _, _ := strings.Cut(kv, "=")
	for _, v := range vars {
		if vk, _, _ := strings.Cut(v, "="); vk == key {
			return true
		}
	}
	return false
}
