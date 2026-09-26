package provenance

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sigstore/sigstore-go/pkg/root"

	"github.com/FemLed/masseuse-camlink/internal/oci"
)

// The fixtures under testdata are the records cosign and the SLSA
// generator attached to masseuse-video-tee v0.4.0 on ghcr.io, saved as
// served: the referrers tag index, the bundle referrer manifest and its
// bundle, and the attestation tag manifest and its DSSE envelope.
const (
	fixtureDigest    = "sha256:9277d7b7b2a2f170098eda4e42eac8c99f1b93554980fe3d0e67874b69f5da0a"
	fixtureRelease   = "v0.4.0"
	fixtureCommit    = "fb4d06cb319eee270c6a4adfa02f9ebe13e9255e"
	fixtureRepoPath  = "femled/masseuse-video-tee"
	fixtureSource    = "github.com/FemLed/masseuse-video-tee"
	fixtureSigner    = "https://github.com/FemLed/masseuse-video-tee/.github/workflows/release.yml@refs/tags/v0.4.0"
	fixtureBuilder   = "https://github.com/slsa-framework/slsa-github-generator/.github/workflows/generator_container_slsa3.yml@refs/tags/v2.1.0"
	fixtureSigIndex  = 2780826360
	fixtureProvIndex = 2780831131
	refmanDigest     = "sha256:ed3003f65e55bc14c1b8595e7c3c0f96a948ec6c8cfa8b1876fcc002cda62e08"
	bundleBlobDigest = "sha256:5cd4a6b74fb80b2d8f6d232d0ea1bb49fbba660d5b6a079acc76f7cdf160a0a9"
	attBlobDigest    = "sha256:5b7d152297d9a0cffa934d702554fd9ed9e63222b664e063971def5159a9748e"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// registry is a fake of the part of a registry the verifier uses. Paths
// are relative to /v2/<repo>; bodies can be overridden per path.
type registry struct {
	t            *testing.T
	srv          *httptest.Server
	referrersAPI bool // answer the referrers API; otherwise 404 so the tag is used
	noAtt        bool // no attestation tag

	mu     sync.Mutex
	bodies map[string][]byte
	hits   map[string]int
	tokens int
}

func newRegistry(t *testing.T) *registry {
	t.Helper()
	hexDigest := strings.TrimPrefix(fixtureDigest, "sha256:")
	r := &registry{t: t, hits: map[string]int{}, bodies: map[string][]byte{
		"/manifests/sha256-" + hexDigest:          fixture(t, "tagidx.json"),
		"/manifests/" + refmanDigest:              fixture(t, "refman.json"),
		"/manifests/sha256-" + hexDigest + ".att": fixture(t, "att.json"),
		"/blobs/" + bundleBlobDigest:              fixture(t, "bundle.json"),
		"/blobs/" + attBlobDigest:                 fixture(t, "att-env.json"),
	}}
	r.srv = httptest.NewTLSServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *registry) repo() string { return r.srv.Listener.Addr().String() + "/" + fixtureRepoPath }

func (r *registry) verifier() *Verifier {
	return &Verifier{Registry: &oci.Client{HTTP: r.srv.Client()}}
}

func (r *registry) expect() Expect {
	return Expect{Digest: fixtureDigest, Release: fixtureRelease, Commit: fixtureCommit, Repo: r.repo(), SourceURI: fixtureSource}
}

func (r *registry) set(path string, body []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bodies[path] = body
}

func (r *registry) count(path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits[path]
}

func (r *registry) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if req.URL.Path == "/token" {
		r.tokens++
		if req.URL.Query().Get("scope") != "repository:"+fixtureRepoPath+":pull" {
			http.Error(w, "bad scope", http.StatusBadRequest)
			return
		}
		_, _ = fmt.Fprint(w, `{"token":"anon"}`)
		return
	}
	prefix := "/v2/" + fixtureRepoPath
	if !strings.HasPrefix(req.URL.Path, prefix) {
		http.NotFound(w, req)
		return
	}
	if req.Header.Get("Authorization") != "Bearer anon" {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="https://%s/token",service="test",scope="repository:%s:pull"`, req.Host, fixtureRepoPath))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	path := strings.TrimPrefix(req.URL.Path, prefix)
	r.hits[path]++
	if strings.HasPrefix(path, "/referrers/") {
		if !r.referrersAPI || path != "/referrers/"+fixtureDigest {
			http.NotFound(w, req)
			return
		}
		// The API returns the manifests' own artifact types and
		// annotations, unlike the tag index.
		var m oci.Manifest
		_ = json.Unmarshal(r.bodies["/manifests/"+refmanDigest], &m)
		idx := map[string]any{"schemaVersion": 2, "mediaType": oci.MediaTypeIndex, "manifests": []oci.Descriptor{{
			MediaType: oci.MediaTypeManifest, Digest: refmanDigest, Size: int64(len(r.bodies["/manifests/"+refmanDigest])),
			ArtifactType: m.ArtifactType, Annotations: map[string]string{"dev.sigstore.bundle.predicateType": "https://sigstore.dev/cosign/sign/v1"},
		}}}
		w.Header().Set("Content-Type", oci.MediaTypeIndex)
		_ = json.NewEncoder(w).Encode(idx)
		return
	}
	if r.noAtt && strings.HasSuffix(path, ".att") {
		http.NotFound(w, req)
		return
	}
	body, ok := r.bodies[path]
	if !ok {
		http.NotFound(w, req)
		return
	}
	_, _ = w.Write(body)
}

func checkResult(t *testing.T, res *Result) {
	t.Helper()
	if res.SignatureIdentity != fixtureSigner {
		t.Errorf("signer %q", res.SignatureIdentity)
	}
	if res.SignatureLogIndex != fixtureSigIndex {
		t.Errorf("signature log index %d", res.SignatureLogIndex)
	}
	if res.Builder != fixtureBuilder {
		t.Errorf("builder %q", res.Builder)
	}
	if res.PredicateType != "https://slsa.dev/provenance/v0.2" {
		t.Errorf("predicate %q", res.PredicateType)
	}
	if res.ProvenanceLogIndex != fixtureProvIndex {
		t.Errorf("provenance log index %d", res.ProvenanceLogIndex)
	}
	if res.Commit != fixtureCommit {
		t.Errorf("commit %q", res.Commit)
	}
	if res.SignatureTime.IsZero() || res.ProvenanceTime.IsZero() || res.SignatureTime.Year() != 2026 {
		t.Errorf("times %v %v", res.SignatureTime, res.ProvenanceTime)
	}
	if res.Cached {
		t.Error("fresh result marked cached")
	}
}

func TestVerifyThroughTheReferrersTag(t *testing.T) {
	r := newRegistry(t)
	res, err := r.verifier().Verify(context.Background(), r.expect())
	if err != nil {
		t.Fatal(err)
	}
	checkResult(t, res)
	if r.tokens != 1 {
		t.Errorf("%d token requests, want 1", r.tokens)
	}
	if r.count("/referrers/"+fixtureDigest) != 1 || r.count("/manifests/sha256-"+fixtureDigest[7:]) != 1 {
		t.Errorf("hits %v", r.hits)
	}
}

func TestVerifyThroughTheReferrersAPI(t *testing.T) {
	r := newRegistry(t)
	r.referrersAPI = true
	res, err := r.verifier().Verify(context.Background(), r.expect())
	if err != nil {
		t.Fatal(err)
	}
	checkResult(t, res)
	if r.count("/manifests/sha256-"+fixtureDigest[7:]) != 0 {
		t.Error("fell back to the tag although the API answered")
	}
}

func TestVerifyLearnsTheCommitWhenTheTokenHasNone(t *testing.T) {
	r := newRegistry(t)
	e := r.expect()
	e.Commit = ""
	res, err := r.verifier().Verify(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	checkResult(t, res)
}

// tamper decodes a DSSE envelope's payload, applies f to the statement
// text and re-encodes it: the signature no longer covers the payload.
func tamperEnvelope(t *testing.T, env map[string]any, f func(string) string) {
	t.Helper()
	payload, err := base64.StdEncoding.DecodeString(env["payload"].(string))
	if err != nil {
		t.Fatal(err)
	}
	changed := f(string(payload))
	if changed == string(payload) {
		t.Fatal("tamper changed nothing")
	}
	env["payload"] = base64.StdEncoding.EncodeToString([]byte(changed))
}

func TestVerifyRefuses(t *testing.T) {
	otherCommit := strings.Repeat("ab", 20)
	cases := []struct {
		name  string
		setup func(r *registry, e *Expect)
		tune  func(v *Verifier)
		want  string
	}{
		{"another release", func(_ *registry, e *Expect) { e.Release = "v0.3.9" }, nil, "signature over"},
		{"another commit", func(_ *registry, e *Expect) { e.Commit = otherCommit }, nil, "signature over"},
		{"another source repository", func(_ *registry, e *Expect) { e.SourceURI = "github.com/FemLed/other" }, nil, "signature over"},
		{"another workflow", func(*registry, *Expect) {}, func(v *Verifier) { v.WorkflowPath = ".github/workflows/build.yml" }, "signature over"},
		{"another issuer", func(*registry, *Expect) {}, func(v *Verifier) { v.Issuer = "https://accounts.google.com" }, "signature over"},
		{"another builder", func(*registry, *Expect) {}, func(v *Verifier) {
			v.BuilderSAN = regexp.MustCompile(`^https://github\.com/other/builder/\.github/workflows/build\.yml@refs/tags/v\d+\.\d+\.\d+$`)
		}, "provenance of"},
		{"unknown digest", func(_ *registry, e *Expect) {
			e.Digest = "sha256:" + strings.Repeat("00", 32)
		}, nil, "no Sigstore records"},
		{"no attestation", func(r *registry, _ *Expect) { r.noAtt = true }, nil, "provenance of"},
		{"attestation manifest without envelope layers", func(r *registry, _ *Expect) {
			r.set("/manifests/sha256-"+fixtureDigest[7:]+".att", []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[]}`))
		}, nil, "provenance of"},
		{"bundle blob replaced", func(r *registry, _ *Expect) {
			// A registry serving other bytes under the blob's digest.
			r.set("/blobs/"+bundleBlobDigest, []byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json"}`))
		}, nil, "does not hash to its digest"},
		{"bundle statement altered, hashes re-rooted at the tag", func(r *registry, _ *Expect) {
			var b map[string]any
			if err := json.Unmarshal(fixture(t, "bundle.json"), &b); err != nil {
				t.Fatal(err)
			}
			tamperEnvelope(t, b["dsseEnvelope"].(map[string]any), func(s string) string {
				return strings.Replace(s, "cosign/sign/v1", "cosign/sign/v2", 1)
			})
			blob, _ := json.Marshal(b)
			var m map[string]any
			_ = json.Unmarshal(fixture(t, "refman.json"), &m)
			layer := m["layers"].([]any)[0].(map[string]any)
			layer["digest"], layer["size"] = digestOf(blob), len(blob)
			man, _ := json.Marshal(m)
			var idx map[string]any
			_ = json.Unmarshal(fixture(t, "tagidx.json"), &idx)
			entry := idx["manifests"].([]any)[0].(map[string]any)
			entry["digest"], entry["size"] = digestOf(man), len(man)
			tag, _ := json.Marshal(idx)
			r.set("/blobs/"+digestOf(blob), blob)
			r.set("/manifests/"+digestOf(man), man)
			r.set("/manifests/sha256-"+fixtureDigest[7:], tag)
		}, nil, "signature over"},
		{"provenance re-labelled for another release", func(r *registry, _ *Expect) {
			var env map[string]any
			if err := json.Unmarshal(fixture(t, "att-env.json"), &env); err != nil {
				t.Fatal(err)
			}
			tamperEnvelope(t, env, func(s string) string {
				return strings.Replace(s, "refs/tags/v0.4.0", "refs/tags/v0.4.1", 1)
			})
			blob, _ := json.Marshal(env)
			var m map[string]any
			_ = json.Unmarshal(fixture(t, "att.json"), &m)
			layer := m["layers"].([]any)[0].(map[string]any)
			layer["digest"], layer["size"] = digestOf(blob), len(blob)
			man, _ := json.Marshal(m)
			r.set("/blobs/"+digestOf(blob), blob)
			r.set("/manifests/sha256-"+fixtureDigest[7:]+".att", man)
		}, nil, "provenance of"},
		{"attestation without its Rekor record", func(r *registry, _ *Expect) {
			var m map[string]any
			_ = json.Unmarshal(fixture(t, "att.json"), &m)
			ann := m["layers"].([]any)[0].(map[string]any)["annotations"].(map[string]any)
			delete(ann, "dev.sigstore.cosign/bundle")
			man, _ := json.Marshal(m)
			r.set("/manifests/sha256-"+fixtureDigest[7:]+".att", man)
		}, nil, "provenance of"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRegistry(t)
			e := r.expect()
			tc.setup(r, &e)
			v := r.verifier()
			if tc.tune != nil {
				tc.tune(v)
			}
			_, err := v.Verify(context.Background(), e)
			if err == nil {
				t.Fatal("verified")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestCacheReusesAndExpires(t *testing.T) {
	r := newRegistry(t)
	dir := t.TempDir()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	v := r.verifier()
	v.CacheDir = dir
	v.Now = func() time.Time { return now }
	// CacheDir also turns on the TUF refresh; the test must not depend on
	// the network, so pin the trust root.
	tr, err := root.NewTrustedRootFromJSON(embeddedTrustedRoot)
	if err != nil {
		t.Fatal(err)
	}
	v.TrustedRoot = tr

	res, err := v.Verify(context.Background(), r.expect())
	if err != nil {
		t.Fatal(err)
	}
	checkResult(t, res)
	blobs := r.count("/blobs/" + bundleBlobDigest)

	again, err := v.Verify(context.Background(), r.expect())
	if err != nil {
		t.Fatal(err)
	}
	if !again.Cached || again.SignatureLogIndex != fixtureSigIndex || r.count("/blobs/"+bundleBlobDigest) != blobs {
		t.Error("second call did not come from memory")
	}

	fresh := r.verifier()
	fresh.CacheDir, fresh.Now, fresh.TrustedRoot = dir, v.Now, tr
	disk, err := fresh.Verify(context.Background(), r.expect())
	if err != nil {
		t.Fatal(err)
	}
	if !disk.Cached || disk.Builder != fixtureBuilder || r.count("/blobs/"+bundleBlobDigest) != blobs {
		t.Error("new verifier did not read the disk cache")
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "provenance"))
	if len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), ".json") {
		t.Errorf("cache dir %v", entries)
	}

	// Another release is another key: the registry is asked, and refuses.
	other := r.expect()
	other.Release = "v0.4.1"
	if _, err := fresh.Verify(context.Background(), other); err == nil {
		t.Error("cache answered for another release")
	}
	if r.count("/blobs/"+bundleBlobDigest) != blobs+1 {
		t.Error("another release did not consult the registry")
	}
	blobs++

	// After the TTL the registry is consulted again.
	now = now.Add(8 * 24 * time.Hour)
	late, err := fresh.Verify(context.Background(), r.expect())
	if err != nil {
		t.Fatal(err)
	}
	if late.Cached || r.count("/blobs/"+bundleBlobDigest) != blobs+1 {
		t.Error("expired entry was reused")
	}
}

