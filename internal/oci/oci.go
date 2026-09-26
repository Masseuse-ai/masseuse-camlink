// Package oci reads, anonymously, what a public registry holds about one
// image digest: the manifests that refer to it (OCI 1.1 referrers, with the
// tag-schema fallback older registries use), a manifest by tag or digest,
// and a blob by digest. It is the least a client needs to fetch an image's
// Sigstore bundles and cosign attestations (internal/provenance); pulling
// images is out of scope.
package oci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Media types the package understands.
const (
	MediaTypeIndex    = "application/vnd.oci.image.index.v1+json"
	MediaTypeManifest = "application/vnd.oci.image.manifest.v1+json"
)

const maxBody = 4 << 20

// Repository is a registry host and a repository path within it.
type Repository struct {
	Host string // e.g. ghcr.io
	Path string // e.g. femled/masseuse-video-tee
}

var (
	hostRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*(:[0-9]{1,5})?$`)
	pathRE = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*$`)
)

// ParseRepository splits "host[:port]/path" (no scheme, tag or digest)
// along the distribution specification's grammar.
func ParseRepository(s string) (Repository, error) {
	host, path, ok := strings.Cut(s, "/")
	if !ok || !hostRE.MatchString(host) || !pathRE.MatchString(path) {
		return Repository{}, fmt.Errorf("oci: %q is not host/repository", s)
	}
	return Repository{Host: host, Path: path}, nil
}

func (r Repository) String() string { return r.Host + "/" + r.Path }

