package runmode

// SandboxMarkerEnvVar / SandboxMarkerEnvValue are the marker every sandboxed
// agent process carries. Two writers set it: the jail's environment
// (cell.JailEnv) and the local sandbox's spawn (internal/agentproc). It
// answers exactly one question for code running inside: am I in a jail?
//
// It has to be an explicit marker rather than a heuristic over the rest of
// the env. Every other variable a jailed process can observe — the proxy
// pairs, the git-hooks bin, an unset TF_MODE — has a legitimate non-sandbox
// configuration, so only a variable whose writers are the sandboxes
// themselves can be trusted as the answer. Read it by exact match against
// SandboxMarkerEnvValue; anything else means "not in a jail".
//
// The binary's boot-identity resolution (bootident.go) is the reader: with
// the marker set, a CLI invocation is the jailed CLI — a pure RPC client that
// opens no database and fails closed when the exec-verb socket is absent.
// Non-credential by construction. The direct (unsandboxed) spawn strips any
// inherited copy, so the marker can only ever mean what its writers meant by
// it.
const (
	SandboxMarkerEnvVar   = "TRIAGE_FACTORY_SANDBOXED"
	SandboxMarkerEnvValue = "1"
)
