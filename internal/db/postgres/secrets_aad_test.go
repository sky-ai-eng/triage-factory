package postgres

// Internal (white-box) tests for the row-identity AAD binding.
// These exercise the unexported secretAAD directly so the core security
// property — a ciphertext is non-portable across row identities — has
// runnable coverage even where Docker (and thus the pgtest relocation
// tests) is unavailable.

import (
	"bytes"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/aead"
)

func mustAEADKey(t *testing.T) aead.Key {
	t.Helper()
	var k aead.Key
	if _, err := rand.Read(k[:]); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	return k
}

// TestSecretAAD_Distinct pins that every component of the row identity
// changes the derived AAD: no two distinct (org, user, key) rows share a
// binding, so a blob can't silently decrypt in a sibling row.
func TestSecretAAD_Distinct(t *testing.T) {
	orgA := uuid.New().String()
	orgB := uuid.New().String()
	userA := uuid.New().String()
	userB := uuid.New().String()

	aadOrgA, err := secretAAD(orgA, "", "k")
	if err != nil {
		t.Fatal(err)
	}
	aadUserA, err := secretAAD(orgA, userA, "k")
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string][]byte{
		"different org":                mustAAD(t, orgB, "", "k"),
		"org-scope vs user-scope":      aadUserA,
		"different user":               mustAAD(t, orgA, userB, "k"),
		"different key (org scope)":    mustAAD(t, orgA, "", "k2"),
		"different key (user scope)":   mustAAD(t, orgA, userA, "k2"),
		"org-scope different org+user": mustAAD(t, orgB, userB, "k"),
	}
	base := map[string][]byte{
		"different org":                aadOrgA,
		"org-scope vs user-scope":      aadOrgA,
		"different user":               aadUserA,
		"different key (org scope)":    aadOrgA,
		"different key (user scope)":   aadUserA,
		"org-scope different org+user": aadOrgA,
	}
	for name, other := range cases {
		if bytes.Equal(base[name], other) {
			t.Errorf("%s: distinct identities produced equal AAD", name)
		}
	}
}

// TestSecretAAD_NormalizesUUIDFormatting guards Put/Get consistency: the
// UUIDs are parsed to their canonical 16-byte form, so a write and a read
// that spell the same UUID differently (case, braces, no-dash) still land
// on byte-identical AAD and decrypt. Without normalization a formatting
// difference between two call sites would silently break decryption.
func TestSecretAAD_NormalizesUUIDFormatting(t *testing.T) {
	org := uuid.New()
	user := uuid.New()

	canon := mustAAD(t, org.String(), user.String(), "key")
	upper := mustAAD(t, strings.ToUpper(org.String()), strings.ToUpper(user.String()), "key")
	if !bytes.Equal(canon, upper) {
		t.Error("AAD differs by UUID letter-case; Put/Get would diverge on formatting")
	}
	braced := mustAAD(t, "{"+org.String()+"}", "{"+user.String()+"}", "key")
	if !bytes.Equal(canon, braced) {
		t.Error("AAD differs by UUID brace formatting")
	}
	noDash := mustAAD(t, strings.ReplaceAll(org.String(), "-", ""), strings.ReplaceAll(user.String(), "-", ""), "key")
	if !bytes.Equal(canon, noDash) {
		t.Error("AAD differs by UUID dash formatting")
	}
}

// TestSecretAAD_OrgScopeSentinel pins both halves of the sentinel
// invariant: org scope (empty userID) binds to the all-zero UUID — the
// same value org_secrets_scope_key_uniq's COALESCE uses — AND the all-zero
// UUID is rejected as an explicit per-user id, so the sentinel stays
// unambiguous and a per-user row can never alias org-scope semantics.
func TestSecretAAD_OrgScopeSentinel(t *testing.T) {
	org := uuid.New()

	// Org scope: the user segment (bytes 16..32) is the all-zero sentinel.
	orgScope := mustAAD(t, org.String(), "", "key")
	if userSeg := orgScope[16:32]; !bytes.Equal(userSeg, make([]byte, 16)) {
		t.Errorf("org-scope user segment = %x, want 16 zero bytes (the COALESCE sentinel)", userSeg)
	}

	// The all-zero UUID must NOT be accepted as an explicit per-user id —
	// otherwise a (org, 0000…, key) row would share the org-scope AAD.
	for _, nilForm := range []string{
		"00000000-0000-0000-0000-000000000000",
		uuid.Nil.String(),
		"{00000000-0000-0000-0000-000000000000}",
	} {
		if _, err := secretAAD(org.String(), nilForm, "key"); err == nil {
			t.Errorf("secretAAD accepted all-zero user_id %q; it must be reserved for the org-scope sentinel", nilForm)
		}
	}
}

