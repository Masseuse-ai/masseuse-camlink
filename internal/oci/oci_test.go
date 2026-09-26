package oci

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseRepository(t *testing.T) {
	good := map[string]Repository{
		"ghcr.io/femled/masseuse-video-tee": {Host: "ghcr.io", Path: "femled/masseuse-video-tee"},
		"localhost:5000/x":                  {Host: "localhost:5000", Path: "x"},
		"us-central1-docker.pkg.dev/p/r/i":  {Host: "us-central1-docker.pkg.dev", Path: "p/r/i"},
	}
	for in, want := range good {
		got, err := ParseRepository(in)
		if err != nil || got != want {
			t.Errorf("%q: %+v %v", in, got, err)
		}
	}
	for _, in := range []string{"", "ghcr.io", "/x", "ghcr.io/", "https://ghcr.io/x", "ghcr.io/x:latest", "ghcr.io/x@sha256:00", "a:b:c/x", "ghcr.io/x y"} {
		if _, err := ParseRepository(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestSplitChallenge(t *testing.T) {
	got := splitChallenge(`realm="https://ghcr.io/token",service="ghcr.io",scope="repository:a/b:pull,push"`)
	want := []string{`realm="https://ghcr.io/token"`, `service="ghcr.io"`, `scope="repository:a/b:pull,push"`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("%q", got)
	}
}

func TestClientFollowsTheChallengeAndChecksDigests(t *testing.T) {
	blob := []byte("hello")
	blobDigest := digestOf(blob)
	var tokens, unauthorized int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			tokens++
			if r.URL.Query().Get("scope") != "repository:a/b:pull" || r.URL.Query().Get("service") != "test" {
				http.Error(w, "bad scope", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"t"}`))
		case r.Header.Get("Authorization") != "Bearer t":
			unauthorized++
			w.Header().Set("WWW-Authenticate", `Bearer realm="https://`+r.Host+`/token",service="test"`)
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/v2/a/b/blobs/"+blobDigest:
			_, _ = w.Write(blob)
		case strings.HasPrefix(r.URL.Path, "/v2/a/b/blobs/"):
			_, _ = w.Write([]byte("other bytes"))
		case r.URL.Path == "/v2/a/b/manifests/tag":
			_, _ = w.Write([]byte(`{"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[{"digest":"` + blobDigest + `","mediaType":"x"}]}`))
		case r.URL.Path == "/v2/a/b/referrers/"+blobDigest:
			http.NotFound(w, r)
		case r.URL.Path == "/v2/a/b/manifests/sha256-"+blobDigest[7:]:
			_, _ = w.Write([]byte(`{"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":"sha256:` + strings.Repeat("11", 32) + `","mediaType":"application/vnd.oci.image.manifest.v1+json","artifactType":"application/vnd.oci.empty.v1+json"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client()}
	repo := Repository{Host: srv.Listener.Addr().String(), Path: "a/b"}
	ctx := context.Background()

	got, err := c.Blob(ctx, repo, blobDigest)
	if err != nil || string(got) != "hello" {
		t.Fatalf("blob: %q %v", got, err)
	}
	if _, err := c.Blob(ctx, repo, "sha256:"+strings.Repeat("22", 32)); err == nil || !strings.Contains(err.Error(), "does not hash") {
		t.Errorf("mismatched blob accepted: %v", err)
	}
	m, _, err := c.Manifest(ctx, repo, "tag")
	if err != nil || len(m.Layers) != 1 || m.Layers[0].Digest != blobDigest {
		t.Fatalf("manifest: %+v %v", m, err)
	}
	if _, _, err := c.Manifest(ctx, repo, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing manifest: %v", err)
	}
	refs, err := c.Referrers(ctx, repo, blobDigest)
	if err != nil || len(refs) != 1 || refs[0].ArtifactType != "application/vnd.oci.empty.v1+json" {
		t.Fatalf("referrers: %+v %v", refs, err)
	}
	if refs, err := c.Referrers(ctx, repo, "sha256:"+strings.Repeat("33", 32)); err != nil || refs != nil {
		t.Errorf("no referrers: %+v %v", refs, err)
	}
	if tokens != 1 || unauthorized != 1 {
		t.Errorf("tokens %d unauthorized %d", tokens, unauthorized)
	}
}

func TestClientRefusesPlainRealms(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="http://`+r.Host+`/token"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client()}
	_, _, err := c.Manifest(context.Background(), Repository{Host: srv.Listener.Addr().String(), Path: "a/b"}, "tag")
	if err == nil || !strings.Contains(err.Error(), "not https") {
		t.Fatalf("err %v", err)
	}
}
