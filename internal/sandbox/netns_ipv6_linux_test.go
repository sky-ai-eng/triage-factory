//go:build linux

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDisableIPv6On(t *testing.T) {
	t.Run("kernel_without_ipv6", func(t *testing.T) {
		if err := disableIPv6On(t.TempDir(), "vh-abcd1"); err != nil {
			t.Fatalf("a kernel with no IPv6 has nothing to disable, got %v", err)
		}
	})

	t.Run("writes_the_interface_switch", func(t *testing.T) {
		root := t.TempDir()
		conf := filepath.Join(root, "sys/net/ipv6/conf/vh-abcd1")
		if err := os.MkdirAll(conf, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(conf, "disable_ipv6"), []byte("0\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := disableIPv6On(root, "vh-abcd1"); err != nil {
			t.Fatalf("disableIPv6On: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(conf, "disable_ipv6"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(got)) != "1" {
			t.Errorf("disable_ipv6 = %q, want 1", got)
		}
	})

	t.Run("missing_interface_fails", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "sys/net/ipv6/conf"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := disableIPv6On(root, "vh-abcd1"); err == nil {
			t.Error("disabling IPv6 on an interface the kernel does not have succeeded")
		}
		if _, err := os.Stat(filepath.Join(root, "sys/net/ipv6/conf/vh-abcd1")); err == nil {
			t.Error("disableIPv6On created the interface's sysctl directory")
		}
	})
}
