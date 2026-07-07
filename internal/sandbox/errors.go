package sandbox

import (
	"errors"
	"fmt"
)

// ErrUnsupportedPlatform is returned by Wrap when running on an OS
// that doesn't support gVisor (i.e. anything other than Linux).
// agentproc.Run gates on runmode.ModeMulti && GOOS == "linux" so this
// error should never surface in production; it exists for the
// non-Linux stub so misconfigured callers fail loudly instead of
// silently routing through the direct-subprocess path.
var ErrUnsupportedPlatform = errors.New("sandbox: gVisor sandboxing requires Linux")

// ErrRunscMissing is returned when exec.LookPath("runsc") fails.
// Surfaced with an install hint so deployment-time misconfiguration
// is obvious. The TF runner image bundles runsc; self-host
// customers install via the gVisor release bundle.
var ErrRunscMissing = errors.New("sandbox: runsc binary not found on PATH — install gVisor from gvisor.dev/releases")

// ErrSubnetsExhausted is returned when the MaxSandboxes-slot per-process
// subnet allocator is full. Each active sandbox holds one /24 from
// 10.42.0.0/16. The default per-replica concurrency cap is well
// below MaxSandboxes; hitting this means either a runaway spawn loop or
// missing Close() calls leaking netns + bundle dirs.
var ErrSubnetsExhausted = fmt.Errorf("sandbox: subnet allocator exhausted (%d concurrent runs is the per-process cap)", MaxSandboxes)
