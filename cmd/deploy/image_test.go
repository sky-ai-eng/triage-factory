package deploy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/sigstore/sigstore-go/pkg/tlog"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
)

const testVersion = "v1.2.3"

// signingFixture is a registry holding one published release image, and a
// virtual Sigstore (its own Fulcio CA and Rekor key) that signs in the shape
// cosign stores on the registry. Each test signs, or doesn't, and resolves.
type signingFixture struct {
	t          *testing.T
	repository string
	sigstore   *ca.VirtualSigstore
	image      v1.Image
	digest     v1.Hash
}

func newSigningFixture(t *testing.T) *signingFixture {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	repository := strings.TrimPrefix(srv.URL, "http://") + "/sky-ai-eng/triage-factory"

	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatalf("random image: %v", err)
	}
	digest, err := img.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatalf("virtual sigstore: %v", err)
	}
	f := &signingFixture{t: t, repository: repository, sigstore: vs, image: img, digest: digest}
	f.push(repository+":"+testVersion, img)
	return f
}

func (f *signingFixture) push(ref string, img v1.Image) {
	f.t.Helper()
	tag, err := name.NewTag(ref)
	if err != nil {
		f.t.Fatalf("tag %s: %v", ref, err)
	}
	if err := remote.Write(tag, img); err != nil {
		f.t.Fatalf("push %s: %v", ref, err)
	}
}

// payload is cosign's simple-signing payload for an image digest.
func payload(digest string) []byte {
	b, _ := json.Marshal(map[string]any{
		"critical": map[string]any{
			"identity": map[string]any{"docker-reference": imageRepository},
			"image":    map[string]any{"docker-manifest-digest": digest},
			"type":     cosignPayloadType,
		},
		"optional": nil,
	})
	return b
}

// signLayer signs body with a certificate for identity and issuer and returns
// the signature layer cosign would attach: the payload, its signature, the
// certificate, and the Rekor entry with the log's signed entry timestamp.
func (f *signingFixture) signLayer(vs *ca.VirtualSigstore, identity, issuer string, body []byte) mutate.Addendum {
	f.t.Helper()
	entity, err := vs.Sign(identity, issuer, body)
	if err != nil {
		f.t.Fatalf("sign: %v", err)
	}
	vc, err := entity.VerificationContent()
	if err != nil {
		f.t.Fatalf("verification content: %v", err)
	}
	certPEM, err := cryptoutils.MarshalCertificateToPEM(vc.(*bundle.Certificate).Certificate())
	if err != nil {
		f.t.Fatalf("cert pem: %v", err)
	}
	sc, err := entity.SignatureContent()
	if err != nil {
		f.t.Fatalf("signature content: %v", err)
	}
	entries, err := entity.TlogEntries()
	if err != nil || len(entries) != 1 {
		f.t.Fatalf("tlog entries: %v (%d)", err, len(entries))
	}
	tle := entries[0].TransparencyLogEntry()
	logID, err := vs.RekorLogID()
	if err != nil {
		f.t.Fatalf("rekor log id: %v", err)
	}
	rp := tlog.RekorPayload{
		Body:           base64.StdEncoding.EncodeToString(tle.CanonicalizedBody),
		IntegratedTime: tle.IntegratedTime,
		LogIndex:       tle.LogIndex,
		LogID:          logID,
	}
	set, err := vs.RekorSignPayload(rp)
	if err != nil {
		f.t.Fatalf("sign entry timestamp: %v", err)
	}
	var rb rekorBundle
	rb.SignedEntryTimestamp = set
	rb.Payload.Body = rp.Body.(string)
	rb.Payload.IntegratedTime = rp.IntegratedTime
	rb.Payload.LogIndex = rp.LogIndex
	rb.Payload.LogID = rp.LogID
	rbJSON, err := json.Marshal(rb)
	if err != nil {
		f.t.Fatalf("marshal rekor bundle: %v", err)
	}
	return mutate.Addendum{
		Layer: static.NewLayer(body, cosignPayloadMediaType),
		Annotations: map[string]string{
			cosignSignatureKey:   base64.StdEncoding.EncodeToString(sc.Signature()),
			cosignCertificateKey: string(certPEM),
			cosignRekorBundleKey: string(rbJSON),
		},
	}
}

// attach pushes layers as the signature image of the image with digest d.
func (f *signingFixture) attach(d v1.Hash, layers ...mutate.Addendum) {
	f.t.Helper()
	img := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
	img = mutate.ConfigMediaType(img, types.OCIConfigJSON)
	img, err := mutate.Append(img, layers...)
	if err != nil {
		f.t.Fatalf("append signature layers: %v", err)
	}
	f.push(f.repository+":sha256-"+d.Hex+".sig", img)
}

func (f *signingFixture) verifier() imageVerifier {
	return imageVerifier{
		repository: f.repository,
		trust:      func() (root.TrustedMaterial, error) { return f.sigstore, nil },
		// The production options with one difference: the virtual Fulcio
		// issues certificates with no SCT, so that check is left out here.
		verifierOptions: []verify.VerifierOption{
			verify.WithTransparencyLog(1),
			verify.WithIntegratedTimestamps(1),
		},
	}
}

