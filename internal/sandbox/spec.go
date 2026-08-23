package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// WorktreeUID is the UID Wrap configures process.user to run as
// inside the sandbox. The caller MUST chown the worktree to this
// UID before invoking Wrap, or the agent's writes will fail with
// EACCES. Exported as a const so callers (delegate spawner, etc.)
// chown deterministically.
const (
	WorktreeUID = 10000
	WorktreeGID = 10000
)

// defaultRlimits is the fixed resource shape applied when a launch names
// none. Kept here (not inlined in buildSpec) so both the default path and
// the validation of a caller-supplied set reference one source of truth.
func defaultRlimits() []specs.POSIXRlimit {
	return []specs.POSIXRlimit{
		{Type: "RLIMIT_NOFILE", Hard: 1024, Soft: 1024},
		{Type: "RLIMIT_NPROC", Hard: 512, Soft: 512},
	}
}

// buildSpec constructs the full OCI runtime spec for one sandbox
// invocation. Returns *specs.Spec rather than the marshaled JSON so
// the bundle writer can hash / validate / golden-test the structure.
//
// Property B invariant: spec.Process.Env is set ONLY from cfg.Env.
// No call to os.Environ, no reading of credential-shaped variables,
// no merge with anything else. The caller's slice is the entire
// environment the sandboxed agent sees.
//
// netnsPath must be a bind-mount of an existing netns at e.g.
// /var/run/netns/tf-<id>. The sandbox spec references it via
// linux.namespaces[].path so runsc joins the namespace instead of
// creating its own (which would isolate from the host-side veth
// we just set up).
func buildSpec(cfg Config, netnsPath string) (*specs.Spec, error) {
	if cfg.ConversationID == "" {
		return nil, fmt.Errorf("spec: Config.ConversationID is required")
	}
	if cfg.Worktree == "" {
		return nil, fmt.Errorf("spec: Config.Worktree is required")
	}
	if len(cfg.Argv) == 0 {
		return nil, fmt.Errorf("spec: Config.Argv is required")
	}
	if netnsPath == "" {
		return nil, fmt.Errorf("spec: netnsPath is required")
	}

	noNewPrivs := true
	// Rootfs MUST be readonly: the cached alpine extraction is shared
	// across every run on the host, so a writable mount would let one
	// tenant's agent persist files into /usr/bin/* (or anywhere in the
	// rootfs) that the next tenant's agent then reads/executes. The
	// places the agent legitimately needs to write — /work (the bind-
	// mounted worktree), /tmp (a per-run tmpfs), /dev (a per-run
	// tmpfs) — are already separate mounts and stay writable
	// regardless of this flag.
	readonlyRootfs := true

	spec := &specs.Spec{
		Version: specs.Version,
		Process: &specs.Process{
			Terminal: false,
			User: specs.User{
				UID: WorktreeUID,
				GID: WorktreeGID,
			},
			Args: cfg.Argv,
			// PROPERTY B INVARIANT: Env is cfg.Env verbatim. No
			// os.Environ read, no credential injection. Caller owns
			// what the agent sees.
			Env:             append([]string(nil), cfg.Env...),
			Cwd:             "/work",
			NoNewPrivileges: noNewPrivs,
			// All four capability slices empty: bounding, effective,
			// permitted, inheritable, ambient. The agent runs with
			// zero capabilities — load-bearing for T3 hardening
			// (RCE-in-SDK can't escalate within the sandbox).
			Capabilities: &specs.LinuxCapabilities{
				Bounding:    []string{},
				Effective:   []string{},
				Permitted:   []string{},
				Inheritable: []string{},
				Ambient:     []string{},
			},
			Rlimits: rlimitsForSpec(cfg.Rlimits),
		},
		Root: &specs.Root{
			Path:     "rootfs",
			Readonly: readonlyRootfs,
		},
		Hostname: "tf-sandbox",
		Mounts: append([]specs.Mount{
			// Standard /proc, /dev, /sys, /dev/pts — runsc applies
			// defaults but we set them explicitly so the spec is
			// self-documenting.
			{Destination: "/proc", Type: "proc", Source: "proc"},
			{Destination: "/dev", Type: "tmpfs", Source: "tmpfs",
				Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
			{Destination: "/dev/pts", Type: "devpts", Source: "devpts",
				Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"}},
			{Destination: "/sys", Type: "sysfs", Source: "sysfs",
				Options: []string{"nosuid", "noexec", "nodev", "ro"}},
			{Destination: "/tmp", Type: "tmpfs", Source: "tmpfs",
				Options: []string{"nosuid", "nodev", "mode=1777"}},

			// Agent's worktree — bind-mount RW so the agent can edit
			// files. Caller must have chowned this to WorktreeUID
			// before Wrap was called.
			{Destination: "/work", Type: "bind", Source: cfg.Worktree,
				Options: []string{"rbind", "rw"}},

			// CA certificates from the host so outbound TLS (to the
			// proxy, to git/Anthropic upstream) verifies.
			// /etc/ssl/certs is the conventional Debian/Alpine path;
			// most distros symlink it.
			{Destination: "/etc/ssl/certs", Type: "bind",
				Source: hostSSLCertsDir(), Options: []string{"rbind", "ro"}},
		}, mountsFromExtra(cfg.ExtraMounts)...),
		Linux: &specs.Linux{
			Namespaces: []specs.LinuxNamespace{
				{Type: specs.PIDNamespace},
				{Type: specs.IPCNamespace},
				{Type: specs.UTSNamespace},
				{Type: specs.MountNamespace},
				// Network namespace — pre-created with the veth pair
				// before Wrap returns. runsc joins instead of creating
				// because we need host-side iptables to NAT the traffic.
				{Type: specs.NetworkNamespace, Path: netnsPath},
			},
			MaskedPaths: []string{
				"/proc/acpi",
				"/proc/asound",
				"/proc/kcore",
				"/proc/keys",
				"/proc/latency_stats",
				"/proc/timer_list",
				"/proc/timer_stats",
				"/proc/sched_debug",
				"/sys/firmware",
			},
			ReadonlyPaths: []string{
				"/proc/bus",
				"/proc/fs",
				"/proc/irq",
				"/proc/sys",
				"/proc/sysrq-trigger",
			},
			Seccomp: defaultSeccompProfile(),
		},
	}

	// Resolv.conf bind mount is the synthesized one in the bundle
	// dir. We don't add it here because the path depends on bundleDir
	// which is constructed by writeBundle; bundle.go appends this
	// mount after calling buildSpec.

	return spec, nil
}

// rlimitsForSpec maps the caller's numeric Rlimit set onto the OCI
// spec type, falling back to defaultRlimits when the caller named none.
// The values reach here having already passed validateRlimits at the
// broker's RPC boundary (the gate in ValidateLaunchParams).
func rlimitsForSpec(rl []Rlimit) []specs.POSIXRlimit {
	if len(rl) == 0 {
		return defaultRlimits()
	}
	out := make([]specs.POSIXRlimit, 0, len(rl))
	for _, r := range rl {
		out = append(out, specs.POSIXRlimit{Type: r.Type, Soft: r.Soft, Hard: r.Hard})
	}
	return out
}

// mountsFromExtra converts the public Mount type to specs.Mount.
// Two-step so the public type doesn't leak the runtime-spec import
// onto callers (keeps the package's public surface small).
func mountsFromExtra(extra []Mount) []specs.Mount {
	out := make([]specs.Mount, 0, len(extra))
	for _, m := range extra {
		out = append(out, specs.Mount{
			Destination: m.Destination,
			Source:      m.Source,
			Type:        "bind",
			Options:     append([]string{"rbind"}, m.Options...),
		})
	}
	return out
}

// hostSSLCertsDir returns the path to the host's CA bundle. Tries
// the conventional paths and falls back to /etc/ssl/certs (works
// on Debian/Alpine; macOS dev doesn't matter because the sandbox
// only runs on Linux). Returning a path that doesn't exist makes
// runsc fail with a clear bind-mount error rather than silently
// running without TLS roots.
func hostSSLCertsDir() string {
	candidates := []string{
		"/etc/ssl/certs",       // Debian, Alpine, most distros
		"/etc/pki/tls/certs",   // RHEL/CentOS
		"/etc/ca-certificates", // some embedded distros
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "/etc/ssl/certs"
}

// defaultSeccompProfile returns the OCI-standard "default action
// SCMP_ACT_ERRNO, allow the safe set" profile.
//
// NOTE — not enforced under gVisor. runsc does NOT apply the OCI
// container seccomp profile to the sandboxed application (validated:
// docs/for-agents/specs/playwright-chromium-sandbox/probe-seccomp.sh — a
// KILL_PROCESS default had zero effect, and KILL can't be shadowed by
// systrap's interception trap). gVisor's design makes the Sentry the
// syscall boundary: the app's syscalls are serviced in user space and
// never reach the host kernel, and gVisor's own internal seccomp
// confines the Sentry's host syscalls. So this profile is inert in the
// gVisor path. We still emit it (rather than leaving Seccomp nil) for
// OCI completeness and so the spec is honest about intent — but it is
// defense-in-depth for a hypothetical non-gVisor runtime, NOT the
// enforced in-sandbox control. See defaultAllowedSyscalls for detail.
func defaultSeccompProfile() *specs.LinuxSeccomp {
	return &specs.LinuxSeccomp{
		DefaultAction: specs.ActErrno,
		Architectures: []specs.Arch{specs.ArchX86_64, specs.ArchX86, specs.ArchAARCH64},
		Syscalls: []specs.LinuxSyscall{
			// Permissive baseline: allow the syscalls a typical Node
			// process needs (file I/O, network, threads, etc). This
			// is essentially Docker's default seccomp set minus the
			// truly dangerous ones (mount, reboot, kexec_load, etc).
			{
				Names:  defaultAllowedSyscalls,
				Action: specs.ActAllow,
			},
		},
	}
}

// MarshalSpec serializes the OCI spec to the JSON shape runsc reads.
// Exposed for tests + bundle writer; production code goes through
// writeBundle which calls this internally.
func MarshalSpec(spec *specs.Spec) ([]byte, error) {
	return json.MarshalIndent(spec, "", "  ")
}

// specOnDisk writes the spec JSON to bundleDir/config.json. Called
// from writeBundle; separate function so spec_test.go can golden-
// file the JSON without exercising the bundle writer.
func specOnDisk(spec *specs.Spec, bundleDir string) error {
	data, err := MarshalSpec(spec)
	if err != nil {
		return fmt.Errorf("spec: marshal: %w", err)
	}
	return os.WriteFile(filepath.Join(bundleDir, "config.json"), data, 0o644)
}
