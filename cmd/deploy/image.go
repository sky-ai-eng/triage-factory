package deploy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	protorekor "github.com/sigstore/protobuf-specs/gen/pb-go/rekor/v1"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

const (
	// imageRepository is where every release's image is published.
	imageRepository = "ghcr.io/sky-ai-eng/triage-factory"

	// signerIssuer and signerWorkflow name who may have signed a release
	// image: GitHub Actions, running docker-publish.yml in this repository.
	// The workflow is the file, not its caller: release.yml calls it, and
	// Fulcio takes the identity from the called workflow's job_workflow_ref.
	signerIssuer   = "https://token.actions.githubusercontent.com"
	signerWorkflow = "https://github.com/sky-ai-eng/triage-factory/.github/workflows/docker-publish.yml"
)

// Cosign stores an image's signatures as an OCI image tagged
// sha256-<digest>.sig in the image's own repository, one layer per signature.
// Each layer is the signed payload, and its annotations carry the signature,
// the Fulcio certificate and the Rekor entry with its signed timestamp.
const (
	cosignPayloadMediaType = "application/vnd.dev.cosign.simplesigning.v1+json"
	cosignSignatureKey     = "dev.cosignproject.cosign/signature"
	cosignCertificateKey   = "dev.sigstore.cosign/certificate"
	cosignRekorBundleKey   = "dev.sigstore.cosign/bundle"
	cosignPayloadType      = "cosign container image signature"

	// maxSignatureLayerBytes bounds what is read for one signature payload,
	// which is a few hundred bytes of JSON.
	maxSignatureLayerBytes = 1 << 20
)

// signerIdentity is the certificate identity version's image signature must
// carry: the docker-publish workflow at that version's tag, exactly. A
// signature from any other workflow, or from the same workflow on another
// ref, is not this release's.
func signerIdentity(version string) string {
	return signerWorkflow + "@refs/tags/" + version
}

// imageVerifier resolves a release's image to a digest and verifies the
// digest's signature before anything is rendered against it.
type imageVerifier struct {
	repository string
	// trust loads the Sigstore trust root: Fulcio's CAs, Rekor's and the CT
	// logs' keys. Called once per resolve.
	trust           func() (root.TrustedMaterial, error)
	verifierOptions []verify.VerifierOption
	remoteOptions   []remote.Option
}

// newImageVerifier is the production verifier: ghcr.io, Sigstore's public
// trust root fetched through TUF from the root embedded in sigstore-go, and
// the registry credentials docker itself uses, for a private package.
func newImageVerifier(version string) imageVerifier {
	return imageVerifier{
		repository: imageRepository,
		trust: func() (root.TrustedMaterial, error) {
			// Held in memory: deploy writes nothing outside the deployment
			// directory, and a cached root would only save one small fetch.
			return root.FetchTrustedRootWithOptions(tuf.DefaultOptions().WithDisableLocalCache())
		},
		// The certificate must carry a CT log's timestamp (Fulcio logged it),
		// and the signature must be in Rekor, whose signed integration time is
		// when the short-lived certificate is checked for validity.
		verifierOptions: []verify.VerifierOption{
			verify.WithSignedCertificateTimestamps(1),
			verify.WithTransparencyLog(1),
			verify.WithIntegratedTimestamps(1),
		},
		remoteOptions: []remote.Option{
			remote.WithAuthFromKeychain(authn.DefaultKeychain),
			remote.WithUserAgent("triagefactory-deploy/" + version),
		},
	}
}

