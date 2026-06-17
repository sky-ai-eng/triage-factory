package sessions

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The AES-GCM primitive (Encrypt/Decrypt/LoadKeyFromEnv) now lives in
// internal/aead and is tested there. sessions keeps only the
// session-specific LogID helper.

func TestLogID_IsStableAndShort(t *testing.T) {
	id := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	got := LogID(id)
	if len(got) != 8 {
		t.Errorf("LogID len=%d, want 8 (4 hex bytes)", len(got))
	}
	// Same input → same prefix.
	if LogID(id) != got {
		t.Error("LogID not deterministic for same uuid")
	}
	// Different input → (almost certainly) different prefix.
	other := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	if LogID(other) == got {
		t.Error("LogID collision on two distinct uuids — suspicious")
	}
	// Sanity: doesn't leak the raw uuid string.
	if strings.Contains(got, id.String()[:8]) {
		t.Errorf("LogID %q contains prefix of raw uuid — should be a hash, not a slice", got)
	}
}
