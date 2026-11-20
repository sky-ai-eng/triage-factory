// Package containertest holds the one rule every container-backed test
// harness uses to decide whether its suite runs or skips.
//
// The harnesses boot Linux images (Postgres, SeaweedFS). A Docker daemon
// that cannot run them is an environment without the dependency, the same
// as no daemon at all, so the suite skips. Nothing else skips: a daemon
// that can run Linux images but fails to boot one is a regression, and a
// skip would hide it behind a green, empty run.
package containertest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
)

// SkipUnlessLinuxDaemon skips t unless the Docker daemon testcontainers
// would use answers and can run Linux containers. A Windows daemon in
// Windows-container mode answers the connection probe and then cannot
// pull a Linux image, so the daemon's OS type is checked as well.
func SkipUnlessLinuxDaemon(t testing.TB) {
	t.Helper()
	osType, err := daemonOSType(context.Background())
	if reason := skipReason(osType, err); reason != "" {
		t.Skip(reason)
	}
}

// skipReason decides from the daemon probe's result whether a
// container-backed test skips, and says why. Empty means it runs.
//
// Only an OS type the daemon names, and that is not Linux, skips. A daemon
// that leaves the field empty gets the boot attempt, because skipping on an
// unknown answer is how a working environment would turn into an empty run.
func skipReason(osType string, probeErr error) string {
	if probeErr != nil {
		return fmt.Sprintf("Docker is not reachable, so the container-backed tests skip: %v", probeErr)
	}
	if osType != "" && !strings.EqualFold(osType, "linux") {
		return fmt.Sprintf("the Docker daemon runs %q containers, not Linux ones, so the container-backed tests skip (switch the daemon to Linux containers to run them)", osType)
	}
	return ""
}

// daemonOSType asks the daemon testcontainers would use for its OS type,
// from the same info call that tells whether it answers at all. A panic
// while building the client is a Docker host this process cannot use, so
// it skips like an unreachable daemon rather than crashing the test binary.
func daemonOSType(ctx context.Context) (osType string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("docker client: %v", r)
		}
	}()
	provider, err := testcontainers.NewDockerProvider()
	if err != nil {
		return "", err
	}
	defer func() { _ = provider.Close() }()
	info, err := provider.Client().Info(ctx, client.InfoOptions{})
	if err != nil {
		return "", err
	}
	return info.Info.OSType, nil
}
