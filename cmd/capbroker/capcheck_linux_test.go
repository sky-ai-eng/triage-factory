//go:build linux

package capbroker

import (
	"strings"
	"testing"
)

// A broker without CAP_SYS_RESOURCE would start and then fail every launch,
// because runsc refuses a spec that lowers the runtime's OOM score; it is
// refused at boot instead, naming the capability.
func TestRequireSysResource(t *testing.T) {
	if err := requireSysResource([]string{"cap_net_admin", "cap_sys_admin", "cap_sys_resource"}); err != nil {
		t.Errorf("a broker holding CAP_SYS_RESOURCE was refused: %v", err)
	}
	err := requireSysResource([]string{"cap_net_admin", "cap_sys_admin"})
	if err == nil {
		t.Fatal("a broker without CAP_SYS_RESOURCE was accepted")
	}
	if !strings.Contains(err.Error(), "SYS_RESOURCE") {
		t.Errorf("the refusal does not name the capability: %v", err)
	}
}
