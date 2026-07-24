package agentproc

// sandboxTFBinary is the canonical in-sandbox path the host TF binary
// is bind-mounted at. Picked once and used everywhere — BuildAllowedTools
// embeds this path in its `Bash(<selfBin> exec *)` pattern, and the agent
// invokes it under the same path. Anything outside this constant would
// break the per-tool path check Claude Code applies before exec.
//
// The agenthost socket lives at /run/tf.sock; that constant is owned by
// cmd/exec/agenthost (DefaultSocketPath) so the agent-side AutoDetect
// and the spawner-side mount registration agree by construction. The
// binary path is owned here because it's an agentproc-internal
// concern — the allowlist patterns that name it are constructed in
// agentproc.BuildAllowedTools.
//
// Defined without a build tag so the constant is reachable from
// agentproc.Run's sandbox-branch code on every platform's build —
// shouldSandbox gates the runtime use to Linux, but the package still
// has to compile on Darwin dev boxes.
const sandboxTFBinary = "/usr/local/bin/triagefactory"

// sandboxTFACBinary is the in-sandbox path the TF binary is ALSO bind-mounted
// at, under the additive `tfac` applet name (TFAC-669). Same source file as
// sandboxTFBinary; argv0 dispatch (main.go) makes `tfac <verb>` mean
// `triagefactory exec <verb>`. A PATH-leading dir (/opt/tf/bin) so it resolves
// ahead of a custom profile's own binaries. Mirrored broker-side as
// sandbox.TrustedTFACBinaryDestination.
const sandboxTFACBinary = "/opt/tf/bin/tfac"

// sandboxGHBinary is the in-sandbox path the TF-pinned real `gh` release binary
// is bind-mounted at, on a PATH-leading directory (/opt/tf/bin) so it wins the
// PATH race against any gh a custom profile image ships (TFAC-408) — and theirs
// would have no credentials anyway. Mirrored broker-side as
// sandbox.TrustedGHBinaryDestination; a drift test cross-checks the two.
const sandboxGHBinary = "/opt/tf/bin/gh"

// sandboxGHInjectorCert is the in-sandbox path the per-run gh-injector TLS
// certificate is bind-mounted at (read-only). SSL_CERT_FILE points gh here so
// it trusts the injector's per-run self-signed leaf — the sole root gh sees, no
// rootfs trust-store edit. Mirrored broker-side as
// sandbox.TrustedGHInjectorCertDestination.
const sandboxGHInjectorCert = "/run/tf-gh-injector.crt"

// hostGHBinaryPath is the executor-host path the pinned gh binary is baked at
// (docker/Dockerfile installs it here); it is both the mount source and the
// jail destination. A var, not a const, so tests can point it at a stub or an
// empty path (a run whose host has no baked gh simply mounts no gh — the
// os.Stat guard skips it, exactly like the git-hooks dir).
var hostGHBinaryPath = "/opt/tf/bin/gh"