// Descriptor is an OCI content descriptor.
type Descriptor struct {
	MediaType    string            `json:"mediaType"`
	Digest       string            `json:"digest"`
	Size         int64             `json:"size"`
	ArtifactType string            `json:"artifactType,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
}

// Manifest is the part of an image manifest or index the package reads.
type Manifest struct {
	MediaType    string       `json:"mediaType"`
	ArtifactType string       `json:"artifactType,omitempty"`
	Layers       []Descriptor `json:"layers,omitempty"`
	Manifests    []Descriptor `json:"manifests,omitempty"`
	Subject      *Descriptor  `json:"subject,omitempty"`
}

// Client talks to registries with anonymous pull tokens.
type Client struct {
	// HTTP is the client used; nil means one with a 30 s timeout.
	HTTP *http.Client

	mu     sync.Mutex
	tokens map[string]string // by host/path
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Referrers lists the manifests that name digest as their subject: the
// referrers API first, then the "sha256-<hex>" tag older registries and
// clients use. Descriptors from the tag may lack the artifact type and
// annotations of the manifests they point to; read the manifest to know.
func (c *Client) Referrers(ctx context.Context, repo Repository, digest string) ([]Descriptor, error) {
	if err := checkDigest(digest); err != nil {
		return nil, err
	}
	var index Manifest
	_, body, err := c.get(ctx, repo, "/referrers/"+digest, MediaTypeIndex)
	switch {
	case err == nil:
		if err := json.Unmarshal(body, &index); err != nil {
			return nil, fmt.Errorf("oci: referrers of %s: %w", digest, err)
		}
	case errors.Is(err, errNotFound):
		_, body, err = c.get(ctx, repo, "/manifests/sha256-"+strings.TrimPrefix(digest, "sha256:"), MediaTypeIndex)
		if errors.Is(err, errNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(body, &index); err != nil {
			return nil, fmt.Errorf("oci: referrers tag of %s: %w", digest, err)
		}
	default:
		return nil, err
	}
	return index.Manifests, nil
}

// Manifest fetches a manifest by tag or digest and checks its digest when
// one was asked for.
func (c *Client) Manifest(ctx context.Context, repo Repository, reference string) (*Manifest, []byte, error) {
	if strings.ContainsAny(reference, "/ \n") || reference == "" {
		return nil, nil, fmt.Errorf("oci: bad reference %q", reference)
	}
	_, body, err := c.get(ctx, repo, "/manifests/"+reference, MediaTypeManifest+", "+MediaTypeIndex)
	if err != nil {
		return nil, nil, err
	}
	if strings.HasPrefix(reference, "sha256:") && digestOf(body) != reference {
		return nil, nil, fmt.Errorf("oci: manifest %s does not hash to its digest", reference)
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, nil, fmt.Errorf("oci: manifest %s: %w", reference, err)
	}
	return &m, body, nil
}

// Blob fetches a blob by digest and checks it.
func (c *Client) Blob(ctx context.Context, repo Repository, digest string) ([]byte, error) {
	if err := checkDigest(digest); err != nil {
		return nil, err
	}
	_, body, err := c.get(ctx, repo, "/blobs/"+digest, "*/*")
	if err != nil {
		return nil, err
	}
	if digestOf(body) != digest {
		return nil, fmt.Errorf("oci: blob %s does not hash to its digest", digest)
	}
	return body, nil
}

// ErrNotFound is wrapped by errors for a manifest or blob the registry
// answers 404 to.
var ErrNotFound = errors.New("not found")

var errNotFound = ErrNotFound

// get performs GET /v2/<repo><path>, obtaining an anonymous token on 401
// the way the registry's WWW-Authenticate challenge says.
func (c *Client) get(ctx context.Context, repo Repository, path, accept string) (string, []byte, error) {
	u := "https://" + repo.Host + "/v2/" + repo.Path + path
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return "", nil, err
		}
		req.Header.Set("Accept", accept)
		if tok := c.token(repo); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := c.http().Do(req)
		if err != nil {
			return "", nil, fmt.Errorf("oci: GET %s: %w", u, err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		_ = resp.Body.Close()
		if err != nil {
			return "", nil, fmt.Errorf("oci: GET %s: %w", u, err)
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			if len(body) > maxBody {
				return "", nil, fmt.Errorf("oci: GET %s: body over %d bytes", u, maxBody)
			}
			return resp.Header.Get("Content-Type"), body, nil
		case resp.StatusCode == http.StatusNotFound:
			return "", nil, fmt.Errorf("oci: GET %s: %w", u, errNotFound)
		case resp.StatusCode == http.StatusUnauthorized && attempt == 0:
			if err := c.authenticate(ctx, repo, resp.Header.Get("WWW-Authenticate")); err != nil {
				return "", nil, fmt.Errorf("oci: GET %s: %w", u, err)
			}
		default:
			return "", nil, fmt.Errorf("oci: GET %s: status %d", u, resp.StatusCode)
		}
	}
	return "", nil, fmt.Errorf("oci: GET %s: still unauthorized with a token", u)
}

func (c *Client) token(repo Repository) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tokens[repo.String()]
}

// authenticate follows a Bearer challenge to its realm for an anonymous
// token: GET <realm>?service=<service>&scope=<scope>.
func (c *Client) authenticate(ctx context.Context, repo Repository, challenge string) error {
	scheme, rest, _ := strings.Cut(challenge, " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return fmt.Errorf("unsupported challenge %q", challenge)
	}
	params := map[string]string{}
	for _, kv := range splitChallenge(rest) {
		k, v, ok := strings.Cut(kv, "=")
		if ok {
			params[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	realm := params["realm"]
	if !strings.HasPrefix(realm, "https://") {
		return fmt.Errorf("challenge realm %q is not https", realm)
	}
	q := url.Values{}
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + repo.Path + ":pull"
	}
	q.Set("scope", scope)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token: status %d", resp.StatusCode)
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return fmt.Errorf("token: %w", err)
	}
	if tok.Token == "" {
		tok.Token = tok.AccessToken
	}
	if tok.Token == "" {
		return errors.New("token: empty")
	}
	c.mu.Lock()
	if c.tokens == nil {
		c.tokens = map[string]string{}
	}
	c.tokens[repo.String()] = tok.Token
	c.mu.Unlock()
	return nil
}

// splitChallenge splits `a="x",b="y,z"` on commas outside quotes.
func splitChallenge(s string) []string {
	var parts []string
	quoted, start := false, 0
	for i, r := range s {
		switch {
		case r == '"':
			quoted = !quoted
		case r == ',' && !quoted:
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}

func checkDigest(d string) error {
	if !strings.HasPrefix(d, "sha256:") || len(d) != 7+64 {
		return fmt.Errorf("oci: %q is not sha256:<64 hex>", d)
	}
	if _, err := hex.DecodeString(d[7:]); err != nil {
		return fmt.Errorf("oci: %q is not sha256:<64 hex>", d)
	}
	return nil
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
