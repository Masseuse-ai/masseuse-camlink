// Package provenance checks, from the public registry and the Sigstore
// transparency log, that the image an enclave attested is a release build
// of the enclave repository.
//
// The attestation (internal/attest) proves which image digest is running
// and that the launcher verified a signature over it from the release key;
// the release stamp in the attested environment says which release and
// commit the image claims to be. This package ties the digest to public,
// independently logged records of the build, the way VERIFY.md tells a
// reader to with cosign and slsa-verifier, but on every dial and inside the
// connector:
//
//  1. a keyless signature over the digest whose Fulcio certificate names
//     the enclave repository's release workflow at the tag the token
//     names, recorded in the Rekor transparency log (cosign's Sigstore
//     bundle, attached to the image as an OCI referrer);
//  2. SLSA provenance for the digest, signed by the SLSA GitHub generator
//     (a certificate naming that builder, with the enclave repository, the
//     same tag and the same commit in its extensions, also logged), whose
//     statement says the build was configured by the release workflow at
//     that tag and commit (cosign's legacy attestation tag, or a bundle
//     referrer).
//
// Verification uses sigstore-go with the public Sigstore trust root: the
// copy embedded at build time, refreshed through TUF into the state
// directory when the network allows. Results are cached per digest,
// release and commit. Anything missing or failing refuses the enclave.
package provenance

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	protodsse "github.com/sigstore/protobuf-specs/gen/pb-go/dsse"
	protorekor "github.com/sigstore/protobuf-specs/gen/pb-go/rekor/v1"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"

	"github.com/FemLed/masseuse-camlink/internal/oci"
)

// The public Sigstore trust root (Fulcio, Rekor, the CT logs and the
// timestamp authority) as served by the Sigstore TUF repository when this
// file was last refreshed. It is the fallback when TUF cannot be reached.
//
//go:embed trusted_root.json
var embeddedTrustedRoot []byte

// Defaults for the GitHub Actions release path.
const (
	DefaultIssuer       = "https://token.actions.githubusercontent.com"
	DefaultWorkflowPath = ".github/workflows/release.yml"
	// DefaultBuilderSAN is the identity of the SLSA GitHub generator for
	// container images at any release of the generator.
	DefaultBuilderSAN = `^https://github\.com/slsa-framework/slsa-github-generator/\.github/workflows/generator_container_slsa3\.yml@refs/tags/v\d+\.\d+\.\d+$`

	bundleMediaTypePrefix = "application/vnd.dev.sigstore.bundle"
	dsseMediaType         = "application/vnd.dsse.envelope.v1+json"
	bundleV01MediaType    = "application/vnd.dev.sigstore.bundle+json;version=0.1"
)

// Expect is what the attestation said about the image and where the
// policy says its records live.
type Expect struct {
	Digest    string // sha256:<hex>, the attested image digest
	Release   string // vX.Y.Z, the attested TEE_IMAGE_VERSION
	Commit    string // the attested TEE_IMAGE_COMMIT (40 hex), or ""
	Repo      string // the public registry holding the digest, e.g. ghcr.io/femled/masseuse-video-tee
	SourceURI string // the repository whose release workflow built it, e.g. github.com/FemLed/masseuse-video-tee
}

var (
	releaseRE = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)
	commitRE  = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

func (e Expect) validate() error {
	if !strings.HasPrefix(e.Digest, "sha256:") || len(e.Digest) != 7+64 {
		return fmt.Errorf("image digest %q is not sha256:<64 hex>", e.Digest)
	}
	if _, err := hex.DecodeString(e.Digest[7:]); err != nil {
		return fmt.Errorf("image digest %q is not sha256:<64 hex>", e.Digest)
	}
	if !releaseRE.MatchString(e.Release) {
		return fmt.Errorf("the image carries no release tag to check provenance against (TEE_IMAGE_VERSION %q)", e.Release)
	}
	if e.Commit != "" && !commitRE.MatchString(e.Commit) {
		return fmt.Errorf("TEE_IMAGE_COMMIT %q is not a commit", e.Commit)
	}
	if _, err := oci.ParseRepository(e.Repo); err != nil {
		return err
	}
	if !strings.HasPrefix(e.SourceURI, "github.com/") || strings.Count(e.SourceURI, "/") != 2 || strings.ContainsAny(e.SourceURI, " @:\n") {
		return fmt.Errorf("source %q is not github.com/<owner>/<repository>", e.SourceURI)
	}
	return nil
}

