//go:build windows

package app

// noteNoLocalSandbox says once, at boot, that agent runs on Windows have no
// isolation beyond the user account TF runs as. The Linux sandbox is
// bubblewrap, which does not exist here, so nothing can be unavailable at
// run time that was promised at boot: this line is the whole disclosure, and
// it is at WARN so it shows at the default log level and is not read as routine
// startup noise.
func noteNoLocalSandbox() {
	appLog.Warn("local agent sandbox not available on Windows; agent runs have the full access of this user account",
		"isolation", "none")
}
