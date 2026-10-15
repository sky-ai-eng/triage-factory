package sandbox

// The trusted-mount classification for the native runtime's tool-host socket.
//
// Producer and validator sit on opposite sides of this one, as they do for the
// step-skill and entity-memory mounts: the orchestrator binds the listener
// inside the per-run directory before the launch RPC, and the broker VALIDATES
// a launch's claimed source against its own derivation rather than overriding
// it. Both read the derivation below, so neither can drift.
//
// It lives outside launchspec_linux.go for that reason. The broker only runs on
// Linux, but the producing side (internal/agentproc's prepareToolHostSocket) is
// ordinary cross-platform code, so a linux-tagged derivation compiles for the
// validator and leaves the producer referencing a symbol that does not exist —
// breaking every non-Linux build of the module.

// TrustedToolHostSocketDirDestination is the fixed in-sandbox directory the
// per-run tool-host socket directory is bind-mounted at. The in-jail server
// connects to the socket inside it; the directory's mode, not the mount, is
// what keeps the jail from creating anything there.
//
// A directory rather than a socket file, unlike the agenthost mount, because
// the listener is on the far side here: the agenthost socket exists host-side
// before launch and is mounted as a file, while the tool host's socket does not
// exist until the jailed process binds it, so what crosses the boundary is the
// directory it appears in.
const TrustedToolHostSocketDirDestination = "/run/tf-tools"

// TrustedToolHostSocketDir returns the host path of conversationID's own tool-host
// socket directory — what the broker creates for each engagement
// (PrepareRunSockets), owned by the orchestrator with the run's jail gid as its
// setgid group, what the orchestrator binds the tool host's socket inside, and
// what the broker requires a TrustedToolHostSocketDirDestination mount's source
// to resolve to.
//
// A sibling of RunSocketDir rather than a directory inside it, because the two
// have different owners: the sidecar serves the agenthost socket and the
// orchestrator serves this one, and neither may write in the other's.
func TrustedToolHostSocketDir(conversationID string) string {
	return RunSocketDir(conversationID) + toolHostSocketDirSuffix
}
