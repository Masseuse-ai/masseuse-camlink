// Package attest decides whether an origin is a masseuse.ai video enclave
// the connector may relay a camera to.
//
// It is a port of the checks the trainer makes before leasing a slot
// (masseuse-trainer/server/tee-attestation.js) and the standalone verifier
// in masseuse-video-tee/verifier: fetch GET /attestation?nonce=<fresh> over
// TLS, keep the leaf certificate that connection used, verify the
// Confidential Space token (RS256 against Google's JWKS; issuer, audience,
// expiry), apply the policy the service publishes at /api/tee-policy (the
// signing key the launcher verified the image with, the lowest release the
// image may be, debug state, STABLE, GPU confidential computing,
// TRAINER_URL, and image digests when the policy pins any), and check the
// nonce bindings: the fresh nonce, the SHA-256 of the enclave's evidence
// key, and the SHA-256 of the TLS leaf's SubjectPublicKeyInfo. The last
// binding is what a browser cannot check and what proves the TLS endpoint
// terminates inside the attested enclave; the connector then pins the
// tunnel's TLS to that same key.
//
// Which code runs is read off the token, not off a list: the enclave
// repository's release workflow bakes the release tag and source commit
// into the image (TEE_IMAGE_VERSION, TEE_IMAGE_COMMIT), the launcher
// attests the whole container environment, and the same workflow is the
// only holder of the signing key. The digest the token names is then
// checked against its SLSA provenance in the public registry with the one
// command the connector logs at dial.
package attest

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Defaults the policy document may override.
const (
	DefaultIssuer  = "https://confidentialcomputing.googleapis.com"
	DefaultJWKSURL = "https://www.googleapis.com/service_accounts/v1/metadata/jwk/signer@confidentialspace-sign.iam.gserviceaccount.com"
	DefaultSWName  = "CONFIDENTIAL_SPACE"
	DefaultHWModel = "GCP_INTEL_TDX"
	dbgstatProd    = "disabled-since-boot"
	dbgstatDebug   = "enabled"
)

// Policy is the document served at <service>/api/tee-policy. It must pin
// the image one of two ways: ImageSignatures (the rule) or
// AllowedImageDigests (an explicit list); a policy with neither is refused.
type Policy struct {
	// AllowedImageDigests, when non-empty, restricts the enclave to these
	// digests on top of everything else.
	AllowedImageDigests []string `json:"allowedImageDigests"`
	AllowDebug          bool     `json:"allowDebug"`
	RequireStable       bool     `json:"requireStable"`
	RequireGpuCc        bool     `json:"requireGpuCc"`
	ExpectedTrainerURL  string   `json:"expectedTrainerUrl"`
	// ImageSignatures are key ids (hex SHA-256 of the DER public key) one
	// of which the launcher must have verified a signature from before it
	// started the image; the token lists them in image_signatures[]. The
	// enclave repository's release workflow is the only user of the key,
	// so a listed key id means "built by that workflow at a release tag".
	ImageSignatures []string `json:"imageSignatures"`
	// MinRelease is the lowest release (vX.Y.Z) an enclave may run, read
	// off the image's attested TEE_IMAGE_VERSION. Empty accepts any release,
	// including images built before the stamp existed.
	MinRelease string `json:"minRelease"`
	// SourceURI is the repository whose release workflow builds the image
	// and ImageRepo the public registry holding the same digest with its
	// SLSA provenance; together with the token's release stamp they form
	// the verify command the connector logs at dial.
	SourceURI string `json:"sourceUri"`
	ImageRepo string `json:"imageRepo"`
	// ImageSources is the older, per-digest form of the same record. It is
	// consulted for the verify command when the token carries no release
	// stamp and the policy has an entry for the digest.
	ImageSources        map[string]ImageSource `json:"imageSources"`
	Issuer              string                 `json:"issuer"`
	JWKSURL             string                 `json:"jwksUrl"`
	SWName              string                 `json:"swname"`
	HWModel             string                 `json:"hwmodel"`
	TeeSlotHostSuffixes []string               `json:"teeSlotHostSuffixes"`
}