func TestExpectValidate(t *testing.T) {
	good := Expect{Digest: fixtureDigest, Release: fixtureRelease, Commit: fixtureCommit, Repo: "ghcr.io/" + fixtureRepoPath, SourceURI: fixtureSource}
	if err := good.validate(); err != nil {
		t.Fatal(err)
	}
	bad := []func(e *Expect){
		func(e *Expect) { e.Digest = "sha512:" + fixtureDigest[7:] },
		func(e *Expect) { e.Digest = fixtureDigest[:70] },
		func(e *Expect) { e.Digest = "sha256:" + strings.Repeat("zz", 32) },
		func(e *Expect) { e.Release = "" },
		func(e *Expect) { e.Release = "0.4.0" },
		func(e *Expect) { e.Release = "v0.4.0 " },
		func(e *Expect) { e.Commit = "fb4d06c" },
		func(e *Expect) { e.Repo = "ghcr.io" },
		func(e *Expect) { e.Repo = "https://ghcr.io/x" },
		func(e *Expect) { e.SourceURI = "gitlab.com/FemLed/x" },
		func(e *Expect) { e.SourceURI = "github.com/FemLed" },
		func(e *Expect) { e.SourceURI = "github.com/FemLed/x/y" },
	}
	for i, f := range bad {
		e := good
		f(&e)
		if err := e.validate(); err == nil {
			t.Errorf("case %d accepted %+v", i, e)
		}
	}
}

