package slack

import "testing"

// TestInferTransport covers the fixed rule table from TFAC-529's "Decisions
// already made": credential shape alone decides the transport except when
// both credentials are present, in which case an explicit choice is
// required.
func TestInferTransport(t *testing.T) {
	cases := []struct {
		name                          string
		hasSigningSecret, hasAppToken bool
		explicit                      string
		want                          string
		wantErr                       bool
	}{
		{"app token only -> socket", false, true, "", transportSocket, false},
		{"signing secret only -> events_api", true, false, "", transportEventsAPI, false},
		{"both + explicit socket -> socket", true, true, "socket", transportSocket, false},
		{"both + explicit events_api -> events_api", true, true, "events_api", transportEventsAPI, false},
		{"both + no explicit -> error", true, true, "", "", true},
		{"both + garbage explicit -> error", true, true, "carrier-pigeon", "", true},
		{"neither -> error", false, false, "", "", true},
		{"app token only ignores contradicting explicit", false, true, "events_api", transportSocket, false},
		{"signing secret only ignores contradicting explicit", true, false, "socket", transportEventsAPI, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := inferTransport(tc.hasSigningSecret, tc.hasAppToken, tc.explicit)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("inferTransport(%v, %v, %q) = (%q, nil); want an error", tc.hasSigningSecret, tc.hasAppToken, tc.explicit, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("inferTransport(%v, %v, %q) unexpected error: %v", tc.hasSigningSecret, tc.hasAppToken, tc.explicit, err)
			}
			if got != tc.want {
				t.Errorf("inferTransport(%v, %v, %q) = %q; want %q", tc.hasSigningSecret, tc.hasAppToken, tc.explicit, got, tc.want)
			}
		})
	}
}

// TestMergeCredentialPresence covers the "leave blank to keep current"
// merge rule standalone: a fresh submission always wins and is never
// flagged "keep" (it must be re-Put); a blank submission falls through to
// whatever the org already has stored.
func TestMergeCredentialPresence(t *testing.T) {
	cases := []struct {
		name                     string
		submitted, alreadyStored bool
		wantHas, wantKeep        bool
	}{
		{"submitted, none stored", true, false, true, false},
		{"submitted, already stored (re-Put wins)", true, true, true, false},
		{"blank, already stored (kept)", false, true, true, true},
		{"blank, nothing stored", false, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			has, keep := mergeCredentialPresence(tc.submitted, tc.alreadyStored)
			if has != tc.wantHas || keep != tc.wantKeep {
				t.Errorf("mergeCredentialPresence(%v, %v) = (%v, %v); want (%v, %v)",
					tc.submitted, tc.alreadyStored, has, keep, tc.wantHas, tc.wantKeep)
			}
		})
	}
}
