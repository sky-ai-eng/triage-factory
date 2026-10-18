//go:build linux

package gvisor

import (
	"slices"
	"testing"
)

func TestGatewayPorts(t *testing.T) {
	const gw = "10.42.7.1"
	got, err := gatewayPorts(gw, []string{"10.42.7.1:443", "10.42.7.1:34567"})
	if err != nil {
		t.Fatalf("gatewayPorts: %v", err)
	}
	if want := []int{443, 34567}; !slices.Equal(got, want) {
		t.Errorf("gatewayPorts = %v, want %v", got, want)
	}

	for _, bad := range []string{
		"10.42.3.1:443",   // another cell's gateway
		"10.42.7.10:443",  // not the gateway, though it starts with it
		"127.0.0.1:9464",  // the host, not the gateway
		"10.42.7.1",       // no port
		"10.42.7.1:https", // a port name
	} {
		if _, err := gatewayPorts(gw, []string{bad}); err == nil {
			t.Errorf("gatewayPorts accepted %q", bad)
		}
	}
}
