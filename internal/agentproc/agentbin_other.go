//go:build !windows

package agentproc

// localAgentBinary is the name the agent uses for the TF CLI outside a
// sandbox: the host binary's own path, which the agent's shell executes as is.
func localAgentBinary(hostBin string) string { return hostBin }

// AgentVisiblePath is the spelling of a host path in text shown to the agent.
// A path the agent's shell and file tools take as is needs no rewrite here.
func AgentVisiblePath(p string) string { return p }

// withAgentBinOnPath leaves env as it is: the agent reaches the TF binary by
// its own path, so no PATH entry is needed.
func withAgentBinOnPath(env []string) ([]string, error) { return env, nil }

// samePathEntry reports whether two cleaned PATH entries name the same
// directory. File names are case-sensitive here.
func samePathEntry(a, b string) bool { return a == b }
