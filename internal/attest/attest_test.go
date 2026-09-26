package attest

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	testDigest  = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testTrainer = "https://masseuse-trainer.example.run.app"
	// The launcher reports a signing key as the hex SHA-256 of its DER
	// public key.
	testKeyID   = "0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f"
	testRelease = "v0.4.0"
	testCommit  = "0123456789abcdef0123456789abcdef01234567"
	testRepo    = "ghcr.io/femled/masseuse-video-tee"
	testSrcURI  = "github.com/FemLed/masseuse-video-tee"
)

var testSource = ImageSource{Repo: testRepo, Tag: "v0.1.0", SourceURI: testSrcURI}

// fakeSlot is an enclave that mints tokens the way Confidential Space does,
// signed by a test key, with the nonce bindings a real slot adds.
type fakeSlot struct {
	t        *testing.T
	srv      *httptest.Server
	signer   *rsa.PrivateKey
	kid      string
	evidence ed25519.PublicKey
	spki     string
	now      time.Time
	// mutate edits the claims and document before they are served.
	mutate func(claims map[string]any, doc map[string]any)
	// echoNonce false serves a token that ignores the caller's nonce.
	echoNonce bool
}

func newFakeSlot(t *testing.T) *fakeSlot {
	t.Helper()
	signer, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	evPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fs := &fakeSlot{t: t, signer: signer, kid: "test-kid", evidence: evPub, now: time.Now(), echoNonce: true}
	fs.srv = httptest.NewTLSServer(http.HandlerFunc(fs.handle))
	t.Cleanup(fs.srv.Close)
	leaf := fs.srv.Certificate()
	h := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	fs.spki = base64.RawURLEncoding.EncodeToString(h[:])
	return fs
}

func (fs *fakeSlot) origin() string { return fs.srv.URL }

func (fs *fakeSlot) roots() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(fs.srv.Certificate())
	return pool
}

func (fs *fakeSlot) evidenceNonce() string {
	h := sha256.Sum256(fs.evidence)
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func (fs *fakeSlot) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/attestation" {
		http.NotFound(w, r)
		return
	}
	nonce := r.URL.Query().Get("nonce")
	eat := []any{fs.evidenceNonce(), fs.spki}
	if fs.echoNonce {
		eat = append(eat, nonce)
	}
	claims := map[string]any{
		"iss":       DefaultIssuer,
		"aud":       fs.origin(),
		"exp":       fs.now.Add(time.Hour).Unix(),
		"iat":       fs.now.Unix(),
		"nbf":       fs.now.Unix(),
		"swname":    DefaultSWName,
		"hwmodel":   DefaultHWModel,
		"dbgstat":   "disabled-since-boot",
		"secboot":   true,
		"eat_nonce": eat,
		"submods": map[string]any{
			"container": map[string]any{
				"image_digest":     testDigest,
				"image_reference":  "us-central1-docker.pkg.dev/p/r/masseuse-video-tee@" + testDigest,
				"env":              map[string]any{"TRAINER_URL": testTrainer + "/", "TEE_IMAGE_VERSION": testRelease, "TEE_IMAGE_COMMIT": testCommit},
				"image_signatures": []any{map[string]any{"key_id": testKeyID, "signature_algorithm": "ECDSA_P256_SHA256"}},
			},
			"gce":                map[string]any{"instance_id": "1234567890", "project_id": "p"},
			"nvidia_gpu":         map[string]any{"cc_mode": "ON", "gpus": []any{map[string]any{"hwmodel": "GCP_NVIDIA_H100"}}},
			"confidential_space": map[string]any{"support_attributes": []any{"LATEST", "STABLE", "USABLE"}},
		},
	}
	spki := fs.spki
	doc := map[string]any{
		"nonces":       eat,
		"tlsSpkiNonce": &spki,
		"evidenceKey":  map[string]any{"alg": "Ed25519", "publicKey": base64.RawURLEncoding.EncodeToString(fs.evidence), "nonce": fs.evidenceNonce()},
	}
	if fs.mutate != nil {
		fs.mutate(claims, doc)
	}
	doc["token"] = fs.sign(claims)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(doc)
}

func (fs *fakeSlot) sign(claims map[string]any) string {
	return signJWT(fs.t, fs.signer, fs.kid, "RS256", claims)
}