// ImageSource is the public build record of one image digest.
type ImageSource struct {
	Repo      string `json:"repo"`      // e.g. ghcr.io/femled/masseuse-video-tee
	Tag       string `json:"tag"`       // the release tag, e.g. v0.4.0; empty when the image is unstamped
	SourceURI string `json:"sourceUri"` // e.g. github.com/FemLed/masseuse-video-tee
}

// String is "<sourceUri>@<tag>", the source revision the digest was built
// from, or the source alone when the tag is unknown.
func (s ImageSource) String() string {
	if s.Tag == "" {
		return s.SourceURI
	}
	return s.SourceURI + "@" + s.Tag
}

// VerifyCommand is the one-line check anyone can run against the digest:
// slsa-verifier confirms the provenance attached to the image in the public
// registry names this source (at this tag, when known), and the digest is
// the one the enclave attested (VERIFY.md, "The enclave your camera streams
// to").
func (s ImageSource) VerifyCommand(digest string) string {
	cmd := fmt.Sprintf("slsa-verifier verify-image %s@%s --source-uri %s", s.Repo, digest, s.SourceURI)
	if s.Tag != "" {
		cmd += " --source-tag " + s.Tag
	}
	return cmd
}

// Release is what the image says about itself: the release tag and the
// source commit the enclave repository's release workflow baked into it as
// TEE_IMAGE_VERSION and TEE_IMAGE_COMMIT, attested with the rest of the
// container environment. Images built before the stamp existed have none.
type Release struct {
	Version string
	Commit  string
}

func validRegistryPath(s string) bool {
	return s != "" && !strings.ContainsAny(s, " @:\n") && strings.Contains(s, "/")
}

func validSourceURI(s string) bool {
	return s != "" && !strings.Contains(s, "://") && !strings.ContainsAny(s, " \n") && strings.Contains(s, "/")
}

// releaseNumbers parses "vMAJOR.MINOR.PATCH[-pre][+build]" into its three
// numbers and its pre-release suffix; ok is false for anything else.
func releaseNumbers(tag string) (nums [3]int, pre string, ok bool) {
	if !strings.HasPrefix(tag, "v") {
		return nums, "", false
	}
	rest := tag[1:]
	if i := strings.IndexByte(rest, '+'); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.IndexByte(rest, '-'); i >= 0 {
		rest, pre = rest[:i], rest[i+1:]
		if pre == "" {
			return nums, "", false
		}
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return nums, "", false
	}
	for i, p := range parts {
		if p == "" || len(p) > 9 {
			return nums, "", false
		}
		n := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				return nums, "", false
			}
			n = n*10 + int(c-'0')
		}
		nums[i] = n
	}
	return nums, pre, true
}

// ValidRelease reports whether tag is a release tag of the form vX.Y.Z,
// optionally with a pre-release suffix.
func ValidRelease(tag string) bool {
	_, _, ok := releaseNumbers(tag)
	return ok
}

