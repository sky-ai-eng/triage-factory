//go:build !windows

package agentproc

// useArgsFile is false where the kernel's argument limit (hundreds of KB to
// MB) is far above anything a run passes, so the wrapper keeps getting its
// flags on the command line.
const useArgsFile = false
