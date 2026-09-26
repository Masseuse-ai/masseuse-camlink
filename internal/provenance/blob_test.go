package provenance

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sigstore/sigstore-go/pkg/root"
)

// The connector's own release v0.10.0, as published: the checksum files
// with cosign's bundles over them and the SLSA generic generator's
// provenance, verified offline against the embedded trust root.
const releaseDir = "testdata/release"

func releaseFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(releaseDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func releaseVerifier(t *testing.T) *Verifier {
	t.Helper()
	tr, err := root.NewTrustedRootFromJSON(embeddedTrustedRoot)
	if err != nil {
		t.Fatal(err)
	}
	return &Verifier{TrustedRoot: tr, Now: func() time.Time { return time.Date(2026, 9, 16, 17, 0, 0, 0, time.UTC) }}
}

const connectorRepo = "github.com/FemLed/masseuse-camlink"

// checksumOf reads an artifact's SHA-256 from a checksum file.
func checksumOf(t *testing.T, checksums []byte, name string) []byte {
	t.Helper()
	for _, line := range strings.Split(string(checksums), "\n") {
		if strings.HasSuffix(line, "  "+name) {
			d, err := hex.DecodeString(line[:64])
			if err != nil {
				t.Fatal(err)
			}
			return d
		}
	}
	t.Fatalf("%s not in the checksums", name)
	return nil
}

func TestVerifyBlobReadsTheTagFromTheCertificate(t *testing.T) {
	v := releaseVerifier(t)
	ctx := context.Background()
	for _, f := range []string{"checksums.txt", "checksums-darwin.txt", "checksums-windows.txt"} {
		res, err := v.VerifyBlob(ctx, releaseFile(t, f), releaseFile(t, f+".sigstore.json"), BlobSigner{SourceURI: connectorRepo})
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if res.Tag != "v0.10.0" {
			t.Fatalf("%s: tag %q", f, res.Tag)
		}
		if res.Identity != "https://github.com/FemLed/masseuse-camlink/.github/workflows/release.yml@refs/tags/v0.10.0" {
			t.Fatalf("%s: identity %q", f, res.Identity)
		}
		if len(res.Commit) != 40 || res.LogIndex < 0 || res.Time.IsZero() {
			t.Fatalf("%s: %+v", f, res)
		}
	}
	// The exact tag, when known, is accepted; another is not.
	if _, err := v.VerifyBlob(ctx, releaseFile(t, "checksums.txt"), releaseFile(t, "checksums.txt.sigstore.json"), BlobSigner{SourceURI: connectorRepo, Tag: "v0.10.0"}); err != nil {
		t.Fatal(err)
	}
	if _, err := v.VerifyBlob(ctx, releaseFile(t, "checksums.txt"), releaseFile(t, "checksums.txt.sigstore.json"), BlobSigner{SourceURI: connectorRepo, Tag: "v0.9.4"}); err == nil {
		t.Fatal("a signature from another tag's run was accepted")
	}
}

func TestVerifyBlobRefuses(t *testing.T) {
	v := releaseVerifier(t)
	ctx := context.Background()
	checksums := releaseFile(t, "checksums.txt")
	bundle := releaseFile(t, "checksums.txt.sigstore.json")
	// A checksum line changed: the blob is no longer what was signed.
	tampered := []byte(strings.Replace(string(checksums), "masseuse-camlink_0.10.0_linux_amd64.tar.gz", "masseuse-camlink_0.10.0_linux_amd64.tar.gz ", 1))
	if _, err := v.VerifyBlob(ctx, tampered, bundle, BlobSigner{SourceURI: connectorRepo}); err == nil {
		t.Fatal("a tampered checksum file verified")
	}
	// Another repository's release workflow would not do.
	if _, err := v.VerifyBlob(ctx, checksums, bundle, BlobSigner{SourceURI: "github.com/FemLed/masseuse-video-tee"}); err == nil {
		t.Fatal("a signature by another repository's workflow was accepted")
	}
	// Another workflow of the same repository would not do.
	if _, err := v.VerifyBlob(ctx, checksums, bundle, BlobSigner{SourceURI: connectorRepo, WorkflowPath: ".github/workflows/ci.yml"}); err == nil {
		t.Fatal("a signature by another workflow was accepted")
	}
	// A bundle over another file does not cover this one.
	if _, err := v.VerifyBlob(ctx, checksums, releaseFile(t, "checksums-darwin.txt.sigstore.json"), BlobSigner{SourceURI: connectorRepo}); err == nil {
		t.Fatal("another file's bundle verified this file")
	}
	// A tag pattern that does not match the run's tag.
	if _, err := v.VerifyBlob(ctx, checksums, bundle, BlobSigner{SourceURI: connectorRepo, TagPattern: `v1\.[0-9]+\.[0-9]+`}); err == nil {
		t.Fatal("a tag outside the pattern was accepted")
	}
	// Malformed input.
	if _, err := v.VerifyBlob(ctx, checksums, []byte("{"), BlobSigner{SourceURI: connectorRepo}); err == nil {
		t.Fatal("a broken bundle verified")
	}
	if _, err := v.VerifyBlob(ctx, checksums, bundle, BlobSigner{SourceURI: "https://github.com/FemLed/masseuse-camlink"}); err == nil {
		t.Fatal("a URL was taken for a repository")
	}
	if _, err := v.VerifyBlob(ctx, checksums, bundle, BlobSigner{SourceURI: connectorRepo, Tag: "0.10.0"}); err == nil {
		t.Fatal("a tag without its v was taken")
	}
}

func TestVerifyBlobProvenanceNamesTheArtifact(t *testing.T) {
	v := releaseVerifier(t)
	ctx := context.Background()
	signer := BlobSigner{SourceURI: connectorRepo, Tag: "v0.10.0"}
	cases := []struct{ checksums, provenance, artifact string }{
		{"checksums.txt", "multiple.intoto.jsonl", "masseuse-camlink_0.10.0_linux_amd64.tar.gz"},
		{"checksums.txt", "multiple.intoto.jsonl", "masseuse-camlink_0.10.0_windows_arm64.zip"},
		{"checksums-darwin.txt", "darwin.intoto.jsonl", "Masseuse.ai-0.10.0.dmg"},
		{"checksums-windows.txt", "windows.intoto.jsonl", "Masseuse.ai-0.10.0-windows.zip"},
	}
	for _, c := range cases {
		digest := checksumOf(t, releaseFile(t, c.checksums), c.artifact)
		res, err := v.VerifyBlobProvenance(ctx, c.artifact, digest, releaseFile(t, c.provenance), signer)
		if err != nil {
			t.Fatalf("%s: %v", c.artifact, err)
		}
		if res.Tag != "v0.10.0" || len(res.Commit) != 40 || !strings.Contains(res.Identity, "generator_generic_slsa3.yml@refs/tags/v") {
			t.Fatalf("%s: %+v", c.artifact, res)
		}
	}
	linux := checksumOf(t, releaseFile(t, "checksums.txt"), "masseuse-camlink_0.10.0_linux_amd64.tar.gz")
	prov := releaseFile(t, "multiple.intoto.jsonl")
	// The right digest under another name, or another digest under the right name.
	if _, err := v.VerifyBlobProvenance(ctx, "masseuse-camlink_0.10.0_linux_arm64.tar.gz", linux, prov, signer); err == nil {
		t.Fatal("a subject name that does not carry the digest was accepted")
	}
	other := append([]byte(nil), linux...)
	other[0] ^= 1
	if _, err := v.VerifyBlobProvenance(ctx, "masseuse-camlink_0.10.0_linux_amd64.tar.gz", other, prov, signer); err == nil {
		t.Fatal("a digest the provenance does not name was accepted")
	}
	// The provenance of another release, or for another repository.
	if _, err := v.VerifyBlobProvenance(ctx, "masseuse-camlink_0.10.0_linux_amd64.tar.gz", linux, prov, BlobSigner{SourceURI: connectorRepo, Tag: "v0.9.4"}); err == nil {
		t.Fatal("provenance for another tag was accepted")
	}
	if _, err := v.VerifyBlobProvenance(ctx, "masseuse-camlink_0.10.0_linux_amd64.tar.gz", linux, prov, BlobSigner{SourceURI: "github.com/FemLed/masseuse-video-tee", Tag: "v0.10.0"}); err == nil {
		t.Fatal("provenance for another repository was accepted")
	}
	// The darwin provenance does not cover the archives, and the tag is required.
	if _, err := v.VerifyBlobProvenance(ctx, "masseuse-camlink_0.10.0_linux_amd64.tar.gz", linux, releaseFile(t, "darwin.intoto.jsonl"), signer); err == nil {
		t.Fatal("another artifact set's provenance was accepted")
	}
	if _, err := v.VerifyBlobProvenance(ctx, "masseuse-camlink_0.10.0_linux_amd64.tar.gz", linux, prov, BlobSigner{SourceURI: connectorRepo}); err == nil {
		t.Fatal("provenance without a tag was accepted")
	}
}
