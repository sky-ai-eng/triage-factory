// Package sandbox isolates one agent run inside gVisor (runsc).
//
// In multi mode on Linux, internal/agentproc.Run wraps the Node
// subprocess via Wrap() so the agent sees only its worktree, a
// scratched-from-empty env, and a netns with no route to host
// services.
//
// The capabilities that build this cell (netns, veth, iptables, cgroup,
// and the runsc launch) are held by a separate cap-broker process —
// never by the orchestrator that resolves credentials and parses agent
// output. This package builds the OCI spec from a fixed template (empty
// caps, uid 10000, seccomp) and validates the narrow, numeric parameters
// the broker accepts over its RPC, so a compromised orchestrator cannot
// steer sandbox construction. The deployment threat model and that split
// live in docs/security/security-overview.md; the validated runsc/egress
// mechanics that this package mirrors in Go live in
// docs/for-agents/specs/sky-254-runsc-validation/.
//
// # Property B invariant
//
// Wrap does NOT inject credentials into the sandbox env. The caller
// passes Config.Env verbatim — sandbox does not read os.Environ for
// ANTHROPIC_*/AWS_*/GitHub PATs, does not invoke
// agentproc.resolveCredentials, and does not propagate the parent
// process's env in any form. A jailbroken agent dumping its own
// /proc/self/environ finds only what the caller put there.
//
// The caller's env is intentionally credential-free. Proxy configuration
// layers proxy URLs + placeholder credentials on top via the
// ConfigureProxies callback: after the netns + veth are up but
// before the OCI bundle is written, the caller binds a per-run LLM
// proxy on Sandbox.HostIP and returns ANTHROPIC_BASE_URL +
// placeholder env entries. The real credential lives in the proxy
// process on the host; the sandbox env carries only the proxy URL.
//
// # Threat model (T1–T4)
//
// T1: credential exfiltration — addressed by Property B above.
// T2: in-run credential misuse — strongly bounded: Property B keeps
//
//	every real credential out of the sandbox, so the agent can reach
//	one only through the host-side proxy/exec interface that mediates
//	it, within the run's own authority and bounded by run wall-clock.
//	See docs/security/security-overview.md §2.
//
// T3: RCE in the agent SDK escaping the SDK process — addressed by
//
//	gVisor + in-sandbox hardening (non-root UID, dropped caps,
//	seccomp, noNewPrivileges).
//
// T4: RCE escaping gVisor to the host kernel — addressed by gVisor's
//
//	own user-mode kernel architecture. Load-bearing reason we use
//	gVisor at all.
//
// Local mode collapses T1/T2/T4 (single-tenant); T3 still applies as
// defense in depth. The Linux + ModeMulti gate in agentproc.Run
// skips this whole package for local installs.
package sandbox
