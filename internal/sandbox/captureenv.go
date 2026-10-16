package sandbox

import "os"

// CaptureJailEnv is the whole environment of the capture jail. Exported, and
// platform-neutral though only Linux launches the jail, because it is the
// contract between the two halves of the capture: the broker that sets it and
// the capture body that runs under it, whose own tests run git under exactly
// this environment rather than a stand-in that drifts from it. It names no
// credential and no proxy: the jail has no network to use one on, and no file
// outside the read-only tree, the TF binary and the root filesystem.
//
// Config is refused from every source but the checkout's own repository
// config: GIT_CONFIG_GLOBAL and GIT_CONFIG_SYSTEM point at the null device and
// HOME at a path that does not exist, so nothing resolves through a user or
// system file. The remaining overrides neutralize the attribute-free keys that
// run a program, as defense in depth. The boundary is the jail: a clean or
// process filter the agent wrote into the checkout's own config does run, and
// runs here, with no network, no write access to the tree, and nothing of any
// other run in view. GIT_NO_LAZY_FETCH keeps a missing object from becoming a
// fetch through a remote the agent configured.
func CaptureJailEnv() []string {
	return []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/nonexistent",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_SYSTEM=" + os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_NO_LAZY_FETCH=1",
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=core.fsmonitor", "GIT_CONFIG_VALUE_0=",
		// diff.external is a tripwire, not the protection: git has no value
		// meaning "disabled" for it, so a diff that reaches this key without
		// --no-ext-diff fails loudly instead of quietly running the agent's
		// program. The flags at the invocation are what protect the capture.
		"GIT_CONFIG_KEY_1=diff.external", "GIT_CONFIG_VALUE_1=",
	}
}