func TestLegacyBundleRejectsIncompleteRecords(t *testing.T) {
	var m oci.Manifest
	if err := json.Unmarshal(fixture(t, "att.json"), &m); err != nil {
		t.Fatal(err)
	}
	env := fixture(t, "att-env.json")
	ann := m.Layers[0].Annotations
	if _, err := legacyBundle(env, ann); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	without := func(key string) map[string]string {
		c := map[string]string{}
		for k, v := range ann {
			if k != key {
				c[k] = v
			}
		}
		return c
	}
	if _, err := legacyBundle(env, without("dev.sigstore.cosign/certificate")); err == nil {
		t.Error("accepted without certificate")
	}
	if _, err := legacyBundle(env, without("dev.sigstore.cosign/bundle")); err == nil {
		t.Error("accepted without Rekor record")
	}
	if _, err := legacyBundle([]byte(`{"payloadType":"x","payload":"","signatures":[]}`), ann); err == nil {
		t.Error("accepted unsigned envelope")
	}
	if _, err := legacyBundle([]byte(`not json`), ann); err == nil {
		t.Error("accepted garbage")
	}
}

func TestEmbeddedTrustedRootIsCurrent(t *testing.T) {
	tr, err := root.NewTrustedRootFromJSON(embeddedTrustedRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.FulcioCertificateAuthorities()) == 0 || len(tr.RekorLogs()) == 0 || len(tr.CTLogs()) == 0 || len(tr.TimestampingAuthorities()) == 0 {
		t.Error("embedded trust root is missing a service")
	}
	// The fixtures were signed in September 2026; the root must cover them.
	var current bool
	for _, ca := range tr.FulcioCertificateAuthorities() {
		if fca, ok := ca.(*root.FulcioCertificateAuthority); ok && (fca.ValidityPeriodEnd.IsZero() || fca.ValidityPeriodEnd.After(time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC))) {
			current = true
		}
	}
	if !current {
		t.Error("no Fulcio authority valid for the fixtures")
	}
}

func TestVerifierFailsClosedWhenTheRegistryIsDown(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	v := &Verifier{Registry: &oci.Client{HTTP: srv.Client()}}
	e := Expect{Digest: fixtureDigest, Release: fixtureRelease, Commit: fixtureCommit, Repo: srv.Listener.Addr().String() + "/" + fixtureRepoPath, SourceURI: fixtureSource}
	_, err := v.Verify(context.Background(), e)
	if err == nil || !strings.Contains(err.Error(), "status 503") {
		t.Fatalf("err %v", err)
	}
	if errors.Is(err, oci.ErrNotFound) {
		t.Error("an outage is not an absence")
	}
}
