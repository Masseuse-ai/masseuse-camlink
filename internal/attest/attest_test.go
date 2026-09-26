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
)

var testSource = ImageSource{Repo: "ghcr.io/femled/masseuse-video-tee", Tag: "v0.1.0", SourceURI: "github.com/FemLed/masseuse-video-tee"}

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
				"env":              map[string]any{"TRAINER_URL": testTrainer + "/"},
				"image_signatures": []any{map[string]any{"key_id": "projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1", "signature_algorithm": "RSASSA_PSS_SHA256"}},
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

func policyFor() *Policy {
	p := &Policy{
		AllowedImageDigests: []string{testDigest},
		AllowDebug:          false,
		RequireStable:       true,
		RequireGpuCc:        true,
		ExpectedTrainerURL:  testTrainer,
		ImageSources:        map[string]ImageSource{testDigest: testSource},
	}
	_ = p.Validate()
	return p
}

func TestImageSources(t *testing.T) {
	fs := newFakeSlot(t)
	res, err := verifierFor(fs, policyFor()).Verify(context.Background(), fs.origin())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if res.Source == nil || *res.Source != testSource {
		t.Fatalf("source %+v, want %+v", res.Source, testSource)
	}
	if got, want := res.Source.String(), "github.com/FemLed/masseuse-video-tee@v0.1.0"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	want := "slsa-verifier verify-image ghcr.io/femled/masseuse-video-tee@" + testDigest +
		" --source-uri github.com/FemLed/masseuse-video-tee --source-tag v0.1.0"
	if got := res.Source.VerifyCommand(res.ImageDigest); got != want {
		t.Fatalf("VerifyCommand() = %q, want %q", got, want)
	}

	// A digest the policy has no build record for verifies with Source nil.
	p := policyFor()
	p.ImageSources = nil
	res, err = verifierFor(fs, p).Verify(context.Background(), fs.origin())
	if err != nil {
		t.Fatalf("verify without sources: %v", err)
	}
	if res.Source != nil {
		t.Fatalf("source %+v, want nil", res.Source)
	}

	// Validate rejects a malformed record; a well-formed one for another
	// digest is fine.
	for name, s := range map[string]ImageSource{
		"no repo":       {Tag: "v0.1.0", SourceURI: "github.com/FemLed/masseuse-video-tee"},
		"repo with tag": {Repo: "ghcr.io/femled/masseuse-video-tee:v0.1.0", Tag: "v0.1.0", SourceURI: "github.com/FemLed/masseuse-video-tee"},
		"bare tag":      {Repo: "ghcr.io/femled/masseuse-video-tee", Tag: "0.1.0", SourceURI: "github.com/FemLed/masseuse-video-tee"},
		"url source":    {Repo: "ghcr.io/femled/masseuse-video-tee", Tag: "v0.1.0", SourceURI: "https://github.com/FemLed/masseuse-video-tee"},
		"no source":     {Repo: "ghcr.io/femled/masseuse-video-tee", Tag: "v0.1.0"},
	} {
		p := policyFor()
		p.ImageSources = map[string]ImageSource{testDigest: s}
		if err := p.Validate(); err == nil {
			t.Errorf("%s: accepted %+v", name, s)
		}
	}
	p = policyFor()
	p.ImageSources = map[string]ImageSource{"latest": testSource}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "imageSources key") {
		t.Fatalf("non-digest key: %v", err)
	}
	p = policyFor()
	p.ImageSources = map[string]ImageSource{"sha256:" + strings.Repeat("ab", 32): testSource}
	if err := p.Validate(); err != nil {
		t.Fatalf("record for another digest: %v", err)
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
		{name: "wrong digest", mutate: func(c, _ map[string]any) {
			c["submods"].(map[string]any)["container"].(map[string]any)["image_digest"] = "sha256:ffff"
		}, want: "is not in the policy"},
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
			c["submods"].(map[string]any)["container"].(map[string]any)["env"] = map[string]any{"TRAINER_URL": "https://evil.example"}
		}, want: "not the expected service"},
		{name: "signature required", policy: func(p *Policy) { p.ImageSignatures = []string{"projects/other"} }, want: "none of the required signatures"},
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
	p.ImageSignatures = []string{"projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1"}
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
		"allowedImageDigests": []string{testDigest},
		"allowDebug":          true,
		"requireStable":       false,
		"requireGpuCc":        true,
		"expectedTrainerUrl":  testTrainer,
		"imageSignatures":     []string{},
		"imageSources":        map[string]any{testDigest: map[string]string{"repo": testSource.Repo, "tag": testSource.Tag, "sourceUri": testSource.SourceURI}},
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
	if p.ImageSources[testDigest] != testSource {
		t.Fatalf("imageSources %+v", p.ImageSources)
	}
	badSource := map[string]any{"allowedImageDigests": []string{testDigest}, "imageSources": map[string]any{testDigest: map[string]string{"repo": "ghcr.io/femled/masseuse-video-tee", "tag": "latest"}}}
	srv5 := serve(badSource)
	defer srv5.Close()
	if _, err := FetchPolicy(context.Background(), srv5.Client(), srv5.URL); err == nil {
		t.Fatal("accepted a malformed image source")
	}

	bad := map[string]any{"allowedImageDigests": []string{"latest"}}
	srv2 := serve(bad)
	defer srv2.Close()
	if _, err := FetchPolicy(context.Background(), srv2.Client(), srv2.URL); err == nil {
		t.Fatal("accepted a non-digest")
	}
	empty := map[string]any{"allowedImageDigests": []string{}}
	srv3 := serve(empty)
	defer srv3.Close()
	if _, err := FetchPolicy(context.Background(), srv3.Client(), srv3.URL); err == nil {
		t.Fatal("accepted an empty digest list")
	}
	insecure := map[string]any{"allowedImageDigests": []string{testDigest}, "jwksUrl": "http://jwks.example"}
	srv4 := serve(insecure)
	defer srv4.Close()
	if _, err := FetchPolicy(context.Background(), srv4.Client(), srv4.URL); err == nil {
		t.Fatal("accepted an http JWKS URL")
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