// Result is what was verified for one image.
type Result struct {
	Digest    string `json:"digest"`
	Release   string `json:"release"`
	Commit    string `json:"commit"`
	Repo      string `json:"repo"`
	SourceURI string `json:"sourceUri"`

	// The release workflow's signature over the digest.
	SignatureIdentity string    `json:"signatureIdentity"` // the certificate's SAN
	SignatureLogIndex int64     `json:"signatureLogIndex"` // its Rekor entry
	SignatureTime     time.Time `json:"signatureTime"`     // when it was logged or timestamped

	// The SLSA provenance for the digest.
	Builder            string    `json:"builder"` // the provenance signer's SAN (the builder id)
	PredicateType      string    `json:"predicateType"`
	ProvenanceLogIndex int64     `json:"provenanceLogIndex"`
	ProvenanceTime     time.Time `json:"provenanceTime"`

	VerifiedAt time.Time `json:"verifiedAt"`
	// Cached is true when the result came from the cache rather than from
	// the registry and the log on this call.
	Cached bool `json:"-"`
}

// Verifier checks images. The zero value uses the embedded trust root, no
// on-disk cache and the GitHub Actions defaults.
type Verifier struct {
	// Registry fetches from the public registry; nil means a default client.
	Registry *oci.Client
	// CacheDir holds the TUF metadata and verified results; "" keeps
	// results in memory only and never refreshes the trust root.
	CacheDir string
	// Issuer is the OIDC issuer both certificates must name.
	Issuer string
	// WorkflowPath is the release workflow within the source repository.
	WorkflowPath string
	// BuilderSAN matches the SAN of the certificate that signed the
	// provenance, and the builder id in it.
	BuilderSAN *regexp.Regexp
	// TTL is how long a verified result is reused; 0 means seven days.
	TTL time.Duration
	// TrustedRoot, when set, is used instead of the embedded and TUF roots.
	TrustedRoot *root.TrustedRoot
	Logger      *slog.Logger
	Now         func() time.Time

	mu      sync.Mutex
	roots   *root.TrustedRoot
	rootsAt time.Time
	cache   map[string]*Result
}

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

func (v *Verifier) logger() *slog.Logger {
	if v.Logger != nil {
		return v.Logger
	}
	return slog.Default()
}

func (v *Verifier) registry() *oci.Client {
	if v.Registry != nil {
		return v.Registry
	}
	return &oci.Client{}
}

func (v *Verifier) issuer() string {
	if v.Issuer != "" {
		return v.Issuer
	}
	return DefaultIssuer
}

func (v *Verifier) workflowPath() string {
	if v.WorkflowPath != "" {
		return v.WorkflowPath
	}
	return DefaultWorkflowPath
}

func (v *Verifier) builderSAN() *regexp.Regexp {
	if v.BuilderSAN != nil {
		return v.BuilderSAN
	}
	return regexp.MustCompile(DefaultBuilderSAN)
}

func (v *Verifier) ttl() time.Duration {
	if v.TTL > 0 {
		return v.TTL
	}
	return 7 * 24 * time.Hour
}

// Verify checks the image e describes and returns what it found. An error
// means the enclave must be refused.
func (v *Verifier) Verify(ctx context.Context, e Expect) (*Result, error) {
	if err := e.validate(); err != nil {
		return nil, err
	}
	key := v.cacheKey(e)
	if r := v.cached(key); r != nil {
		return r, nil
	}
	tr, err := v.trustedRoot(ctx)
	if err != nil {
		return nil, err
	}
	repo, _ := oci.ParseRepository(e.Repo)
	digest, _ := hex.DecodeString(e.Digest[7:])
	sourceURL := "https://" + e.SourceURI

	// Every Sigstore bundle attached to the digest, from the referrers and
	// from cosign's attestation tag; each check picks the ones it needs.
	bundles, fetchErrs := v.fetchBundles(ctx, repo, e)
	if len(bundles) == 0 {
		return nil, fmt.Errorf("no Sigstore records for %s in %s: %w", e.Digest, e.Repo, errors.Join(fetchErrs...))
	}
	res := &Result{Digest: e.Digest, Release: e.Release, Commit: e.Commit, Repo: e.Repo, SourceURI: e.SourceURI}
	if err := v.verifySignature(tr, bundles, e, digest, sourceURL, res); err != nil {
		return nil, fmt.Errorf("signature over %s: %w", e.Digest, errors.Join(append([]error{err}, fetchErrs...)...))
	}
	if err := v.verifyProvenance(tr, bundles, e, digest, sourceURL, res); err != nil {
		return nil, fmt.Errorf("provenance of %s: %w", e.Digest, errors.Join(append([]error{err}, fetchErrs...)...))
	}
	res.VerifiedAt = v.now()
	v.store(key, res)
	return res, nil
}

