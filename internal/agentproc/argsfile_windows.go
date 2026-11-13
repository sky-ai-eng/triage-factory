//go:build windows

package agentproc

// useArgsFile is true where the OS caps a command line below what a run's
// arguments need. CreateProcess refuses more than 32,767 characters, and the
// system prompt, tool allowlist and tool schemas of an ordinary run already
// come to 22-40 KB before Windows argument quoting grows them.
const useArgsFile = true