// CompareRelease orders two release tags: -1, 0 or +1 as a is older than,
// the same as or newer than b. A pre-release precedes the release it
// precedes ("v1.2.0-rc.1" < "v1.2.0"); two pre-releases of one version
// compare as strings. Both tags must satisfy ValidRelease.
func CompareRelease(a, b string) int {
	an, ap, _ := releaseNumbers(a)
	bn, bp, _ := releaseNumbers(b)
	for i := range an {
		if an[i] != bn[i] {
			if an[i] < bn[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case ap == bp:
		return 0
	case ap == "":
		return 1
	case bp == "":
		return -1
	case ap < bp:
		return -1
	default:
		return 1
	}
}

func isDigest(d string) bool {
	if !strings.HasPrefix(d, "sha256:") || len(d) != 7+64 {
		return false
	}
	for _, c := range d[7:] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Validate fills defaults and rejects a policy that could not pin anything.
func (p *Policy) Validate() error {
	if len(p.AllowedImageDigests) == 0 && len(p.ImageSignatures) == 0 {
		return errors.New("policy names neither image signing keys nor image digests")
	}
	for _, d := range p.AllowedImageDigests {
		if !isDigest(d) {
			return fmt.Errorf("policy digest %q is not sha256:<64 hex>", d)
		}
	}
	for _, k := range p.ImageSignatures {
		if k == "" || strings.ContainsAny(k, " \n") {
			return fmt.Errorf("policy imageSignatures entry %q is not a key id", k)
		}
	}
	if p.MinRelease != "" && !ValidRelease(p.MinRelease) {
		return fmt.Errorf("policy minRelease %q is not a release tag (vX.Y.Z)", p.MinRelease)
	}
	if p.SourceURI != "" && !validSourceURI(p.SourceURI) {
		return fmt.Errorf("policy sourceUri %q is not a source URI", p.SourceURI)
	}
	if p.ImageRepo != "" && !validRegistryPath(p.ImageRepo) {
		return fmt.Errorf("policy imageRepo %q is not a registry path", p.ImageRepo)
	}
	if (p.SourceURI == "") != (p.ImageRepo == "") {
		return errors.New("policy sourceUri and imageRepo go together")
	}
	for d, s := range p.ImageSources {
		if !isDigest(d) {
			return fmt.Errorf("policy imageSources key %q is not sha256:<64 hex>", d)
		}
		if !validRegistryPath(s.Repo) {
			return fmt.Errorf("policy imageSources[%s].repo %q is not a registry path", d, s.Repo)
		}
		if !strings.HasPrefix(s.Tag, "v") || strings.ContainsAny(s.Tag, " \n") {
			return fmt.Errorf("policy imageSources[%s].tag %q is not a release tag", d, s.Tag)
		}
		if !validSourceURI(s.SourceURI) {
			return fmt.Errorf("policy imageSources[%s].sourceUri %q is not a source URI", d, s.SourceURI)
		}
	}
	if p.Issuer == "" {
		p.Issuer = DefaultIssuer
	}
	if p.JWKSURL == "" {
		p.JWKSURL = DefaultJWKSURL
	}
	if !strings.HasPrefix(p.JWKSURL, "https://") {
		return errors.New("policy jwksUrl is not https")
	}
	if p.SWName == "" {
		p.SWName = DefaultSWName
	}
	if p.HWModel == "" {
		p.HWModel = DefaultHWModel
	}
	return nil
}

// FetchPolicy downloads and validates the service's policy.
func FetchPolicy(ctx context.Context, client *http.Client, service string) (*Policy, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(service, "/")+"/api/tee-policy", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET /api/tee-policy: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /api/tee-policy: status %d", resp.StatusCode)
	}
	var p Policy
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&p); err != nil {
		return nil, fmt.Errorf("GET /api/tee-policy: %w", err)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// PolicyError lists every check the enclave failed.
type PolicyError struct{ Reasons []string }

func (e *PolicyError) Error() string {
	return "attestation refused: " + strings.Join(e.Reasons, "; ")
}

// Result is what a verified enclave looks like.
type Result struct {
	Origin      string
	ImageDigest string
	// Release is the image's attested release stamp; nil for an image built
	// before the stamp existed.
	Release *Release
	// Source is where to check ImageDigest against its provenance: the
	// policy's sourceUri/imageRepo with the token's release tag, or the
	// policy's per-digest record for an unstamped image; nil when the
	// policy publishes neither.
	Source       *ImageSource
	InstanceID   string
	DbgStat      string
	Leaf         *x509.Certificate
	SPKISHA256   [32]byte
	TLSSpkiNonce string // base64url(SPKISHA256), the form eat_nonce carries
	TokenExpiry  time.Time
}

// KeySource resolves an RS256 signing key by key id.
type KeySource interface {
	Key(ctx context.Context, kid string) (*rsa.PublicKey, error)
}

// Verifier checks one origin at a time.
type Verifier struct {
	Policy *Policy
	// Keys resolves JWT signing keys; nil means a RemoteJWKS for the policy's
	// jwksUrl.
	Keys KeySource
	// RootCAs verifies the origin's TLS certificate; nil means the system
	// roots.
	RootCAs *x509.CertPool
	// Now is the clock; nil means time.Now.
	Now     func() time.Time
	Timeout time.Duration

	once sync.Once
	keys KeySource
}

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

func (v *Verifier) keySource() KeySource {
	v.once.Do(func() {
		v.keys = v.Keys
		if v.keys == nil {
			v.keys = NewRemoteJWKS(v.Policy.JWKSURL, nil)
		}
	})
	return v.keys
}

type attestationDoc struct {
	Token        string   `json:"token"`
	Nonces       []string `json:"nonces"`
	TLSSpkiNonce *string  `json:"tlsSpkiNonce"`
	EvidenceKey  struct {
		Alg       string `json:"alg"`
		PublicKey string `json:"publicKey"`
		Nonce     string `json:"nonce"`
	} `json:"evidenceKey"`
}

// Verify fetches and checks origin's attestation. On success the result's
// SPKISHA256 is the key the tunnel must pin.
func (v *Verifier) Verify(ctx context.Context, origin string) (*Result, error) {
	if v.Policy == nil {
		return nil, errors.New("attest: no policy")
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("attest: origin %q is not an https origin", origin)
	}
	origin = "https://" + u.Host
	host := u.Hostname()
	if len(v.Policy.TeeSlotHostSuffixes) > 0 && !hasSuffixAny(host, v.Policy.TeeSlotHostSuffixes) {
		return nil, &PolicyError{[]string{fmt.Sprintf("origin host %s is not under a slot host suffix", host)}}
	}
	timeout := v.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	nonce := make([]byte, 24)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	nonceStr := base64.RawURLEncoding.EncodeToString(nonce)

	var leaf *x509.Certificate
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    v.RootCAs,
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) > 0 {
					leaf = cs.PeerCertificates[0]
				}
				return nil
			},
		},
		DisableKeepAlives: true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/attestation?nonce="+nonceStr, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET /attestation: %w", err)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("GET /attestation: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /attestation: status %d", resp.StatusCode)
	}
	if leaf == nil {
		return nil, errors.New("GET /attestation: no TLS leaf certificate captured")
	}
	var doc attestationDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("GET /attestation: %w", err)
	}
	if doc.Token == "" {
		return nil, errors.New("attestation document carries no token")
	}

	claims, err := VerifyToken(ctx, v.keySource(), doc.Token, v.Policy.Issuer, origin, v.now())
	if err != nil {
		return nil, err
	}

	res := &Result{Origin: origin, Leaf: leaf}
	res.SPKISHA256 = sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	res.TLSSpkiNonce = base64.RawURLEncoding.EncodeToString(res.SPKISHA256[:])
	res.ImageDigest = nestedString(claims.Submods, "container", "image_digest")
	res.Release = releaseOf(claims)
	res.Source = sourceOf(v.Policy, res.ImageDigest, res.Release)
	res.InstanceID = nestedString(claims.Submods, "gce", "instance_id")
	res.DbgStat = claims.DbgStat
	res.TokenExpiry = time.Unix(claims.Expiry, 0)

	if reasons := checkClaims(v.Policy, claims, &doc, nonceStr, res.TLSSpkiNonce); len(reasons) > 0 {
		return nil, &PolicyError{Reasons: reasons}
	}
	return res, nil
}

