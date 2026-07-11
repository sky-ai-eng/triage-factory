package credseal

import (
	"bytes"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	kp, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	plaintext := []byte(`{"llm":{"anthropic_api_key":"sk-ant-test"}}`)

	sealed, err := Seal(kp.Public, plaintext)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	got, err := kp.Open(sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("Open = %q, want %q", got, plaintext)
	}
}

func TestOpenWithWrongKeyFails(t *testing.T) {
	kp1, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	kp2, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	sealed, err := Seal(kp1.Public, []byte("secret"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := kp2.Open(sealed); err == nil {
		t.Fatal("Open with the wrong keypair succeeded; want ErrSealedBoxInvalid")
	}
}

func TestOpenTamperedCiphertextFails(t *testing.T) {
	kp, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	sealed, err := Seal(kp.Public, []byte("secret"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 0xFF
	if _, err := kp.Open(tampered); err == nil {
		t.Fatal("Open of tampered ciphertext succeeded; want ErrSealedBoxInvalid")
	}
}

func TestOpenTooShortFails(t *testing.T) {
	kp, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	if _, err := kp.Open([]byte("short")); err == nil {
		t.Fatal("Open of a too-short blob succeeded; want ErrSealedBoxInvalid")
	}
}

func TestSealIsNonDeterministic(t *testing.T) {
	kp, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	a, err := Seal(kp.Public, []byte("secret"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	b, err := Seal(kp.Public, []byte("secret"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("two Seal calls of the same plaintext produced identical ciphertext (ephemeral key/nonce reuse)")
	}
}