// resolve returns version's image pinned by digest, <repository>:<version>@
// sha256:…, once a signature over that digest verifies as version's.
func (v imageVerifier) resolve(ctx context.Context, version string) (string, error) {
	tag, err := v.tag(version)
	if err != nil {
		return "", err
	}
	opts := v.options(ctx)

	desc, err := remote.Get(tag, opts...)
	if err != nil {
		switch {
		case isNotFound(err):
			return "", fmt.Errorf("%s is not published yet. A release's image is pushed and signed by its release workflow; if %s was tagged in the last few minutes, wait for that workflow to finish and try again", tag, version)
		case isDenied(err):
			return "", fmt.Errorf("%s: the registry refused the pull (%w); if the package is private, run `docker login ghcr.io` first", tag, err)
		}
		return "", fmt.Errorf("resolve %s: %w", tag, err)
	}
	if err := v.verifyDigest(ctx, tag, desc.Digest, version, opts); err != nil {
		return "", err
	}
	return tag.String() + "@" + desc.Digest.String(), nil
}

// verifyPinned verifies the signature of an image deploy already pinned,
// ref being <repository>:<version>@sha256:…: the pinned digest itself, not
// whatever the tag names now, so a tag moved since the pin changes nothing.
func (v imageVerifier) verifyPinned(ctx context.Context, version, ref string) error {
	tag, err := v.tag(version)
	if err != nil {
		return err
	}
	base, digestStr, ok := strings.Cut(ref, "@")
	if !ok || base != tag.String() {
		return fmt.Errorf("the pinned image %s is not %s@<digest>", ref, tag)
	}
	digest, err := v1.NewHash(digestStr)
	if err != nil {
		return fmt.Errorf("the pinned image %s: %w", ref, err)
	}
	return v.verifyDigest(ctx, tag, digest, version, v.options(ctx))
}

func (v imageVerifier) tag(version string) (name.Tag, error) {
	tagRef := v.repository + ":" + version
	tag, err := name.NewTag(tagRef, name.StrictValidation)
	if err != nil {
		return name.Tag{}, fmt.Errorf("image reference %s: %w", tagRef, err)
	}
	return tag, nil
}

func (v imageVerifier) options(ctx context.Context) []remote.Option {
	return append([]remote.Option{remote.WithContext(ctx)}, v.remoteOptions...)
}

// verifyDigest checks that a signature cosign attached to digest verifies as
// version's.
func (v imageVerifier) verifyDigest(ctx context.Context, tag name.Tag, digest v1.Hash, version string, opts []remote.Option) error {
	payloads, err := v.signatureLayers(ctx, tag, digest.Hex, opts)
	if err != nil {
		return err
	}
	material, err := v.trust()
	if err != nil {
		return fmt.Errorf("load the Sigstore trust root: %w", err)
	}
	identity := signerIdentity(version)
	var failures []error
	for _, p := range payloads {
		err := verifySignatureLayer(material, v.verifierOptions, identity, digest.String(), p)
		if err == nil {
			return nil
		}
		failures = append(failures, err)
	}
	return fmt.Errorf("%s@%s has no signature that verifies as %s's (want issuer %s and identity %s): %w",
		tag, digest, version, signerIssuer, identity, errors.Join(failures...))
}

// signatureLayer is one cosign signature: the payload it signs and the
// annotations that carry the signature and its proof.
type signatureLayer struct {
	payload     []byte
	annotations map[string]string
}

