package containertest

import (
	"errors"
	"strings"
	"testing"
)

func TestSkipReason(t *testing.T) {
	cases := []struct {
		name     string
		osType   string
		probeErr error
		// want is a substring of the skip reason; empty means the test runs.
		want string
	}{
		{name: "linux daemon runs", osType: "linux"},
		{name: "linux in another case runs", osType: "Linux"},
		{name: "daemon with no OS type runs", osType: ""},
		{name: "windows-container daemon skips", osType: "windows", want: `runs "windows" containers`},
		{name: "unreachable daemon skips", probeErr: errors.New("dial unix /var/run/docker.sock: connect: no such file or directory"), want: "Docker is not reachable"},
		{name: "probe error outranks a reported OS type", osType: "linux", probeErr: errors.New("boom"), want: "Docker is not reachable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := skipReason(tc.osType, tc.probeErr)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("skipReason(%q, %v) = %q, want the test to run", tc.osType, tc.probeErr, got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("skipReason(%q, %v) = %q, want it to contain %q", tc.osType, tc.probeErr, got, tc.want)
			}
		})
	}
}
