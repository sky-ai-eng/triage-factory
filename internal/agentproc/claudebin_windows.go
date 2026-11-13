//go:build windows

package agentproc

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

// checkClaudeBinaryRunnable accepts what the SDK can launch on Windows: a
// regular file that is a native executable (.exe) or a JavaScript entry point
// (.js) run through node. Windows files carry no execute permission bit, so
// the extension is the only signal of what a file is.
func checkClaudeBinaryRunnable(raw, abs string, info fs.FileInfo) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s=%q is not a regular file", envClaudeBinary, raw)
	}
	switch strings.ToLower(filepath.Ext(abs)) {
	case ".exe", ".js":
		return nil
	}
	return fmt.Errorf("%s=%q is not a Claude binary: on Windows it must be an .exe or .js file", envClaudeBinary, raw)
}