// signatureLayers reads the signature image cosign attached to digestHex.
func (v imageVerifier) signatureLayers(ctx context.Context, tag name.Tag, digestHex string, opts []remote.Option) ([]signatureLayer, error) {
	sigTag := tag.Context().Tag("sha256-" + digestHex + ".sig")
	img, err := remote.Image(sigTag, opts...)
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%s@sha256:%s has no cosign signature (no %s); refusing to pin an unsigned image", tag, digestHex, sigTag.TagStr())
		}
		return nil, fmt.Errorf("read the signature of %s: %w", tag, err)
	}
	manifest, err := img.Manifest()
	if err != nil {
		return nil, fmt.Errorf("read the signature of %s: %w", tag, err)
	}
	var layers []signatureLayer
	for _, l := range manifest.Layers {
		if string(l.MediaType) != cosignPayloadMediaType {
			continue
		}
		if l.Size > maxSignatureLayerBytes {
			return nil, fmt.Errorf("signature payload %s is %d bytes; refusing to read more than %d", l.Digest, l.Size, maxSignatureLayerBytes)
		}
		layer, err := img.LayerByDigest(l.Digest)
		if err != nil {
			return nil, fmt.Errorf("read signature payload %s: %w", l.Digest, err)
		}
		rc, err := layer.Compressed()
		if err != nil {
			return nil, fmt.Errorf("read signature payload %s: %w", l.Digest, err)
		}
		// The reader checks the bytes against the layer's digest at EOF, so
		// the payload is the one the manifest names.
		payload, err := io.ReadAll(io.LimitReader(rc, maxSignatureLayerBytes))
		_ = rc.Close()
		if err != nil {
			return nil, fmt.Errorf("read signature payload %s: %w", l.Digest, err)
		}
		layers = append(layers, signatureLayer{payload: payload, annotations: l.Annotations})
	}
	if len(layers) == 0 {
		return nil, fmt.Errorf("%s holds no cosign signature payload", sigTag)
	}
	return layers, nil
}

// simpleSigning is the part of cosign's signed payload that binds it to an
// image.
type simpleSigning struct {
	Critical struct {
		Image struct {
			DockerManifestDigest string `json:"docker-manifest-digest"`
		} `json:"image"`
		Type string `json:"type"`
	} `json:"critical"`
}

// rekorBundle is cosign's record of the signature's Rekor entry: the
// canonicalized entry, where it sits in the log, and the log's signed entry
// timestamp over all three.
type rekorBundle struct {
	SignedEntryTimestamp []byte `json:"SignedEntryTimestamp"`
	Payload              struct {
		Body           string `json:"body"`
		IntegratedTime int64  `json:"integratedTime"`
		LogIndex       int64  `json:"logIndex"`
		LogID          string `json:"logID"`
	} `json:"Payload"`
}

// verifySignatureLayer verifies one cosign signature over digest: the payload
// names digest, and Sigstore verifies the signature over the payload, the
// certificate's chain to Fulcio, its SCT, the Rekor entry, and that the
// certificate names exactly identity, issued by GitHub Actions.
func verifySignatureLayer(material root.TrustedMaterial, opts []verify.VerifierOption, identity, digest string, l signatureLayer) error {
	var payload simpleSigning
	if err := json.Unmarshal(l.payload, &payload); err != nil {
		return fmt.Errorf("parse signed payload: %w", err)
	}
	if payload.Critical.Type != cosignPayloadType {
		return fmt.Errorf("signed payload has type %q, want %q", payload.Critical.Type, cosignPayloadType)
	}
	if got := payload.Critical.Image.DockerManifestDigest; got != digest {
		return fmt.Errorf("signature is for %s, not %s", got, digest)
	}

	b, err := cosignBundle(l)
	if err != nil {
		return err
	}
	verifier, err := verify.NewVerifier(material, opts...)
	if err != nil {
		return fmt.Errorf("build verifier: %w", err)
	}
	certID, err := verify.NewShortCertificateIdentity(signerIssuer, "", identity, "")
	if err != nil {
		return fmt.Errorf("build identity policy: %w", err)
	}
	policy := verify.NewPolicy(verify.WithArtifact(bytes.NewReader(l.payload)), verify.WithCertificateIdentity(certID))
	if _, err := verifier.Verify(b, policy); err != nil {
		return err
	}
	return nil
}

