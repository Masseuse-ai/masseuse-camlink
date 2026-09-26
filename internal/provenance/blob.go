package provenance

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// The connector's own releases are checked the way VERIFY.md tells a
// reader to check them, but by the running program when it updates itself
// (internal/update): the checksum file's keyless signature was made by the
// release workflow of this repository at a release tag, and the SLSA
// provenance for an artifact was made by the SLSA generic generator for a
// build configured by that workflow at that tag. Both are Sigstore bundles
// (cosign's `sign-blob --bundle`; the generator's `*.intoto.jsonl`), with
// the certificate and the Rekor entry inside, so they verify offline
// against the trust root like the enclave image's records do.

// DefaultGenericBuilderSAN is the identity of the SLSA GitHub generator for
// files (the generic generator) at any release of the generator.
const DefaultGenericBuilderSAN = `^https://github\.com/slsa-framework/slsa-github-generator/\.github/workflows/generator_generic_slsa3\.yml@refs/tags/v\d+\.\d+\.\d+$`

// BlobSigner names who must have signed a blob: the release workflow
// (WorkflowPath, DefaultWorkflowPath when empty) of the repository at
// SourceURI, for a run on the tag Tag, or, when Tag is empty, on any tag
// matching TagPattern (a release tag, vX.Y.Z, when that is empty too). The
// tag the certificate names is what VerifyBlob reports, so a caller that
// does not know the release yet learns it from the signature and never
// from a file name.
type BlobSigner struct {
	SourceURI    string
	WorkflowPath string
	Tag          string
	TagPattern   string
}

func (s BlobSigner) validate() error {
	if s.SourceURI == "" || strings.Contains(s.SourceURI, "://") || strings.HasSuffix(s.SourceURI, "/") {
		return fmt.Errorf("source repository %q is not host/owner/name", s.SourceURI)
	}
	if s.Tag != "" && !releaseRE.MatchString(s.Tag) {
		return fmt.Errorf("tag %q is not a release tag", s.Tag)
	}
	if s.Tag == "" && s.TagPattern != "" {
		if _, err := regexp.Compile(s.TagPattern); err != nil {
			return fmt.Errorf("tag pattern: %w", err)
		}
	}
	return nil
}

func (s BlobSigner) workflowPath(v *Verifier) string {
	if s.WorkflowPath != "" {
		return s.WorkflowPath
	}
	return v.workflowPath()
}

// BlobResult is what a verified blob signature or provenance says.
type BlobResult struct {
	// Tag is the release tag the signing run was on (the certificate's
	// source repository ref, without refs/tags/).
	Tag string `json:"tag"`
	// Commit is the source commit of that run, when the certificate names it.
	Commit string `json:"commit,omitempty"`
	// Identity is the signing certificate's SAN: the workflow at the tag,
	// or the builder for provenance.
	Identity string    `json:"identity"`
	LogIndex int64     `json:"logIndex"`
	Time     time.Time `json:"time"`
}

// VerifyBlob checks a Sigstore bundle (cosign `sign-blob --bundle`) over
// blob: the certificate chains to Fulcio, was logged, and was issued to
// signer's workflow by the GitHub Actions issuer for a run at the tag (or
// a tag matching the pattern); the signature verifies over blob's
// SHA-256; the Rekor entry checks against the trust root.
func (v *Verifier) VerifyBlob(ctx context.Context, blob, bundleJSON []byte, signer BlobSigner) (*BlobResult, error) {
	if err := signer.validate(); err != nil {
		return nil, err
	}
	tr, err := v.trustedRoot(ctx)
	if err != nil {
		return nil, err
	}
	var b bundle.Bundle
	if err := b.UnmarshalJSON(bundleJSON); err != nil {
		return nil, fmt.Errorf("signature bundle: %w", err)
	}
	sourceURL := "https://" + signer.SourceURI
	prefix := sourceURL + "/" + signer.workflowPath(v) + "@refs/tags/"
	var san, sanRegexp string
	if signer.Tag != "" {
		san = prefix + signer.Tag
	} else {
		pattern := signer.TagPattern
		if pattern == "" {
			pattern = strings.TrimSuffix(strings.TrimPrefix(releaseRE.String(), "^"), "$")
		}
		sanRegexp = "^" + regexp.QuoteMeta(prefix) + "(" + pattern + ")$"
	}
	id, err := v.identity(san, sanRegexp, sourceURL, Expect{Release: signer.Tag})
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(blob)
	out, err := v.verifyBundle(tr, &b, digest[:], id)
	if err != nil {
		return nil, err
	}
	return blobResult(&b, out, prefix)
}