// TestSecretAAD_Layout pins the wire layout — orgUUID(16) || userUUID(16)
// || key — so the two fixed-width prefixes keep the encoding unambiguous
// (a change here would risk a collision class).
func TestSecretAAD_Layout(t *testing.T) {
	org := uuid.New()
	user := uuid.New()
	const key = "jira_token/host"

	aad := mustAAD(t, org.String(), user.String(), key)
	want := append(append(append([]byte{}, org[:]...), user[:]...), key...)
	if !bytes.Equal(aad, want) {
		t.Fatalf("AAD layout mismatch:\n got %x\nwant %x", aad, want)
	}
	if len(aad) != 16+16+len(key) {
		t.Fatalf("AAD length %d, want %d", len(aad), 16+16+len(key))
	}
}

func TestSecretAAD_RejectsInvalidArgs(t *testing.T) {
	if _, err := secretAAD("not-a-uuid", "", "key"); err == nil {
		t.Error("invalid org_id accepted")
	}
	if _, err := secretAAD(uuid.New().String(), "not-a-uuid", "key"); err == nil {
		t.Error("invalid user_id accepted")
	}
	// Empty key is a caller bug (also barred by org_secrets_key_nonempty);
	// secretAAD rejects it so the binding always has a real key segment.
	if _, err := secretAAD(uuid.New().String(), "", ""); err == nil {
		t.Error("empty key accepted (org scope)")
	}
	if _, err := secretAAD(uuid.New().String(), uuid.New().String(), ""); err == nil {
		t.Error("empty key accepted (user scope)")
	}
}

// TestSecretAAD_RelocationFailsDecrypt is the pure-crypto mirror of the
// pgtest relocation tests: a value encrypted under one row's AAD does not
// decrypt under another's, even with the correct deployment key — while it
// still decrypts under its home identity (so the failure is the binding,
// not a corrupt blob).
func TestSecretAAD_RelocationFailsDecrypt(t *testing.T) {
	key := mustAEADKey(t)
	orgA := uuid.New().String()
	orgB := uuid.New().String()
	userA := uuid.New().String()

	aadHome := mustAAD(t, orgA, "", "github_pat")
	const plaintext = "ghp_secret_value"

	ct, nonce, err := key.Encrypt([]byte(plaintext), aadHome)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Same deployment key, foreign row identities → every relocation fails.
	for name, foreign := range map[string][]byte{
		"cross-org":     mustAAD(t, orgB, "", "github_pat"),
		"org→user":      mustAAD(t, orgA, userA, "github_pat"),
		"different key": mustAAD(t, orgA, "", "jira_token"),
	} {
		if _, err := key.Decrypt(ct, nonce, foreign); err == nil {
			t.Errorf("%s: ciphertext decrypted under a foreign row AAD", name)
		}
	}

	// Home identity still opens it.
	got, err := key.Decrypt(ct, nonce, aadHome)
	if err != nil {
		t.Fatalf("home decrypt: %v", err)
	}
	if string(got) != plaintext {
		t.Fatalf("home decrypt got %q want %q", got, plaintext)
	}
}

func mustAAD(t *testing.T, orgID, userID, key string) []byte {
	t.Helper()
	aad, err := secretAAD(orgID, userID, key)
	if err != nil {
		t.Fatalf("secretAAD(%q,%q,%q): %v", orgID, userID, key, err)
	}
	return aad
}
