//go:build windows

package worktree

import (
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

// longTempDir is os.TempDir() in its long-name form. TEMP on a Windows account
// whose name is longer than eight characters is commonly the 8.3 short form
// (`C:\Users\ERIC~1.ARN\AppData\Local\Temp`), while Claude Code realpaths its
// cwd to the long form. Handing the agent the long form up front keeps the cwd
// TF passes and the path the session directory is named from in agreement.
func longTempDir() string { return longPathName(os.TempDir()) }

// longPathName resolves p through GetLongPathName. On any error p is returned
// unchanged: a short name is still a working path.
func longPathName(p string) string {
	in, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return p
	}
	n, err := windows.GetLongPathName(in, nil, 0)
	if err != nil || n == 0 {
		return p
	}
	buf := make([]uint16, n)
	n, err = windows.GetLongPathName(in, &buf[0], n)
	if err != nil || n == 0 || n > uint32(len(buf)) {
		return p
	}
	return windows.UTF16ToString(buf[:n])
}

// sameFileName compares two file names the way NTFS does by default: ignoring
// case.
func sameFileName(a, b string) bool { return strings.EqualFold(a, b) }