// VerifyBlobProvenance checks SLSA provenance (the generic generator's
// bundle, one line of `*.intoto.jsonl`) for one artifact: the certificate
// is the generic generator's (BuilderSAN, DefaultGenericBuilderSAN when
// unset) for a run on signer's repository at signer.Tag; the statement
// names the artifact by name and SHA-256 among its subjects; the predicate
// says the build was configured by signer's release workflow at that tag.
// signer.Tag is required: the artifact's release is known by then.
func (v *Verifier) VerifyBlobProvenance(ctx context.Context, name string, digest []byte, bundleJSON []byte, signer BlobSigner) (*BlobResult, error) {
	if err := signer.validate(); err != nil {
		return nil, err
	}
	if signer.Tag == "" {
		return nil, errors.New("provenance needs the release tag")
	}
	if len(digest) != sha256.Size {
		return nil, errors.New("digest is not a SHA-256")
	}
	tr, err := v.trustedRoot(ctx)
	if err != nil {
		return nil, err
	}
	var b bundle.Bundle
	if err := b.UnmarshalJSON(bundleJSON); err != nil {
		return nil, fmt.Errorf("provenance bundle: %w", err)
	}
	sourceURL := "https://" + signer.SourceURI
	builderSAN := v.BuilderSAN
	if builderSAN == nil {
		builderSAN = regexp.MustCompile(DefaultGenericBuilderSAN)
	}
	id, err := v.identity("", builderSAN.String(), sourceURL, Expect{Release: signer.Tag})
	if err != nil {
		return nil, err
	}
	out, err := v.verifyBundle(tr, &b, digest, id)
	if err != nil {
		return nil, err
	}
	if out.Statement == nil {
		return nil, errors.New("no in-toto statement")
	}
	named := false
	for _, s := range out.Statement.Subject {
		if s.Name == name && strings.EqualFold(s.Digest["sha256"], fmt.Sprintf("%x", digest)) {
			named = true
			break
		}
	}
	if !named {
		return nil, fmt.Errorf("the provenance does not name %s with that digest among its %d subjects", name, len(out.Statement.Subject))
	}
	res := &Result{}
	if err := v.checkStatementWith(out, Expect{Release: signer.Tag}, sourceURL, builderSAN, signer.workflowPath(v), res); err != nil {
		return nil, err
	}
	r, err := blobResult(&b, out, sourceURL+"/"+signer.workflowPath(v)+"@refs/tags/")
	if err != nil {
		return nil, err
	}
	r.Commit = res.Commit
	return r, nil
}

// blobResult reads the tag, the commit, the identity and the log record
// off a verified bundle. prefix is the SAN's part before the tag, for a
// signature by the workflow; a builder's certificate names the tag in its
// source repository ref instead.
func blobResult(b *bundle.Bundle, out *verify.VerificationResult, prefix string) (*BlobResult, error) {
	cert := out.Signature.Certificate
	ref := cert.SourceRepositoryRef
	tag := strings.TrimPrefix(ref, "refs/tags/")
	if tag == ref || tag == "" {
		if strings.HasPrefix(cert.SubjectAlternativeName, prefix) {
			tag = strings.TrimPrefix(cert.SubjectAlternativeName, prefix)
		}
	}
	if !releaseRE.MatchString(tag) {
		return nil, fmt.Errorf("the certificate names no release tag (ref %q)", ref)
	}
	index, at := logIndexAndTime(b, out)
	return &BlobResult{
		Tag:      tag,
		Commit:   cert.SourceRepositoryDigest,
		Identity: cert.SubjectAlternativeName,
		LogIndex: index,
		Time:     at,
	}, nil
}