// containerEnv returns the attested container environment.
func containerEnv(c *Claims) map[string]any {
	env, _ := nestedAny(c.Submods, "container", "env").(map[string]any)
	return env
}

// releaseOf reads the image's release stamp off the attested environment;
// nil when the image carries none.
func releaseOf(c *Claims) *Release {
	env := containerEnv(c)
	version, _ := env["TEE_IMAGE_VERSION"].(string)
	if version == "" {
		return nil
	}
	commit, _ := env["TEE_IMAGE_COMMIT"].(string)
	return &Release{Version: version, Commit: commit}
}

// sourceOf says where digest can be checked against its provenance: the
// policy's source repository and public registry with the release the
// token names, else the policy's per-digest record, else nothing.
func sourceOf(p *Policy, digest string, rel *Release) *ImageSource {
	if p.SourceURI != "" && p.ImageRepo != "" {
		s := &ImageSource{Repo: p.ImageRepo, SourceURI: p.SourceURI}
		if rel != nil && ValidRelease(rel.Version) {
			s.Tag = rel.Version
		} else if old, ok := p.ImageSources[digest]; ok {
			s.Tag = old.Tag
		}
		return s
	}
	if s, ok := p.ImageSources[digest]; ok {
		return &s
	}
	return nil
}

// checkClaims applies the policy to verified claims and returns every
// failed check. Mirrors checkAttestationClaims in the trainer plus the
// nonce bindings from the verifier.
func checkClaims(p *Policy, c *Claims, doc *attestationDoc, nonce, tlsSpki string) []string {
	var reasons []string
	fail := func(format string, args ...any) { reasons = append(reasons, fmt.Sprintf(format, args...)) }

	if c.SWName != p.SWName {
		fail("swname is %s", c.SWName)
	}
	if c.HWModel != p.HWModel {
		fail("hwmodel is %s", c.HWModel)
	}
	if !c.SecBoot {
		fail("secure boot is not on")
	}
	digest := nestedString(c.Submods, "container", "image_digest")
	if digest == "" {
		fail("no image digest in the attestation")
	} else if len(p.AllowedImageDigests) > 0 && !contains(p.AllowedImageDigests, digest) {
		fail("image %s is not in the policy", digest)
	}
	if p.MinRelease != "" {
		version, _ := containerEnv(c)["TEE_IMAGE_VERSION"].(string)
		switch {
		case version == "":
			fail("image carries no release stamp, policy requires %s or later", p.MinRelease)
		case !ValidRelease(version):
			fail("image release stamp %q is not a release tag", version)
		case CompareRelease(version, p.MinRelease) < 0:
			fail("image release %s is older than the policy's minimum %s", version, p.MinRelease)
		}
	}
	switch c.DbgStat {
	case dbgstatProd:
	case dbgstatDebug:
		if !p.AllowDebug {
			fail("debug image, policy does not allow debug")
		}
	default:
		fail("dbgstat is %s", orUnknown(c.DbgStat))
	}
	if p.RequireStable && !containsAny(nestedAny(c.Submods, "confidential_space", "support_attributes"), "STABLE") {
		fail("image is not on a STABLE Confidential Space release")
	}
	if p.RequireGpuCc {
		if mode := nestedString(c.Submods, "nvidia_gpu", "cc_mode"); mode != "ON" {
			fail("GPU confidential computing is %s", orAbsent(mode))
		}
		if gpus, _ := nestedAny(c.Submods, "nvidia_gpu", "gpus").([]any); len(gpus) == 0 {
			fail("no GPU in the attestation")
		}
	}
	if p.ExpectedTrainerURL != "" {
		trainer, _ := containerEnv(c)["TRAINER_URL"].(string)
		if strings.TrimRight(trainer, "/") != strings.TrimRight(p.ExpectedTrainerURL, "/") {
			fail("slot posts to %s, not the expected service", orNowhere(trainer))
		}
	}
	if len(p.ImageSignatures) > 0 {
		sigs, _ := nestedAny(c.Submods, "container", "image_signatures").([]any)
		ok := false
		for _, s := range sigs {
			m, _ := s.(map[string]any)
			if id, _ := m["key_id"].(string); id != "" && contains(p.ImageSignatures, id) {
				ok = true
			}
		}
		if !ok {
			fail("image carries none of the required signatures")
		}
	}

	// Nonce bindings.
	if !nonceMatches(c.EatNonce, nonce) {
		fail("eat_nonce does not echo this run's nonce")
	}
	if doc.EvidenceKey.Alg != "Ed25519" {
		fail("no Ed25519 evidence key in the attestation document")
	} else {
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(doc.EvidenceKey.PublicKey, "="))
		if err != nil || len(raw) != 32 {
			fail("evidence key is not 32 bytes")
		} else {
			h := sha256.Sum256(raw)
			if !nonceMatches(c.EatNonce, base64.RawURLEncoding.EncodeToString(h[:])) {
				fail("evidence key is not bound into eat_nonce")
			}
		}
	}
	switch {
	case doc.TLSSpkiNonce == nil:
		fail("attestation document carries no tlsSpkiNonce")
	case *doc.TLSSpkiNonce != tlsSpki:
		fail("tlsSpkiNonce is not the key this connection negotiated")
	case !nonceMatches(c.EatNonce, tlsSpki):
		fail("TLS key is not bound into eat_nonce")
	}
	if nestedString(c.Submods, "gce", "instance_id") == "" {
		fail("no GCE instance id")
	}
	return reasons
}

