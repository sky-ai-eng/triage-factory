package bindaddr

import "testing"

func TestRequireLoopback(t *testing.T) {
	for addr, wantOK := range map[string]bool{
		"127.0.0.1:0":   true,
		"[::1]:8080":    true,
		"localhost:0":   true,
		":8080":         false,
		"0.0.0.0:8080":  false,
		"[::]:8080":     false,
		"10.0.0.5:8080": false,
		"no-port":       false,
	} {
		if err := RequireLoopback(addr); (err == nil) != wantOK {
			t.Errorf("RequireLoopback(%q) = %v, want ok=%v", addr, err, wantOK)
		}
	}
}