func signJWT(t *testing.T, key *rsa.PrivateKey, kid, alg string, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": alg, "kid": kid, "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	signing := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// policyFor is the rule-based policy the service publishes: the signing
// key, a release floor, and where the source and the public image live.
func policyFor() *Policy {
	p := &Policy{
		AllowDebug:         false,
		RequireStable:      true,
		RequireGpuCc:       true,
		ExpectedTrainerURL: testTrainer,
		ImageSignatures:    []string{testKeyID},
		MinRelease:         "v0.4.0",
		SourceURI:          testSrcURI,
		ImageRepo:          testRepo,
	}
	if err := p.Validate(); err != nil {
		panic(err)
	}
	return p
}

// pinnedPolicyFor is the older shape: an explicit digest list with a
// per-digest build record and no signing key.
func pinnedPolicyFor() *Policy {
	p := &Policy{
		AllowedImageDigests: []string{testDigest},
		RequireStable:       true,
		RequireGpuCc:        true,
		ExpectedTrainerURL:  testTrainer,
		ImageSources:        map[string]ImageSource{testDigest: testSource},
	}
	if err := p.Validate(); err != nil {
		panic(err)
	}
	return p
}

func TestReleaseStampAndSource(t *testing.T) {
	fs := newFakeSlot(t)
	res, err := verifierFor(fs, policyFor()).Verify(context.Background(), fs.origin())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if res.Release == nil || res.Release.Version != testRelease || res.Release.Commit != testCommit {
		t.Fatalf("release %+v", res.Release)
	}
	// The source record comes from the policy's constants and the token's
	// tag, so the verify command names the release that is running.
	want := ImageSource{Repo: testRepo, Tag: testRelease, SourceURI: testSrcURI}
	if res.Source == nil || *res.Source != want {
		t.Fatalf("source %+v, want %+v", res.Source, want)
	}
	if got, want := res.Source.String(), testSrcURI+"@"+testRelease; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	wantCmd := "slsa-verifier verify-image " + testRepo + "@" + testDigest + " --source-uri " + testSrcURI + " --source-tag " + testRelease
	if got := res.Source.VerifyCommand(res.ImageDigest); got != wantCmd {
		t.Fatalf("VerifyCommand() = %q, want %q", got, wantCmd)
	}

	// An unstamped image under a policy without a floor: no release, and
	// the verify command names the source without a tag.
	unstamped := newFakeSlot(t)
	unstamped.mutate = func(c, _ map[string]any) {
		c["submods"].(map[string]any)["container"].(map[string]any)["env"] = map[string]any{"TRAINER_URL": testTrainer}
	}
	p := policyFor()
	p.MinRelease = ""
	res, err = verifierFor(unstamped, p).Verify(context.Background(), unstamped.origin())
	if err != nil {
		t.Fatalf("unstamped image without a floor: %v", err)
	}
	if res.Release != nil {
		t.Fatalf("release %+v, want nil", res.Release)
	}
	if got, want := res.Source.VerifyCommand(res.ImageDigest), "slsa-verifier verify-image "+testRepo+"@"+testDigest+" --source-uri "+testSrcURI; got != want {
		t.Fatalf("VerifyCommand() = %q, want %q", got, want)
	}
	if got, want := res.Source.String(), testSrcURI; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	// ... unless the policy still carries a per-digest record for it.
	p.ImageSources = map[string]ImageSource{testDigest: testSource}
	res, err = verifierFor(unstamped, p).Verify(context.Background(), unstamped.origin())
	if err != nil {
		t.Fatal(err)
	}
	if res.Source == nil || res.Source.Tag != "v0.1.0" {
		t.Fatalf("source %+v, want the record's tag", res.Source)
	}

	// A policy that publishes no source at all: Source nil, attestation holds.
	p = policyFor()
	p.SourceURI, p.ImageRepo = "", ""
	res, err = verifierFor(fs, p).Verify(context.Background(), fs.origin())
	if err != nil {
		t.Fatalf("verify without a source: %v", err)
	}
	if res.Source != nil {
		t.Fatalf("source %+v, want nil", res.Source)
	}
}

func TestPinnedPolicyStillWorks(t *testing.T) {
	fs := newFakeSlot(t)
	res, err := verifierFor(fs, pinnedPolicyFor()).Verify(context.Background(), fs.origin())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	// With no sourceUri/imageRepo the per-digest record is the source.
	if res.Source == nil || *res.Source != testSource {
		t.Fatalf("source %+v, want %+v", res.Source, testSource)
	}
	want := "slsa-verifier verify-image " + testRepo + "@" + testDigest + " --source-uri " + testSrcURI + " --source-tag v0.1.0"
	if got := res.Source.VerifyCommand(res.ImageDigest); got != want {
		t.Fatalf("VerifyCommand() = %q, want %q", got, want)
	}
	p := pinnedPolicyFor()
	p.ImageSources = nil
	res, err = verifierFor(fs, p).Verify(context.Background(), fs.origin())
	if err != nil {
		t.Fatalf("verify without sources: %v", err)
	}
	if res.Source != nil {
		t.Fatalf("source %+v, want nil", res.Source)
	}
	// Both pins at once: the digest list still applies.
	p = policyFor()
	p.AllowedImageDigests = []string{"sha256:" + strings.Repeat("ab", 32)}
	_, err = verifierFor(fs, p).Verify(context.Background(), fs.origin())
	var perr *PolicyError
	if !errors.As(err, &perr) || !strings.Contains(err.Error(), "is not in the policy") {
		t.Fatalf("digest list ignored: %v", err)
	}
}

func TestPolicyValidate(t *testing.T) {
	if err := (&Policy{}).Validate(); err == nil || !strings.Contains(err.Error(), "neither") {
		t.Fatalf("empty policy: %v", err)
	}
	bad := map[string]func(*Policy){
		"bad digest":       func(p *Policy) { p.AllowedImageDigests = []string{"latest"} },
		"empty key id":     func(p *Policy) { p.ImageSignatures = []string{""} },
		"bare min release": func(p *Policy) { p.MinRelease = "0.4.0" },
		"two-part release": func(p *Policy) { p.MinRelease = "v0.4" },
		"url source":       func(p *Policy) { p.SourceURI = "https://" + testSrcURI },
		"repo with tag":    func(p *Policy) { p.ImageRepo = testRepo + ":v0.4.0" },
		"repo without src": func(p *Policy) { p.SourceURI = "" },
		"src without repo": func(p *Policy) { p.ImageRepo = "" },
		"insecure jwks":    func(p *Policy) { p.JWKSURL = "http://jwks.example" },
		"source no repo": func(p *Policy) {
			p.ImageSources = map[string]ImageSource{testDigest: {Tag: "v0.1.0", SourceURI: testSrcURI}}
		},
		"source bare tag": func(p *Policy) {
			p.ImageSources = map[string]ImageSource{testDigest: {Repo: testRepo, Tag: "0.1.0", SourceURI: testSrcURI}}
		},
		"source url": func(p *Policy) {
			p.ImageSources = map[string]ImageSource{testDigest: {Repo: testRepo, Tag: "v0.1.0", SourceURI: "https://" + testSrcURI}}
		},
		"source no src":     func(p *Policy) { p.ImageSources = map[string]ImageSource{testDigest: {Repo: testRepo, Tag: "v0.1.0"}} },
		"source non-digest": func(p *Policy) { p.ImageSources = map[string]ImageSource{"latest": testSource} },
	}
	for name, mutate := range bad {
		p := policyFor()
		mutate(p)
		if err := p.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Pre-release floors and records for other digests are fine.
	p := policyFor()
	p.MinRelease = "v1.2.0-rc.1"
	p.ImageSources = map[string]ImageSource{"sha256:" + strings.Repeat("ab", 32): testSource}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	// Defaults are filled.
	if p.Issuer != DefaultIssuer || p.JWKSURL != DefaultJWKSURL || p.SWName != DefaultSWName || p.HWModel != DefaultHWModel {
		t.Fatalf("defaults %+v", p)
	}
}

func TestCompareRelease(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"v0.4.0", "v0.4.0", 0},
		{"v0.4.1", "v0.4.0", 1},
		{"v0.4.0", "v0.10.0", -1},
		{"v1.0.0", "v0.99.99", 1},
		{"v0.4.0-rc.1", "v0.4.0", -1},
		{"v0.4.0", "v0.4.0-rc.1", 1},
		{"v0.4.0-rc.1", "v0.4.0-rc.2", -1},
		{"v0.4.0+build.7", "v0.4.0", 0},
	} {
		if got := CompareRelease(tc.a, tc.b); got != tc.want {
			t.Errorf("CompareRelease(%s, %s) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
	for _, ok := range []string{"v0.0.0", "v10.20.30", "v1.2.3-beta", "v1.2.3+meta"} {
		if !ValidRelease(ok) {
			t.Errorf("ValidRelease(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "0.4.0", "v0.4", "v0.4.0.1", "va.b.c", "v0.4.0-", "v 0.4.0", "latest"} {
		if ValidRelease(bad) {
			t.Errorf("ValidRelease(%q) = true", bad)
		}
	}
}

func verifierFor(fs *fakeSlot, p *Policy) *Verifier {
	return &Verifier{
		Policy:  p,
		Keys:    StaticKeys{fs.kid: &fs.signer.PublicKey},
		RootCAs: fs.roots(),
		Now:     func() time.Time { return fs.now },
	}
}

func TestVerifyPassesAndPins(t *testing.T) {
	fs := newFakeSlot(t)
	v := verifierFor(fs, policyFor())
	res, err := v.Verify(context.Background(), fs.origin()+"/")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if res.TLSSpkiNonce != fs.spki {
		t.Fatalf("pinned %s, want %s", res.TLSSpkiNonce, fs.spki)
	}
	if res.ImageDigest != testDigest || res.InstanceID != "1234567890" || res.DbgStat != "disabled-since-boot" {
		t.Fatalf("result %+v", res)
	}
	if res.Origin != fs.origin() {
		t.Fatalf("origin %s", res.Origin)
	}

	// The pinned TLS config accepts the attested key and refuses another.
	host := strings.TrimPrefix(fs.origin(), "https://")
	conn, err := tls.Dial("tcp", host, PinnedTLSConfig(fs.roots(), "127.0.0.1", res.SPKISHA256))
	if err != nil {
		t.Fatalf("pinned dial: %v", err)
	}
	conn.Close()
	var other [32]byte
	other[0] = 1
	if _, err := tls.Dial("tcp", host, PinnedTLSConfig(fs.roots(), "127.0.0.1", other)); err == nil || !strings.Contains(err.Error(), "not the attested key") {
		t.Fatalf("wrong pin accepted: %v", err)
	}
}

func TestVerifyPolicyFailures(t *testing.T) {
	cases := []struct {
		name   string
		policy func(*Policy)
		mutate func(claims, doc map[string]any)
		nonce  bool
		want   string
	}{
		{name: "wrong digest", policy: func(p *Policy) { p.AllowedImageDigests = []string{"sha256:" + strings.Repeat("ab", 32)} }, want: "is not in the policy"},
		{name: "no digest", mutate: func(c, _ map[string]any) {
			delete(c["submods"].(map[string]any)["container"].(map[string]any), "image_digest")
		}, want: "no image digest"},
		{name: "unsigned image", mutate: func(c, _ map[string]any) {
			c["submods"].(map[string]any)["container"].(map[string]any)["image_signatures"] = []any{}
		}, want: "none of the required signatures"},
		{name: "other key", mutate: func(c, _ map[string]any) {
			c["submods"].(map[string]any)["container"].(map[string]any)["image_signatures"] = []any{map[string]any{"key_id": strings.Repeat("ee", 32), "signature_algorithm": "ECDSA_P256_SHA256"}}
		}, want: "none of the required signatures"},
		{name: "release below the floor", policy: func(p *Policy) { p.MinRelease = "v0.4.1" }, want: "older than the policy's minimum v0.4.1"},
		{name: "release pre-release below the floor", mutate: func(c, _ map[string]any) {
			c["submods"].(map[string]any)["container"].(map[string]any)["env"].(map[string]any)["TEE_IMAGE_VERSION"] = "v0.4.0-rc.2"
		}, want: "older than the policy's minimum"},
		{name: "unstamped image under a floor", mutate: func(c, _ map[string]any) {
			c["submods"].(map[string]any)["container"].(map[string]any)["env"] = map[string]any{"TRAINER_URL": testTrainer}
		}, want: "no release stamp"},
		{name: "malformed stamp", mutate: func(c, _ map[string]any) {
			c["submods"].(map[string]any)["container"].(map[string]any)["env"].(map[string]any)["TEE_IMAGE_VERSION"] = "latest"
		}, want: "is not a release tag"},
		{name: "debug not allowed", mutate: func(c, _ map[string]any) { c["dbgstat"] = "enabled" }, want: "does not allow debug"},
		{name: "unknown dbgstat", mutate: func(c, _ map[string]any) { c["dbgstat"] = "weird" }, want: "dbgstat is weird"},
		{name: "not stable", mutate: func(c, _ map[string]any) {
			c["submods"].(map[string]any)["confidential_space"].(map[string]any)["support_attributes"] = []any{"LATEST"}
		}, want: "STABLE"},
		{name: "gpu cc off", mutate: func(c, _ map[string]any) {
			c["submods"].(map[string]any)["nvidia_gpu"].(map[string]any)["cc_mode"] = "OFF"
		}, want: "GPU confidential computing is OFF"},
		{name: "no gpu", mutate: func(c, _ map[string]any) {
			c["submods"].(map[string]any)["nvidia_gpu"].(map[string]any)["gpus"] = []any{}
		}, want: "no GPU"},
		{name: "wrong trainer", mutate: func(c, _ map[string]any) {
			c["submods"].(map[string]any)["container"].(map[string]any)["env"].(map[string]any)["TRAINER_URL"] = "https://evil.example"
		}, want: "not the expected service"},
		{name: "signature required", policy: func(p *Policy) { p.ImageSignatures = []string{strings.Repeat("ee", 32)} }, want: "none of the required signatures"},
		{name: "swname", mutate: func(c, _ map[string]any) { c["swname"] = "OTHER" }, want: "swname is OTHER"},
		{name: "hwmodel", mutate: func(c, _ map[string]any) { c["hwmodel"] = "GCP_AMD_SEV" }, want: "hwmodel is GCP_AMD_SEV"},
		{name: "secboot", mutate: func(c, _ map[string]any) { c["secboot"] = false }, want: "secure boot"},
		{name: "stale nonce", nonce: true, want: "does not echo this run's nonce"},
		{name: "tls spki not bound", mutate: func(c, d map[string]any) {
			d["tlsSpkiNonce"] = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		}, want: "not the key this connection negotiated"},
		{name: "tls spki missing", mutate: func(c, d map[string]any) { d["tlsSpkiNonce"] = nil }, want: "no tlsSpkiNonce"},
		{name: "tls spki not in eat_nonce", mutate: func(c, d map[string]any) {
			eat := c["eat_nonce"].([]any)
			c["eat_nonce"] = []any{eat[0], eat[2]} // drop the SPKI
		}, want: "TLS key is not bound"},
		{name: "evidence key not bound", mutate: func(c, d map[string]any) {
			eat := c["eat_nonce"].([]any)
			c["eat_nonce"] = []any{eat[1], eat[2]} // drop the evidence key
		}, want: "evidence key is not bound"},
		{name: "evidence key missing", mutate: func(c, d map[string]any) {
			d["evidenceKey"] = map[string]any{"alg": "RSA", "publicKey": "x"}
		}, want: "no Ed25519 evidence key"},
		{name: "no instance id", mutate: func(c, _ map[string]any) {
			c["submods"].(map[string]any)["gce"] = map[string]any{}
		}, want: "no GCE instance id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeSlot(t)
			fs.mutate = tc.mutate
			fs.echoNonce = !tc.nonce
			p := policyFor()
			if tc.policy != nil {
				tc.policy(p)
			}
			_, err := verifierFor(fs, p).Verify(context.Background(), fs.origin())
			var perr *PolicyError
			if !errors.As(err, &perr) {
				t.Fatalf("err %v, want PolicyError", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("reasons %v do not mention %q", perr.Reasons, tc.want)
			}
		})
	}
}

func TestVerifyDebugPostureAllowed(t *testing.T) {
	fs := newFakeSlot(t)
	fs.mutate = func(c, _ map[string]any) {
		c["dbgstat"] = "enabled"
		c["submods"].(map[string]any)["confidential_space"].(map[string]any)["support_attributes"] = []any{"LATEST"}
	}
	p := policyFor()
	p.AllowDebug = true
	p.RequireStable = false
	if _, err := verifierFor(fs, p).Verify(context.Background(), fs.origin()); err != nil {
		t.Fatalf("debug posture refused: %v", err)
	}
	// Any listed key satisfies the signature check.
	p.ImageSignatures = []string{strings.Repeat("ee", 32), testKeyID}
	if _, err := verifierFor(fs, p).Verify(context.Background(), fs.origin()); err != nil {
		t.Fatalf("matching signature refused: %v", err)
	}
}

func TestVerifyTokenFailures(t *testing.T) {
	t.Run("wrong audience", func(t *testing.T) {
		fs := newFakeSlot(t)
		fs.mutate = func(c, _ map[string]any) { c["aud"] = "https://slot-9.example" }
		_, err := verifierFor(fs, policyFor()).Verify(context.Background(), fs.origin())
		if err == nil || !strings.Contains(err.Error(), "audience") {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("expired", func(t *testing.T) {
		fs := newFakeSlot(t)
		fs.mutate = func(c, _ map[string]any) { c["exp"] = fs.now.Add(-2 * time.Hour).Unix() }
		_, err := verifierFor(fs, policyFor()).Verify(context.Background(), fs.origin())
		if err == nil || !strings.Contains(err.Error(), "expired") {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("wrong issuer", func(t *testing.T) {
		fs := newFakeSlot(t)
		fs.mutate = func(c, _ map[string]any) { c["iss"] = "https://accounts.google.com" }
		_, err := verifierFor(fs, policyFor()).Verify(context.Background(), fs.origin())
		if err == nil || !strings.Contains(err.Error(), "issuer") {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("unknown signer", func(t *testing.T) {
		fs := newFakeSlot(t)
		other, _ := rsa.GenerateKey(rand.Reader, 2048)
		v := verifierFor(fs, policyFor())
		v.Keys = StaticKeys{fs.kid: &other.PublicKey}
		_, err := v.Verify(context.Background(), fs.origin())
		if err == nil || !strings.Contains(err.Error(), "signature does not verify") {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("alg none", func(t *testing.T) {
		claims := map[string]any{"iss": DefaultIssuer, "aud": "x", "exp": time.Now().Add(time.Hour).Unix()}
		header, _ := json.Marshal(map[string]string{"alg": "none", "kid": "k"})
		payload, _ := json.Marshal(claims)
		tok := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + "."
		_, err := VerifyToken(context.Background(), StaticKeys{}, tok, DefaultIssuer, "x", time.Now())
		if err == nil || !strings.Contains(err.Error(), "RS256") {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("untrusted certificate", func(t *testing.T) {
		fs := newFakeSlot(t)
		v := verifierFor(fs, policyFor())
		v.RootCAs = x509.NewCertPool() // trusts nothing
		if _, err := v.Verify(context.Background(), fs.origin()); err == nil {
			t.Fatal("verified over an untrusted certificate")
		}
	})
}

func TestOriginRules(t *testing.T) {
	fs := newFakeSlot(t)
	p := policyFor()
	p.TeeSlotHostSuffixes = []string{".tee.masseuse.ai"}
	_, err := verifierFor(fs, p).Verify(context.Background(), fs.origin())
	var perr *PolicyError
	if !errors.As(err, &perr) || !strings.Contains(err.Error(), "slot host suffix") {
		t.Fatalf("IP origin accepted under a host-suffix policy: %v", err)
	}
	if !hasSuffixAny("slot-3.tee.masseuse.ai", []string{"tee.masseuse.ai"}) {
		t.Fatal("suffix without leading dot")
	}
	if hasSuffixAny("eviltee.masseuse.ai", []string{".tee.masseuse.ai"}) {
		t.Fatal("suffix matched inside a label")
	}
	for _, bad := range []string{"http://slot.example", "https://slot.example/attestation", "slot.example", ""} {
		if _, err := verifierFor(fs, policyFor()).Verify(context.Background(), bad); err == nil || errors.As(err, &perr) {
			t.Fatalf("origin %q: %v", bad, err)
		}
	}
}

func TestFetchPolicy(t *testing.T) {
	good := map[string]any{
		"allowedImageDigests": []string{},
		"allowDebug":          true,
		"requireStable":       false,
		"requireGpuCc":        true,
		"expectedTrainerUrl":  testTrainer,
		"imageSignatures":     []string{testKeyID},
		"minRelease":          "v0.4.0",
		"sourceUri":           testSrcURI,
		"imageRepo":           testRepo,
		"issuer":              DefaultIssuer,
		"jwksUrl":             DefaultJWKSURL,
		"swname":              DefaultSWName,
		"hwmodel":             DefaultHWModel,
		"teeSlotHostSuffixes": []string{".tee.masseuse.ai"},
	}
	serve := func(doc any) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/tee-policy" {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(doc)
		}))
	}
	srv := serve(good)
	defer srv.Close()
	p, err := FetchPolicy(context.Background(), srv.Client(), srv.URL+"/")
	if err != nil {
		t.Fatal(err)
	}
	if !p.AllowDebug || p.RequireStable || len(p.TeeSlotHostSuffixes) != 1 || p.JWKSURL != DefaultJWKSURL {
		t.Fatalf("policy %+v", p)
	}
	if p.MinRelease != "v0.4.0" || p.SourceURI != testSrcURI || p.ImageRepo != testRepo || len(p.ImageSignatures) != 1 {
		t.Fatalf("policy %+v", p)
	}

	// The older, digest-pinned shape is still accepted.
	pinned := map[string]any{
		"allowedImageDigests": []string{testDigest},
		"imageSources":        map[string]any{testDigest: map[string]string{"repo": testSource.Repo, "tag": testSource.Tag, "sourceUri": testSource.SourceURI}},
	}
	srvPinned := serve(pinned)
	defer srvPinned.Close()
	p, err = FetchPolicy(context.Background(), srvPinned.Client(), srvPinned.URL)
	if err != nil {
		t.Fatal(err)
	}
	if p.ImageSources[testDigest] != testSource {
		t.Fatalf("imageSources %+v", p.ImageSources)
	}

	for name, doc := range map[string]map[string]any{
		"malformed image source": {"allowedImageDigests": []string{testDigest}, "imageSources": map[string]any{testDigest: map[string]string{"repo": testRepo, "tag": "latest"}}},
		"non-digest":             {"allowedImageDigests": []string{"latest"}},
		"neither pin":            {"allowedImageDigests": []string{}, "imageSignatures": []string{}},
		"bad min release":        {"imageSignatures": []string{testKeyID}, "minRelease": "0.4.0"},
		"source without repo":    {"imageSignatures": []string{testKeyID}, "sourceUri": testSrcURI},
		"http jwks":              {"imageSignatures": []string{testKeyID}, "jwksUrl": "http://jwks.example"},
	} {
		srv := serve(doc)
		_, err := FetchPolicy(context.Background(), srv.Client(), srv.URL)
		srv.Close()
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRemoteJWKS(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	fetches := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches++
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{
			"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	defer srv.Close()
	j := NewRemoteJWKS(srv.URL, srv.Client())
	got, err := j.Key(context.Background(), "k1")
	if err != nil {
		t.Fatal(err)
	}
	if got.N.Cmp(key.N) != 0 || got.E != key.E {
		t.Fatal("wrong key")
	}
	if _, err := j.Key(context.Background(), "k1"); err != nil || fetches != 1 {
		t.Fatalf("cache miss: fetches=%d err=%v", fetches, err)
	}
	if _, err := j.Key(context.Background(), "k2"); err == nil || fetches != 2 {
		t.Fatalf("unknown kid: fetches=%d err=%v", fetches, err)
	}
	// A token signed by the served key verifies end to end through the JWKS.
	tok := signJWT(t, key, "k1", "RS256", map[string]any{"iss": "i", "aud": []string{"a", "b"}, "exp": time.Now().Add(time.Hour).Unix()})
	if _, err := VerifyToken(context.Background(), j, tok, "i", "b", time.Now()); err != nil {
		t.Fatal(err)
	}
	// Small moduli are refused.
	small, _ := rsa.GenerateKey(rand.Reader, 1024)
	_, err = ParseJWKS(strings.NewReader(`{"keys":[{"kty":"RSA","kid":"s","n":"` +
		base64.RawURLEncoding.EncodeToString(small.N.Bytes()) + `","e":"AQAB"}]}`))
	if err == nil {
		t.Fatal("accepted a 1024-bit key")
	}
}