// PinnedTLSConfig verifies the chain against roots (nil: system) for
// serverName and additionally requires the leaf's SubjectPublicKeyInfo to
// hash to spki: the key the attestation bound.
func PinnedTLSConfig(roots *x509.CertPool, serverName string, spki [32]byte) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
		ServerName: serverName,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("attest: no peer certificate")
			}
			got := sha256.Sum256(cs.PeerCertificates[0].RawSubjectPublicKeyInfo)
			if got != spki {
				return errors.New("attest: TLS key is not the attested key")
			}
			return nil
		},
	}
}

// Claims are the Confidential Space token claims the checks read.
type Claims struct {
	Issuer    string         `json:"iss"`
	Audience  audience       `json:"aud"`
	Expiry    int64          `json:"exp"`
	IssuedAt  int64          `json:"iat"`
	NotBefore int64          `json:"nbf"`
	SWName    string         `json:"swname"`
	HWModel   string         `json:"hwmodel"`
	DbgStat   string         `json:"dbgstat"`
	SecBoot   bool           `json:"secboot"`
	EatNonce  any            `json:"eat_nonce"`
	Submods   map[string]any `json:"submods"`
}

type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = audience(many)
	return nil
}

// VerifyToken checks an RS256 JWT's signature (key by kid from keys),
// issuer, audience and time claims, and returns its claims.
func VerifyToken(ctx context.Context, keys KeySource, token, issuer, aud string, now time.Time) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("jwt: not three segments")
	}
	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("jwt: header: %w", err)
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		return nil, fmt.Errorf("jwt: header: %w", err)
	}
	if header.Alg != "RS256" {
		return nil, fmt.Errorf("jwt: alg %q, want RS256", header.Alg)
	}
	if header.Kid == "" {
		return nil, errors.New("jwt: no kid")
	}
	key, err := keys.Key(ctx, header.Kid)
	if err != nil {
		return nil, fmt.Errorf("jwt: signing key: %w", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("jwt: signature: %w", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return nil, errors.New("jwt: signature does not verify")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("jwt: payload: %w", err)
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, fmt.Errorf("jwt: payload: %w", err)
	}
	if c.Issuer != issuer {
		return nil, fmt.Errorf("jwt: issuer %q, want %q", c.Issuer, issuer)
	}
	if !contains(c.Audience, aud) {
		return nil, fmt.Errorf("jwt: audience %v does not include %s", []string(c.Audience), aud)
	}
	const leeway = 60
	if c.Expiry == 0 {
		return nil, errors.New("jwt: no exp")
	}
	if now.Unix() > c.Expiry+leeway {
		return nil, errors.New("jwt: expired")
	}
	if c.NotBefore != 0 && now.Unix() < c.NotBefore-leeway {
		return nil, errors.New("jwt: not yet valid")
	}
	if c.IssuedAt != 0 && now.Unix() < c.IssuedAt-leeway {
		return nil, errors.New("jwt: issued in the future")
	}
	return &c, nil
}

// RemoteJWKS fetches RS256 keys from a JWKS URL and caches them.
type RemoteJWKS struct {
	URL    string
	Client *http.Client

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

// NewRemoteJWKS returns a key source for url; client nil means a default
// client with a 10 s timeout.
func NewRemoteJWKS(url string, client *http.Client) *RemoteJWKS {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &RemoteJWKS{URL: url, Client: client}
}

// Key returns the key for kid, refreshing the set when it is unknown or
// older than an hour.
func (j *RemoteJWKS) Key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if k, ok := j.keys[kid]; ok && time.Since(j.fetched) < time.Hour {
		return k, nil
	}
	if err := j.refreshLocked(ctx); err != nil {
		return nil, err
	}
	if k, ok := j.keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("kid %q not in JWKS", kid)
}

func (j *RemoteJWKS) refreshLocked(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.URL, nil)
	if err != nil {
		return err
	}
	resp, err := j.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("JWKS status %d", resp.StatusCode)
	}
	keys, err := ParseJWKS(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	j.keys = keys
	j.fetched = time.Now()
	return nil
}

