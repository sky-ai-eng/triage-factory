//go:build !windows

package agentproc

import (
	"fmt"
	"io/fs"
)

// checkClaudeBinaryRunnable requires an execute bit, the only signal unix
// files carry of being launchable.
func checkClaudeBinaryRunnable(raw, _ string, info fs.FileInfo) error {
	if info.Mode()&0o111 == 0 {
		return fmt.Errorf("%s=%q is not executable", envClaudeBinary, raw)
	}
	return nil
}