func TestResolve_VerifiedReleaseIsPinnedByDigest(t *testing.T) {
	f := newSigningFixture(t)
	f.attach(f.digest, f.signLayer(f.sigstore, signerIdentity(testVersion), signerIssuer, payload(f.digest.String())))

	ref, err := f.verifier().resolve(context.Background(), testVersion)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if want := f.repository + ":" + testVersion + "@" + f.digest.String(); ref != want {
		t.Errorf("ref = %q; want %q", ref, want)
	}
}

// TestResolve_AnyVerifyingLayerSuffices: a signature image can hold several
// signatures (an image signed twice); one that verifies is enough, and the
// others are not held against it.
func TestResolve_AnyVerifyingLayerSuffices(t *testing.T) {
	f := newSigningFixture(t)
	f.attach(f.digest,
		f.signLayer(f.sigstore, signerWorkflow+"@refs/heads/main", signerIssuer, payload(f.digest.String())),
		f.signLayer(f.sigstore, signerIdentity(testVersion), signerIssuer, payload(f.digest.String())),
	)
	if _, err := f.verifier().resolve(context.Background(), testVersion); err != nil {
		t.Fatalf("resolve: %v", err)
	}
}

func TestResolve_Refusals(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(f *signingFixture)
		wantErr string
	}{
		{
			name:    "unsigned digest",
			arrange: func(*signingFixture) {},
			wantErr: "has no cosign signature",
		},
		{
			name: "signature from another workflow",
			arrange: func(f *signingFixture) {
				id := "https://github.com/sky-ai-eng/triage-factory/.github/workflows/release.yml@refs/tags/" + testVersion
				f.attach(f.digest, f.signLayer(f.sigstore, id, signerIssuer, payload(f.digest.String())))
			},
			wantErr: "no signature that verifies",
		},
		{
			name: "signature from another tag",
			arrange: func(f *signingFixture) {
				f.attach(f.digest, f.signLayer(f.sigstore, signerIdentity("v1.2.2"), signerIssuer, payload(f.digest.String())))
			},
			wantErr: "no signature that verifies",
		},
		{
			name: "signature from a branch build of the same workflow",
			arrange: func(f *signingFixture) {
				f.attach(f.digest, f.signLayer(f.sigstore, signerWorkflow+"@refs/heads/main", signerIssuer, payload(f.digest.String())))
			},
			wantErr: "no signature that verifies",
		},
		{
			name: "signature from another repository's workflow",
			arrange: func(f *signingFixture) {
				id := "https://github.com/someone-else/triage-factory/.github/workflows/docker-publish.yml@refs/tags/" + testVersion
				f.attach(f.digest, f.signLayer(f.sigstore, id, signerIssuer, payload(f.digest.String())))
			},
			wantErr: "no signature that verifies",
		},
		{
			name: "signature from another OIDC issuer",
			arrange: func(f *signingFixture) {
				f.attach(f.digest, f.signLayer(f.sigstore, signerIdentity(testVersion), "https://accounts.google.com", payload(f.digest.String())))
			},
			wantErr: "no signature that verifies",
		},
		{
			// The tag was moved to an image the release never signed, and the
			// valid signature of the real release image was copied alongside.
			name: "tampered digest",
			arrange: func(f *signingFixture) {
				other, err := random.Image(256, 1)
				if err != nil {
					f.t.Fatalf("random image: %v", err)
				}
				otherDigest, _ := other.Digest()
				f.push(f.repository+":"+testVersion, other)
				f.attach(otherDigest, f.signLayer(f.sigstore, signerIdentity(testVersion), signerIssuer, payload(f.digest.String())))
			},
			wantErr: "signature is for " + "sha256:",
		},
		{
			name: "payload altered after signing",
			arrange: func(f *signingFixture) {
				layer := f.signLayer(f.sigstore, signerIdentity(testVersion), signerIssuer, payload(f.digest.String()))
				tampered := append(payload(f.digest.String()), ' ')
				layer.Layer = static.NewLayer(tampered, cosignPayloadMediaType)
				f.attach(f.digest, layer)
			},
			wantErr: "no signature that verifies",
		},
		{
			name: "signature from another Sigstore instance",
			arrange: func(f *signingFixture) {
				rogue, err := ca.NewVirtualSigstore()
				if err != nil {
					f.t.Fatalf("virtual sigstore: %v", err)
				}
				f.attach(f.digest, f.signLayer(rogue, signerIdentity(testVersion), signerIssuer, payload(f.digest.String())))
			},
			wantErr: "no signature that verifies",
		},
		{
			name: "signed entry timestamp from another log",
			arrange: func(f *signingFixture) {
				layer := f.signLayer(f.sigstore, signerIdentity(testVersion), signerIssuer, payload(f.digest.String()))
				var rb rekorBundle
				if err := json.Unmarshal([]byte(layer.Annotations[cosignRekorBundleKey]), &rb); err != nil {
					f.t.Fatalf("unmarshal: %v", err)
				}
				rb.SignedEntryTimestamp[len(rb.SignedEntryTimestamp)-1] ^= 0xff
				b, _ := json.Marshal(rb)
				layer.Annotations[cosignRekorBundleKey] = string(b)
				f.attach(f.digest, layer)
			},
			wantErr: "no signature that verifies",
		},
		{
			name: "not published",
			arrange: func(f *signingFixture) {
				f.repository += "-unpublished"
			},
			wantErr: "is not published yet",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newSigningFixture(t)
			tt.arrange(f)
			ref, err := f.verifier().resolve(context.Background(), testVersion)
			if err == nil {
				t.Fatalf("resolve = %q; want a refusal", ref)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v; want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestSignerIdentity(t *testing.T) {
	got := signerIdentity("v1.17.0")
	want := "https://github.com/sky-ai-eng/triage-factory/.github/workflows/docker-publish.yml@refs/tags/v1.17.0"
	if got != want {
		t.Errorf("signerIdentity = %q; want %q", got, want)
	}
}

// TestVerifySignatureLayer_RealSignature runs the production verifier options,
// SCT check included, over a real cosign signature made by GitHub Actions and
// logged in the public Rekor: Flux's source-controller v1.4.1, fetched from
// ghcr.io as cosign stored it. The virtual Sigstore above proves the policy;
// this proves the reading of cosign's annotations against what cosign writes,
// under Sigstore's public-good trust root (testdata holds a snapshot, so the
// test needs no network).
func TestVerifySignatureLayer_RealSignature(t *testing.T) {
	const (
		digest   = "sha256:3c5f0f022f990ffc0daf00e5b199548fc0fa6e7119e972318f0267081a332963"
		identity = "https://github.com/fluxcd/source-controller/.github/workflows/release.yml@refs/tags/v1.4.1"
	)
	material, err := root.NewTrustedRootFromPath("testdata/trusted-root-public-good.json")
	if err != nil {
		t.Fatalf("load trust root: %v", err)
	}
	rawManifest, err := os.ReadFile("testdata/flux-source-controller-v1.4.1.sig.json")
	if err != nil {
		t.Fatalf("read signature manifest: %v", err)
	}
	manifest, err := v1.ParseManifest(bytes.NewReader(rawManifest))
	if err != nil {
		t.Fatalf("parse signature manifest: %v", err)
	}
	signed, err := os.ReadFile("testdata/flux-source-controller-v1.4.1.payload.json")
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if len(manifest.Layers) != 1 {
		t.Fatalf("signature manifest has %d layers; want 1", len(manifest.Layers))
	}
	if sum := sha256.Sum256(signed); hex.EncodeToString(sum[:]) != manifest.Layers[0].Digest.Hex {
		t.Fatal("payload fixture does not match the layer digest the signature manifest names")
	}
	layer := signatureLayer{payload: signed, annotations: manifest.Layers[0].Annotations}
	opts := newImageVerifier(testVersion).verifierOptions

	if err := verifySignatureLayer(material, opts, identity, digest, layer); err != nil {
		t.Fatalf("real signature did not verify: %v", err)
	}
	if err := verifySignatureLayer(material, opts, signerIdentity("v1.4.1"), digest, layer); err == nil {
		t.Error("a signature from another repository's workflow verified as ours")
	}
	other := "sha256:" + strings.Repeat("0", 64)
	if err := verifySignatureLayer(material, opts, identity, other, layer); err == nil {
		t.Error("a signature verified for a digest its payload does not name")
	}
}

// TestDockerPublishSignsInTheFormatInitReads holds docker-publish.yml to the
// signature format resolve reads. cosign 2 (what cosign-installer v3
// installs) attaches a signature as the tag sha256-<digest>.sig; cosign 3
// defaults to a bundle stored as an OCI referrer, which resolve would not
// find, and every release signed that way would fail deploy init.
func TestDockerPublishSignsInTheFormatInitReads(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "docker-publish.yml"))
	if err != nil {
		t.Fatalf("read docker-publish.yml: %v", err)
	}
	workflow := string(raw)
	installers := regexp.MustCompile(`uses:\s*sigstore/cosign-installer@(\S+)`).FindAllStringSubmatch(workflow, -1)
	if len(installers) != 1 || installers[0][1] != "v3" {
		t.Errorf("docker-publish.yml installs cosign with %v; want sigstore/cosign-installer@v3, whose cosign 2 writes the .sig tag deploy init verifies", installers)
	}
	for _, setting := range []string{"cosign-release", "new-bundle-format"} {
		if strings.Contains(workflow, setting) {
			t.Errorf("docker-publish.yml sets %s, which changes how the image signature is stored; deploy init reads only the .sig tag", setting)
		}
	}
}
