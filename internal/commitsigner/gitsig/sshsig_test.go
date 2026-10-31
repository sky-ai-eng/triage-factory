package gitsig

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const testPayload = "tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\n" +
	"author Octo Cat <1+octocat@users.noreply.github.com> 1700000000 +0000\n" +
	"committer Octo Cat <1+octocat@users.noreply.github.com> 1700000000 +0000\n" +
	"\n" +
	"subject\n"

func mustKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	key, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return key
}

func mustPublicKey(t *testing.T, key ed25519.PrivateKey) string {
	t.Helper()
	pub, err := PublicKey(key)
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}
	return pub
}

// writeKeyFile writes key where ssh-keygen will read it: OpenSSH format, mode
// 0600, since ssh-keygen refuses a private key other users can read.
func writeKeyFile(t *testing.T, key ed25519.PrivateKey) string {
	t.Helper()
	pemBytes, err := MarshalPrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalPrivateKey: %v", err)
	}
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func requireSSHKeygen(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen not installed")
	}
	return path
}

func sshString(s string) []byte {
	return binary.BigEndian.AppendUint32(nil, uint32(len(s)))
}

func TestSignedDataLayout(t *testing.T) {
	d := DigestOf([]byte(testPayload))
	var want []byte
	want = append(want, "SSHSIG"...)
	for _, field := range []string{"git", "", "sha512", string(d[:])} {
		want = append(want, sshString(field)...)
		want = append(want, field...)
	}
	if got := SignedData(d); !bytes.Equal(got, want) {
		t.Fatalf("SignedData = %x, want %x", got, want)
	}
}

func TestSignDigestEqualsSign(t *testing.T) {
	key := mustKey(t)
	fromPayload, err := Sign(key, []byte(testPayload))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	fromDigest, err := SignDigest(key, DigestOf([]byte(testPayload)))
	if err != nil {
		t.Fatalf("SignDigest: %v", err)
	}
	if !bytes.Equal(fromPayload, fromDigest) {
		t.Fatalf("SignDigest = %q, Sign = %q", fromDigest, fromPayload)
	}
}

func TestSignArmor(t *testing.T) {
	sig, err := Sign(mustKey(t), []byte(testPayload))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(sig), "\n"), "\n")
	if lines[0] != "-----BEGIN SSH SIGNATURE-----" || lines[len(lines)-1] != "-----END SSH SIGNATURE-----" {
		t.Fatalf("armor lines = %q", lines)
	}
	for _, line := range lines[1 : len(lines)-1] {
		if len(line) > 70 {
			t.Fatalf("armor line is %d characters, want at most 70: %q", len(line), line)
		}
	}
	if !strings.HasSuffix(string(sig), "\n") {
		t.Fatal("armored signature does not end in a newline")
	}
}

func TestSignDigestRefusesMalformedKey(t *testing.T) {
	for _, key := range []ed25519.PrivateKey{nil, make(ed25519.PrivateKey, 32)} {
		if _, err := SignDigest(key, Digest{}); err == nil {
			t.Fatalf("SignDigest with a %d-byte key succeeded", len(key))
		}
	}
}