// maxReferrers bounds how many manifests attached to one digest are read.
const maxReferrers = 32

// fetchBundles collects the Sigstore bundles attached to the digest:
// bundle layers of the manifests that refer to it, and cosign attestation
// layers (a DSSE envelope with the certificate and Rekor record in the
// layer's annotations) converted to bundles. Fetch and parse problems are
// returned alongside so a refusal can explain itself.
func (v *Verifier) fetchBundles(ctx context.Context, repo oci.Repository, e Expect) ([]*bundle.Bundle, []error) {
	var out []*bundle.Bundle
	var errs []error
	refs, err := v.registry().Referrers(ctx, repo, e.Digest)
	if err != nil {
		errs = append(errs, err)
	}
	if len(refs) > maxReferrers {
		errs = append(errs, fmt.Errorf("%d manifests refer to the digest, reading the first %d", len(refs), maxReferrers))
		refs = refs[:maxReferrers]
	}
	for _, d := range refs {
		m, _, err := v.registry().Manifest(ctx, repo, d.Digest)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, layer := range m.Layers {
			if !strings.HasPrefix(layer.MediaType, bundleMediaTypePrefix) {
				continue
			}
			blob, err := v.registry().Blob(ctx, repo, layer.Digest)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			var b bundle.Bundle
			if err := b.UnmarshalJSON(blob); err != nil {
				errs = append(errs, fmt.Errorf("bundle %s: %w", layer.Digest, err))
				continue
			}
			out = append(out, &b)
		}
	}
	att, _, err := v.registry().Manifest(ctx, repo, "sha256-"+e.Digest[7:]+".att")
	if err != nil && !errors.Is(err, oci.ErrNotFound) {
		errs = append(errs, err)
	}
	if att != nil {
		for _, layer := range att.Layers {
			if layer.MediaType != dsseMediaType {
				continue
			}
			blob, err := v.registry().Blob(ctx, repo, layer.Digest)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			b, err := legacyBundle(blob, layer.Annotations)
			if err != nil {
				errs = append(errs, fmt.Errorf("attestation %s: %w", layer.Digest, err))
				continue
			}
			out = append(out, b)
		}
	}
	return out, errs
}

