// Package containertest holds the one rule every container-backed test
// harness uses to decide whether its suite runs or skips.
//
// The harnesses boot Linux images (Postgres, SeaweedFS). A Docker daemon
// that cannot run them is an environment without the dependency, the same
// as no daemon at all, so the suite skips. A Linux daemon whose container
// then fails to boot is a regression the harness reports as a failure;
// that decision belongs to the harness, after this check.
package containertest

import (
	"context"
	"fmt"
	"testing"

	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
)

// SkipUnlessLinuxDaemon skips t unless the Docker daemon testcontainers
// would use answers and reports that it runs Linux containers. A Windows
// daemon in Windows-container mode answers the health probe and then
// cannot pull a Linux image, so the OS type is checked as well as the
// connection.
func SkipUnlessLinuxDaemon(t testing.TB) {
	t.Helper()
	osType, err := daemonOSType(context.Background())
	if reason := skipReason(osType, err); reason != "" {
		t.Skip(reason)
	}
}

// skipReason decides from the daemon probe's result whether a
// container-backed test skips, and says why. Empty means it runs.
func skipReason(osType string, probeErr error) string {
	if probeErr != nil {
		return fmt.Sprintf("Docker is not reachable, so the container-backed tests skip: %v", probeErr)
	}
	if osType != "linux" {
		return fmt.Sprintf("the Docker daemon runs %q containers, not Linux ones, so the container-backed tests skip (on Windows, switch Docker to Linux containers to run them)", osType)
	}
	return ""
}

// daemonOSType asks the daemon testcontainers would use for its OS type.
// The info call is the health probe as well, so one round trip decides
// both skip cases. A panic while building the client counts as an
// unreachable daemon: it means no Docker host could be resolved.
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