func TestSignVerifiesWithSSHKeygen(t *testing.T) {
	keygen := requireSSHKeygen(t)
	key := mustKey(t)
	dir := t.TempDir()
	allowed := filepath.Join(dir, "allowed_signers")
	line := `signer@example.com namespaces="git" ` + mustPublicKey(t, key) + "\n"
	if err := os.WriteFile(allowed, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	sig, err := Sign(key, []byte(testPayload))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	sigFile := filepath.Join(dir, "payload.sig")
	if err := os.WriteFile(sigFile, sig, 0o644); err != nil {
		t.Fatal(err)
	}

	verify := func(namespace, payload string) ([]byte, error) {
		cmd := exec.Command(keygen, "-Y", "verify", "-f", allowed, "-I", "signer@example.com", "-n", namespace, "-s", sigFile)
		cmd.Stdin = strings.NewReader(payload)
		return cmd.CombinedOutput()
	}
	if out, err := verify("git", testPayload); err != nil {
		t.Fatalf("ssh-keygen -Y verify: %v\n%s", err, out)
	}
	// The same check fails for anything the signature does not cover, which
	// is what makes the pass above mean something.
	if out, err := verify("git", testPayload+"tampered\n"); err == nil {
		t.Fatalf("ssh-keygen verified a tampered payload:\n%s", out)
	}
	if out, err := verify("file", testPayload); err == nil {
		t.Fatalf("ssh-keygen verified the signature in another namespace:\n%s", out)
	}
}

// ed25519 signatures are deterministic, so ssh-keygen signing the same payload
// with the same key writes exactly the bytes Sign returns. That holds the
// armor to ssh-keygen's, and shows ssh-keygen reads MarshalPrivateKey's
// encoding.
func TestSignMatchesSSHKeygen(t *testing.T) {
	keygen := requireSSHKeygen(t)
	key := mustKey(t)
	keyFile := writeKeyFile(t, key)
	payloadFile := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(payloadFile, []byte(testPayload), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(keygen, "-Y", "sign", "-n", "git", "-f", keyFile, payloadFile).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen -Y sign: %v\n%s", err, out)
	}
	want, err := os.ReadFile(payloadFile + ".sig")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Sign(key, []byte(testPayload))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Sign =\n%s\nssh-keygen -Y sign =\n%s", got, want)
	}
}

func TestParsePrivateKeyReadsSSHKeygenKey(t *testing.T) {
	keygen := requireSSHKeygen(t)
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if out, err := exec.Command(keygen, "-q", "-t", "ed25519", "-N", "", "-C", "comment", "-f", path).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen -t ed25519: %v\n%s", err, out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ParsePrivateKey(data)
	if err != nil {
		t.Fatalf("ParsePrivateKey: %v", err)
	}
	pubLine, err := os.ReadFile(path + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(pubLine))
	if want := fields[0] + " " + fields[1]; mustPublicKey(t, key) != want {
		t.Fatalf("PublicKey = %q, want %q", mustPublicKey(t, key), want)
	}
}

func TestPrivateKeyRoundTrip(t *testing.T) {
	key := mustKey(t)
	data, err := MarshalPrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalPrivateKey: %v", err)
	}
	if !bytes.HasPrefix(data, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\n")) {
		t.Fatalf("MarshalPrivateKey = %q, want an OpenSSH private key", data)
	}
	got, err := ParsePrivateKey(data)
	if err != nil {
		t.Fatalf("ParsePrivateKey: %v", err)
	}
	if !key.Equal(got) {
		t.Fatal("ParsePrivateKey returned a different key")
	}
	pub := mustPublicKey(t, key)
	if !strings.HasPrefix(pub, "ssh-ed25519 AAAA") || strings.ContainsAny(pub, "\n") || len(strings.Fields(pub)) != 2 {
		t.Fatalf("PublicKey = %q, want ssh-ed25519 AAAA… with no comment", pub)
	}
}

func TestParsePrivateKeyRefusesOtherKeys(t *testing.T) {
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(ec)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"ecdsa":   pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}),
		"not pem": []byte("ssh-ed25519 AAAA"),
	} {
		if _, err := ParsePrivateKey(data); err == nil {
			t.Errorf("ParsePrivateKey(%s) succeeded", name)
		}
	}
}

func TestParsePrivateKeyRefusesMismatchedPublicHalf(t *testing.T) {
	key := mustKey(t)
	key[len(key)-1] ^= 0xff
	data, err := MarshalPrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalPrivateKey: %v", err)
	}
	if _, err := ParsePrivateKey(data); err == nil {
		t.Fatal("ParsePrivateKey accepted a key whose public half does not match its seed")
	}
}

func TestParseDigest(t *testing.T) {
	d := DigestOf([]byte(testPayload))
	got, err := ParseDigest(d.String())
	if err != nil {
		t.Fatalf("ParseDigest: %v", err)
	}
	if got != d {
		t.Fatalf("ParseDigest = %s, want %s", got, d)
	}
	for _, s := range []string{
		"",
		d.String()[:126],
		d.String() + "00",
		strings.ToUpper(d.String()),
		"zz" + d.String()[2:],
	} {
		if _, err := ParseDigest(s); err == nil {
			t.Errorf("ParseDigest(%q) succeeded", s)
		}
	}
}