// ParseJWKS reads a JWK set and returns its RSA keys by kid.
func ParseJWKS(r io.Reader) (map[string]*rsa.PublicKey, error) {
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(r).Decode(&set); err != nil {
		return nil, fmt.Errorf("JWKS: %w", err)
	}
	out := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		n, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			return nil, fmt.Errorf("JWKS key %s: n: %w", k.Kid, err)
		}
		e, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			return nil, fmt.Errorf("JWKS key %s: e: %w", k.Kid, err)
		}
		eInt := new(big.Int).SetBytes(e)
		if !eInt.IsInt64() || eInt.Int64() < 3 || eInt.Int64() > 1<<31 {
			return nil, fmt.Errorf("JWKS key %s: bad exponent", k.Kid)
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(eInt.Int64())}
		if pub.N.BitLen() < 2048 {
			return nil, fmt.Errorf("JWKS key %s: modulus is %d bits", k.Kid, pub.N.BitLen())
		}
		out[k.Kid] = pub
	}
	if len(out) == 0 {
		return nil, errors.New("JWKS has no RSA keys")
	}
	return out, nil
}

// StaticKeys is a KeySource for tests and pinned deployments.
type StaticKeys map[string]*rsa.PublicKey

// Key implements KeySource.
func (s StaticKeys) Key(_ context.Context, kid string) (*rsa.PublicKey, error) {
	if k, ok := s[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("kid %q unknown", kid)
}

func hasSuffixAny(host string, suffixes []string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if ip := net.ParseIP(h); ip != nil {
		return false
	}
	for _, s := range suffixes {
		s = strings.ToLower(strings.TrimSuffix(s, "."))
		if s == "" {
			continue
		}
		if !strings.HasPrefix(s, ".") {
			s = "." + s
		}
		if strings.HasSuffix(h, s) {
			return true
		}
	}
	return false
}

func nonceMatches(eatNonce any, want string) bool {
	switch v := eatNonce.(type) {
	case string:
		return v == want
	case []any:
		for _, e := range v {
			if s, ok := e.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func containsAny(v any, s string) bool {
	list, _ := v.([]any)
	for _, e := range list {
		if str, ok := e.(string); ok && str == s {
			return true
		}
	}
	return false
}

func nestedAny(m map[string]any, path ...string) any {
	cur := any(m)
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

func nestedString(m map[string]any, path ...string) string {
	s, _ := nestedAny(m, path...).(string)
	return s
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func orAbsent(s string) string {
	if s == "" {
		return "absent"
	}
	return s
}

func orNowhere(s string) string {
	if s == "" {
		return "nowhere"
	}
	return s
}
