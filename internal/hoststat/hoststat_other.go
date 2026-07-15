//go:build !linux

package hoststat

import (
	"context"
	"time"
)

// read returns an empty Sample on non-Linux hosts — no /proc to diff. The
// sampler writes NULL cpu/load, which is honest ("not observed on this
// platform").
func read(_ context.Context, _ time.Duration) Sample { return Sample{} }

// oomKills is Linux-cgroup-only; elsewhere the count is Unknown.
func oomKills() int { return Unknown }