// cosignBundle restates a cosign signature layer as a Sigstore bundle, so
// sigstore-go verifies it as it would any other. Cosign's layer is a v0.1
// bundle in all but name: a certificate, a signature over the payload, and a
// Rekor entry proven by its signed entry timestamp rather than an inclusion
// proof.
func cosignBundle(l signatureLayer) (*bundle.Bundle, error) {
	sig, err := base64.StdEncoding.DecodeString(l.annotations[cosignSignatureKey])
	if err != nil || len(sig) == 0 {
		return nil, fmt.Errorf("signature annotation %s is missing or not base64", cosignSignatureKey)
	}
	block, _ := pem.Decode([]byte(l.annotations[cosignCertificateKey]))
	if block == nil {
		return nil, fmt.Errorf("certificate annotation %s is missing or not PEM", cosignCertificateKey)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse signing certificate: %w", err)
	}

	var rb rekorBundle
	if err := json.Unmarshal([]byte(l.annotations[cosignRekorBundleKey]), &rb); err != nil {
		return nil, fmt.Errorf("the Rekor annotation %s is missing or malformed: %w", cosignRekorBundleKey, err)
	}
	body, err := base64.StdEncoding.DecodeString(rb.Payload.Body)
	if err != nil {
		return nil, fmt.Errorf("the Rekor entry body is not base64: %w", err)
	}
	logID, err := hex.DecodeString(rb.Payload.LogID)
	if err != nil {
		return nil, fmt.Errorf("the Rekor log id is not hex: %w", err)
	}
	var kind struct {
		Kind       string `json:"kind"`
		APIVersion string `json:"apiVersion"`
	}
	if err := json.Unmarshal(body, &kind); err != nil {
		return nil, fmt.Errorf("parse Rekor entry body: %w", err)
	}

	digest := sha256.Sum256(l.payload)
	pb := &protobundle.Bundle{
		MediaType: "application/vnd.dev.sigstore.bundle+json;version=0.1",
		VerificationMaterial: &protobundle.VerificationMaterial{
			Content: &protobundle.VerificationMaterial_X509CertificateChain{
				X509CertificateChain: &protocommon.X509CertificateChain{
					Certificates: []*protocommon.X509Certificate{{RawBytes: cert.Raw}},
				},
			},
			TlogEntries: []*protorekor.TransparencyLogEntry{{
				LogIndex:          rb.Payload.LogIndex,
				LogId:             &protocommon.LogId{KeyId: logID},
				KindVersion:       &protorekor.KindVersion{Kind: kind.Kind, Version: kind.APIVersion},
				IntegratedTime:    rb.Payload.IntegratedTime,
				InclusionPromise:  &protorekor.InclusionPromise{SignedEntryTimestamp: rb.SignedEntryTimestamp},
				CanonicalizedBody: body,
			}},
		},
		Content: &protobundle.Bundle_MessageSignature{
			MessageSignature: &protocommon.MessageSignature{
				MessageDigest: &protocommon.HashOutput{Algorithm: protocommon.HashAlgorithm_SHA2_256, Digest: digest[:]},
				Signature:     sig,
			},
		},
	}
	b, err := bundle.NewBundle(pb)
	if err != nil {
		return nil, fmt.Errorf("assemble signature bundle: %w", err)
	}
	return b, nil
}

// isNotFound reports a registry 404: no such tag, manifest or repository.
func isNotFound(err error) bool {
	var terr *transport.Error
	if !errors.As(err, &terr) {
		return false
	}
	if terr.StatusCode == http.StatusNotFound {
		return true
	}
	for _, d := range terr.Errors {
		switch d.Code {
		case transport.ManifestUnknownErrorCode, transport.NameUnknownErrorCode:
			return true
		}
	}
	return false
}

// isDenied reports a registry 401 or 403, which ghcr answers for a private
// package pulled without credentials.
func isDenied(err error) bool {
	var terr *transport.Error
	if !errors.As(err, &terr) {
		return false
	}
	if terr.StatusCode == http.StatusUnauthorized || terr.StatusCode == http.StatusForbidden {
		return true
	}
	for _, d := range terr.Errors {
		switch d.Code {
		case transport.UnauthorizedErrorCode, transport.DeniedErrorCode:
			return true
		}
	}
	return false
}