// verifySignature finds, among the bundles, one whose certificate is the
// release workflow at the release and whose statement is about the digest.
func (v *Verifier) verifySignature(tr *root.TrustedRoot, bundles []*bundle.Bundle, e Expect, digest []byte, sourceURL string, res *Result) error {
	san := sourceURL + "/" + v.workflowPath() + "@refs/tags/" + e.Release
	id, err := v.identity(san, "", sourceURL, e)
	if err != nil {
		return err
	}
	var errs []error
	for _, b := range bundles {
		out, err := v.verifyBundle(tr, b, digest, id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		res.SignatureIdentity = out.Signature.Certificate.SubjectAlternativeName
		res.SignatureLogIndex, res.SignatureTime = logIndexAndTime(b, out)
		return nil
	}
	return fmt.Errorf("none of %d records is a signature by %s: %w", len(bundles), san, errors.Join(errs...))
}

// verifyProvenance finds, among the bundles, SLSA provenance for the digest
// signed by the builder about the release, and reads its statement.
func (v *Verifier) verifyProvenance(tr *root.TrustedRoot, bundles []*bundle.Bundle, e Expect, digest []byte, sourceURL string, res *Result) error {
	id, err := v.identity("", v.builderSAN().String(), sourceURL, e)
	if err != nil {
		return err
	}
	var errs []error
	for _, b := range bundles {
		out, err := v.verifyBundle(tr, b, digest, id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := v.checkStatement(out, e, sourceURL, res); err != nil {
			errs = append(errs, err)
			continue
		}
		res.Builder = out.Signature.Certificate.SubjectAlternativeName
		res.PredicateType = out.Statement.PredicateType
		res.ProvenanceLogIndex, res.ProvenanceTime = logIndexAndTime(b, out)
		return nil
	}
	return fmt.Errorf("none of %d records is SLSA provenance by the builder for %s at %s: %w", len(bundles), e.SourceURI, e.Release, errors.Join(errs...))
}

// identity is the certificate the signer must have used: issued by the
// GitHub Actions issuer to the given SAN (or one matching sanRegexp), for
// a run on the enclave repository at the release tag and, when the token
// named it, the commit.
func (v *Verifier) identity(san, sanRegexp, sourceURL string, e Expect) (verify.CertificateIdentity, error) {
	sanMatcher, err := verify.NewSANMatcher(san, sanRegexp)
	if err != nil {
		return verify.CertificateIdentity{}, err
	}
	issuer, err := verify.NewIssuerMatcher(v.issuer(), "")
	if err != nil {
		return verify.CertificateIdentity{}, err
	}
	return verify.NewCertificateIdentity(sanMatcher, issuer, certificate.Extensions{
		SourceRepositoryURI:    sourceURL,
		SourceRepositoryRef:    "refs/tags/" + e.Release,
		SourceRepositoryDigest: e.Commit,
	})
}

// verifyBundle runs sigstore-go: the certificate chains to Fulcio and was
// logged in a CT log, the signature verifies over the statement, the
// statement's subject is the digest, the Rekor entry (and timestamp, when
// present) checks against the trust root, and the certificate is id.
func (v *Verifier) verifyBundle(tr *root.TrustedRoot, b *bundle.Bundle, digest []byte, id verify.CertificateIdentity) (*verify.VerificationResult, error) {
	sv, err := verify.NewVerifier(tr,
		verify.WithSignedCertificateTimestamps(1),
		verify.WithTransparencyLog(1),
		verify.WithObserverTimestamps(1),
	)
	if err != nil {
		return nil, err
	}
	return sv.Verify(b, verify.NewPolicy(verify.WithArtifactDigest("sha256", digest), verify.WithCertificateIdentity(id)))
}

// legacyBundle turns a cosign attestation layer (a DSSE envelope, with the
// Fulcio certificate and the Rekor record in the layer's annotations) into
// a v0.1 Sigstore bundle, which sigstore-go verifies like any other.
func legacyBundle(envelope []byte, ann map[string]string) (*bundle.Bundle, error) {
	var env struct {
		PayloadType string `json:"payloadType"`
		Payload     string `json:"payload"`
		Signatures  []struct {
			KeyID string `json:"keyid"`
			Sig   string `json:"sig"`
		} `json:"signatures"`
	}
	if err := json.Unmarshal(envelope, &env); err != nil {
		return nil, fmt.Errorf("envelope: %w", err)
	}
	if len(env.Signatures) == 0 {
		return nil, errors.New("envelope has no signature")
	}
	payload, err := base64.StdEncoding.DecodeString(env.Payload)
	if err != nil {
		return nil, fmt.Errorf("envelope payload: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(env.Signatures[0].Sig)
	if err != nil {
		return nil, fmt.Errorf("envelope signature: %w", err)
	}
	block, _ := pem.Decode([]byte(ann["dev.sigstore.cosign/certificate"]))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("no certificate annotation")
	}
	var rekor struct {
		SignedEntryTimestamp string `json:"SignedEntryTimestamp"`
		Payload              struct {
			Body           string `json:"body"`
			IntegratedTime int64  `json:"integratedTime"`
			LogIndex       int64  `json:"logIndex"`
			LogID          string `json:"logID"`
		} `json:"Payload"`
	}
	if err := json.Unmarshal([]byte(ann["dev.sigstore.cosign/bundle"]), &rekor); err != nil {
		return nil, fmt.Errorf("rekor record annotation: %w", err)
	}
	set, err := base64.StdEncoding.DecodeString(rekor.SignedEntryTimestamp)
	if err != nil {
		return nil, fmt.Errorf("rekor record: %w", err)
	}
	body, err := base64.StdEncoding.DecodeString(rekor.Payload.Body)
	if err != nil {
		return nil, fmt.Errorf("rekor record body: %w", err)
	}
	logID, err := hex.DecodeString(rekor.Payload.LogID)
	if err != nil {
		return nil, fmt.Errorf("rekor record log id: %w", err)
	}
	var entry struct {
		Kind       string `json:"kind"`
		APIVersion string `json:"apiVersion"`
	}
	if err := json.Unmarshal(body, &entry); err != nil || entry.Kind == "" || entry.APIVersion == "" {
		return nil, errors.New("rekor record body is not an entry")
	}
	pb := &protobundle.Bundle{
		MediaType: bundleV01MediaType,
		VerificationMaterial: &protobundle.VerificationMaterial{
			Content: &protobundle.VerificationMaterial_X509CertificateChain{X509CertificateChain: &protocommon.X509CertificateChain{
				Certificates: []*protocommon.X509Certificate{{RawBytes: block.Bytes}},
			}},
			TlogEntries: []*protorekor.TransparencyLogEntry{{
				LogIndex:          rekor.Payload.LogIndex,
				LogId:             &protocommon.LogId{KeyId: logID},
				KindVersion:       &protorekor.KindVersion{Kind: entry.Kind, Version: entry.APIVersion},
				IntegratedTime:    rekor.Payload.IntegratedTime,
				InclusionPromise:  &protorekor.InclusionPromise{SignedEntryTimestamp: set},
				CanonicalizedBody: body,
			}},
		},
		Content: &protobundle.Bundle_DsseEnvelope{DsseEnvelope: &protodsse.Envelope{
			Payload:     payload,
			PayloadType: env.PayloadType,
			Signatures:  []*protodsse.Signature{{Sig: sig, Keyid: env.Signatures[0].KeyID}},
		}},
	}
	return bundle.NewBundle(pb)
}

// checkStatement reads the verified provenance: the builder is the SLSA
// generator, and the build was configured by the release workflow in the
// enclave repository at the release tag and commit. SLSA v0.2 (what the
// generator produces today) and v1 are both understood.
func (v *Verifier) checkStatement(out *verify.VerificationResult, e Expect, sourceURL string, res *Result) error {
	if out.Statement == nil {
		return errors.New("no in-toto statement")
	}
	raw, err := json.Marshal(out.Statement.Predicate)
	if err != nil {
		return err
	}
	wantRef := "refs/tags/" + e.Release
	wantURI := "git+" + sourceURL + "@" + wantRef
	// What the build was configured from, in either predicate's terms.
	var builder, repoURL, ref, entryPoint, commit string
	switch out.Statement.PredicateType {
	case "https://slsa.dev/provenance/v0.2":
		var p struct {
			Builder struct {
				ID string `json:"id"`
			} `json:"builder"`
			Invocation struct {
				ConfigSource struct {
					URI        string            `json:"uri"`
					Digest     map[string]string `json:"digest"`
					EntryPoint string            `json:"entryPoint"`
				} `json:"configSource"`
			} `json:"invocation"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return fmt.Errorf("provenance predicate: %w", err)
		}
		cs := p.Invocation.ConfigSource
		builder, entryPoint, commit = p.Builder.ID, cs.EntryPoint, cs.Digest["sha1"]
		repoURL, ref, _ = strings.Cut(strings.TrimPrefix(cs.URI, "git+"), "@")
	case "https://slsa.dev/provenance/v1":
		var p struct {
			BuildDefinition struct {
				ExternalParameters struct {
					Workflow struct {
						Ref        string `json:"ref"`
						Repository string `json:"repository"`
						Path       string `json:"path"`
					} `json:"workflow"`
				} `json:"externalParameters"`
				ResolvedDependencies []struct {
					URI    string            `json:"uri"`
					Digest map[string]string `json:"digest"`
				} `json:"resolvedDependencies"`
			} `json:"buildDefinition"`
			RunDetails struct {
				Builder struct {
					ID string `json:"id"`
				} `json:"builder"`
			} `json:"runDetails"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return fmt.Errorf("provenance predicate: %w", err)
		}
		w := p.BuildDefinition.ExternalParameters.Workflow
		builder, repoURL, ref, entryPoint = p.RunDetails.Builder.ID, w.Repository, w.Ref, w.Path
		for _, d := range p.BuildDefinition.ResolvedDependencies {
			if d.URI == wantURI {
				commit = d.Digest["gitCommit"]
			}
		}
	default:
		return fmt.Errorf("predicate %s is not SLSA provenance", out.Statement.PredicateType)
	}
	if !v.builderSAN().MatchString(builder) {
		return fmt.Errorf("provenance builder is %q", builder)
	}
	if repoURL != sourceURL || ref != wantRef {
		return fmt.Errorf("provenance was built from %s at %s, the token names %s at %s", repoURL, ref, sourceURL, wantRef)
	}
	if entryPoint != v.workflowPath() {
		return fmt.Errorf("provenance entry point is %q, not %s", entryPoint, v.workflowPath())
	}
	if commit == "" {
		return errors.New("provenance names no source commit")
	}
	if e.Commit != "" && commit != e.Commit {
		return fmt.Errorf("provenance commit %s is not the attested %s", commit, e.Commit)
	}
	res.Commit = commit
	return nil
}

// logIndexAndTime reads the Rekor entry index the bundle carries and the
// time the verifier accepted for it.
func logIndexAndTime(b *bundle.Bundle, out *verify.VerificationResult) (int64, time.Time) {
	var index int64 = -1
	if entries, err := b.TlogEntries(); err == nil && len(entries) > 0 {
		index = entries[0].LogIndex()
	}
	var at time.Time
	for _, ts := range out.VerifiedTimestamps {
		if at.IsZero() || ts.Timestamp.Before(at) {
			at = ts.Timestamp
		}
	}
	return index, at
}

// trustedRoot returns the Sigstore trust root: TrustedRoot when set, else
// the TUF-refreshed copy under CacheDir when it can be fetched, else the
// embedded copy. The choice is cached for a day.
func (v *Verifier) trustedRoot(ctx context.Context) (*root.TrustedRoot, error) {
	if v.TrustedRoot != nil {
		return v.TrustedRoot, nil
	}
	v.mu.Lock()
	if v.roots != nil && v.now().Sub(v.rootsAt) < 24*time.Hour {
		defer v.mu.Unlock()
		return v.roots, nil
	}
	v.mu.Unlock()

	data := embeddedTrustedRoot
	source := "embedded"
	if v.CacheDir != "" {
		fresh, err := fetchTrustedRoot(ctx, filepath.Join(v.CacheDir, "sigstore-tuf"))
		if err != nil {
			v.logger().Debug("sigstore trust root: TUF refresh failed, using the embedded copy", "err", err)
		} else {
			data, source = fresh, "tuf"
		}
	}
	tr, err := root.NewTrustedRootFromJSON(data)
	if err != nil && source == "tuf" {
		v.logger().Warn("sigstore trust root from TUF does not parse, using the embedded copy", "err", err)
		tr, err = root.NewTrustedRootFromJSON(embeddedTrustedRoot)
	}
	if err != nil {
		return nil, fmt.Errorf("sigstore trust root: %w", err)
	}
	v.mu.Lock()
	v.roots, v.rootsAt = tr, v.now()
	v.mu.Unlock()
	return tr, nil
}

// fetchTrustedRoot refreshes the Sigstore TUF repository into dir and
// returns the current trusted_root.json, giving up after a short while so
// a dial is not held up by a slow CDN.
func fetchTrustedRoot(ctx context.Context, dir string) ([]byte, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		client, err := tuf.New(tuf.DefaultOptions().WithCachePath(dir).WithCacheValidity(1).WithContext(ctx))
		if err != nil {
			ch <- result{nil, err}
			return
		}
		data, err := client.GetTarget("trusted_root.json")
		ch <- result{data, err}
	}()
	select {
	case r := <-ch:
		return r.data, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Cache: verified results by digest, release, commit and the checks'
// configuration, in memory and (with CacheDir) on disk, for TTL.

func (v *Verifier) cacheKey(e Expect) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		e.Digest, e.Release, e.Commit, e.Repo, e.SourceURI,
		v.issuer(), v.workflowPath(), v.builderSAN().String(),
	}, "\n")))
	return hex.EncodeToString(sum[:16])
}

func (v *Verifier) cached(key string) *Result {
	v.mu.Lock()
	r, ok := v.cache[key]
	v.mu.Unlock()
	if !ok && v.CacheDir != "" {
		data, err := os.ReadFile(filepath.Join(v.CacheDir, "provenance", key+".json"))
		if err != nil {
			return nil
		}
		var disk Result
		if err := json.Unmarshal(data, &disk); err != nil {
			return nil
		}
		r = &disk
	}
	if r == nil || v.now().Sub(r.VerifiedAt) > v.ttl() || r.VerifiedAt.After(v.now().Add(time.Hour)) {
		return nil
	}
	c := *r
	c.Cached = true
	return &c
}

func (v *Verifier) store(key string, r *Result) {
	v.mu.Lock()
	if v.cache == nil {
		v.cache = map[string]*Result{}
	}
	v.cache[key] = r
	v.mu.Unlock()
	if v.CacheDir == "" {
		return
	}
	dir := filepath.Join(v.CacheDir, "provenance")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	data, err := json.Marshal(r)
	if err != nil {
		return
	}
	tmp := filepath.Join(dir, key+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err == nil {
		_ = os.Rename(tmp, filepath.Join(dir, key+".json"))
	}
}
