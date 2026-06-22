package server

import "testing"

// TestEmailDomain covers the exact-domain extraction the discovery endpoint
// routes on. emailDomain stays in core (sso_seam_helpers.go) — the ExtensionAPI
// delegates to it — so its test stays here. The ssoStartURL / discover-handler
// tests moved to ee/sso with the handlers (see ee/sso/funcs_test.go).
func TestEmailDomain(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{
		{"plain", "user@corp.com", "corp.com", true},
		{"uppercased", "Alice@Corp.Com", "corp.com", true},
		{"surrounding whitespace", "  bob@corp.com\t", "corp.com", true},
		{"trailing fqdn dot", "x@corp.com.", "corp.com", true},
		{"multiple trailing dots", "x@corp.com..", "corp.com", true},
		{"subdomain kept whole", "alice@eng.corp.com", "eng.corp.com", true},
		{"plus tag in local part", "user+tag@corp.com", "corp.com", true},

		{"empty", "", "", false},
		{"no at", "not-an-email", "", false},
		{"empty local part", "@corp.com", "", false},
		{"empty domain", "user@", "", false},
		{"trailing-dot-only domain", "user@.", "", false},
		{"two ats", "a@b@corp.com", "", false},
		{"interior whitespace in domain", "user@cor p.com", "", false},
		{"single label localhost", "user@localhost", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := emailDomain(tc.in)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("emailDomain(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
