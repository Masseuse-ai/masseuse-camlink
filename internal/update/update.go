// Package update keeps the connector current: it finds the latest release
// on GitHub, verifies it the way VERIFY.md tells a reader to (the checksum
// file's keyless signature by this repository's release workflow at the
// tag, the artifact's hash, its SLSA provenance, and the platform's own
// signature checks), downloads it, and puts it in place of the running
// program, which restarts into it (README "Updates").
//
// The service is not involved: masseuse.ai names no version and cannot
// push code. The only authority is the release workflow's signature, read
// from the Sigstore bundle with the trust root the connector carries
// (internal/provenance). Nothing is run, moved or deleted before it has
// verified; a failure at any step (no network, a bad signature, a
// checksum that does not match, no disk space) leaves the running program
// as it is and is reported once.
//
// Three install layouts are told apart from the program's own file: the
// macOS application bundle (Masseuse.app, updated from the disk image),
// the Windows package (the one Masseuse.exe carrying ffmpeg.exe and the
// helpers as its payload, internal/payload, updated from the release's
// Masseuse.exe), and a bare archive install (the binary with
// units/ beside it, updated from the goreleaser archive for the platform).
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/attest"
	"github.com/FemLed/masseuse-camlink/internal/payload"
	"github.com/FemLed/masseuse-camlink/internal/provenance"
)

// Defaults for this repository's releases.
const (
	// DefaultReleasesURL is where the release assets live; "latest/download/"
	// and "download/<tag>/" hang off it.
	DefaultReleasesURL = "https://github.com/FemLed/masseuse-camlink/releases"
	// SourceURI is the repository whose release workflow signs the releases.
	SourceURI = "github.com/FemLed/masseuse-camlink"
	// MaxArtifactSize bounds one download (the DMG is the largest, tens of MB).
	MaxArtifactSize = 512 << 20
	// SpaceMargin is free space kept beyond what the update itself needs.
	SpaceMargin = 200 << 20
)

// ErrUpToDate says the latest release is not newer than the running program.
var ErrUpToDate = errors.New("update: up to date")

// ErrNotARelease says the running program is not a release build, so there
// is nothing to compare with and nothing to update.
var ErrNotARelease = errors.New("update: not a release build")

// Layout is how the program is installed.
type Layout int

const (
	// LayoutArchive is the bare binary with units/ beside it: the goreleaser
	// archives (Linux, Windows arm64, anyone using the tar.gz on a Mac).
	LayoutArchive Layout = iota
	// LayoutBundle is the macOS application bundle, Masseuse.app.
	LayoutBundle
	// LayoutPackage is the Windows package: one Masseuse.exe, ffmpeg.exe
	// and the helpers inside it as a payload (internal/payload), unpacked
	// under the state directory when it runs. Until v0.12 the package was
	// a zip with those files beside an executable called Masseuse.ai.exe.
	LayoutPackage
)

func (l Layout) String() string {
	switch l {
	case LayoutBundle:
		return "bundle"
	case LayoutPackage:
		return "package"
	}
	return "archive"
}

// Install is where and how the running program is installed.
type Install struct {
	// Exe is the running executable, symlinks resolved.
	Exe    string
	Layout Layout
	// Root is what an update replaces: the .app directory for a bundle,
	// the executable's directory otherwise.
	Root string
	// GOOS and GOARCH are the platform; GOARM is "7" on 32-bit ARM.
	GOOS, GOARCH, GOARM string
}

// PackageExe is the Windows package's name on the release, and the name
// the package is staged under (packaging/windows): Masseuse, the way the
// Mac bundle is Masseuse.app, the program calling itself Masseuse.ai. The
// running program may be called something else (a download renamed,
// "Masseuse (1).exe"; Masseuse.ai.exe in an install that came through the
// zip of releases before 0.13); Swap keeps the running name.
const PackageExe = "Masseuse.exe"

// Detect reads the layout off the executable: a bundle when it lies in
// <name>.app/Contents/MacOS on darwin, the Windows package when it carries
// a payload (internal/payload; whatever it is called), an archive install
// otherwise.
func Detect(exe, goos, goarch string) Install {
	in := Install{Exe: exe, GOOS: goos, GOARCH: goarch}
	if goarch == "arm" {
		in.GOARM = "7"
	}
	dir := filepath.Dir(exe)
	switch {
	case goos == "darwin" && filepath.Base(dir) == "MacOS" && filepath.Base(filepath.Dir(dir)) == "Contents" &&
		strings.HasSuffix(filepath.Dir(filepath.Dir(dir)), ".app"):
		in.Layout = LayoutBundle
		in.Root = filepath.Dir(filepath.Dir(dir))
	case goos == "windows" && payload.Carries(exe):
		in.Layout = LayoutPackage
		in.Root = dir
	default:
		in.Layout = LayoutArchive
		in.Root = dir
	}
	return in
}

// Artifact names the release asset an update of this install takes, the
// checksum file that lists it, and the provenance file that covers it,
// for the release tag.
func (in Install) Artifact(tag string) (name, checksums, provenanceFile string) {
	version := strings.TrimPrefix(tag, "v")
	switch in.Layout {
	case LayoutBundle:
		return "Masseuse.ai-" + version + ".dmg", "checksums-darwin.txt", "darwin.intoto.jsonl"
	case LayoutPackage:
		return PackageExe, "checksums-windows.txt", "windows.intoto.jsonl"
	}
	arch := in.GOARCH
	if arch == "arm" && in.GOARM != "" {
		arch += "v" + in.GOARM
	}
	ext := ".tar.gz"
	if in.GOOS == "windows" {
		ext = ".zip"
	}
	return "masseuse-camlink_" + version + "_" + in.GOOS + "_" + arch + ext, "checksums.txt", "multiple.intoto.jsonl"
}

// CurrentGOARM is the GOARM the running binary was built with, from its
// build settings ("" when not ARM).
func CurrentGOARM() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key == "GOARM" {
			return strings.TrimSuffix(s.Value, ",softfloat")
		}
	}
	return ""
}

// Release is a verified release: its tag (from the signature's certificate)
// and the checksum file that signature covers.
type Release struct {
	Tag string
	// Checksums is checksums.txt, verified.
	Checksums []byte
	// Signature is what the bundle said.
	Signature *provenance.BlobResult
}

// Staged is a verified download, ready to be installed.
type Staged struct {
	Tag string
	// Name is the asset's file name; Path is where it lies, under the
	// updates directory.
	Name string
	Path string
	// SHA256 is the artifact's digest, as the signed checksum file lists it.
	SHA256 string
	Size   int64
	// Provenance is what the SLSA provenance said.
	Provenance *provenance.BlobResult
}

// Client finds, verifies and downloads releases.
type Client struct {
	// ReleasesURL is DefaultReleasesURL, or a stand-in in tests.
	ReleasesURL string
	// HTTP is the client used; nil means a default with sane timeouts.
	HTTP *http.Client
	// Verifier checks the Sigstore bundles: a provenance.Verifier whose
	// BuilderSAN is the generic generator's, which a nil field gives.
	Verifier BlobVerifier
	// Current is the running release tag (buildinfo.Version()).
	Current string
	// Install is the running install.
	Install Install
	// StateDir is the connector's state directory: downloads go under
	// its updates/ directory.
	StateDir string
	// FreeSpace reports the free bytes on the volume holding a path; nil
	// means the platform's own.
	FreeSpace func(path string) (uint64, error)
	Logger    *slog.Logger
}

func (c *Client) releasesURL() string {
	if c.ReleasesURL != "" {
		return strings.TrimSuffix(c.ReleasesURL, "/")
	}
	return DefaultReleasesURL
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

// BlobVerifier is the part of provenance.Verifier the updater uses.
type BlobVerifier interface {
	VerifyBlob(ctx context.Context, blob, bundle []byte, signer provenance.BlobSigner) (*provenance.BlobResult, error)
	VerifyBlobProvenance(ctx context.Context, name string, digest, bundle []byte, signer provenance.BlobSigner) (*provenance.BlobResult, error)
}

// NewVerifier is the provenance.Verifier for this repository's releases:
// the generic generator as the builder, its TUF cache of its own under the
// state directory's update/ (the enclave checks keep theirs beside it, and
// the two never write the same files).
func NewVerifier(stateDir string, log *slog.Logger) *provenance.Verifier {
	v := &provenance.Verifier{BuilderSAN: regexp.MustCompile(provenance.DefaultGenericBuilderSAN), Logger: log}
	if stateDir != "" {
		v.CacheDir = filepath.Join(stateDir, "update")
	}
	return v
}

func (c *Client) verifier() BlobVerifier {
	if c.Verifier != nil {
		return c.Verifier
	}
	return NewVerifier(c.StateDir, c.logger())
}

func (c *Client) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.New(slog.DiscardHandler)
}

func (c *Client) freeSpace(path string) (uint64, error) {
	if c.FreeSpace != nil {
		return c.FreeSpace(path)
	}
	return freeSpace(path)
}

// UpdatesDir is where downloads are staged.
func (c *Client) UpdatesDir() string { return filepath.Join(c.StateDir, "updates") }

// latestURL is an asset of the latest release; tagURL an asset of a release.
func (c *Client) latestURL(asset string) string { return c.releasesURL() + "/latest/download/" + asset }
func (c *Client) tagURL(tag, asset string) string {
	return c.releasesURL() + "/download/" + tag + "/" + asset
}

// fetch downloads a small asset (a checksum file, a bundle) whole.
func (c *Client) fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "masseuse-camlink/"+c.Current)
	res, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", url, res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s: larger than %d bytes", url, limit)
	}
	return b, nil
}

// Check finds the latest release and says whether it is newer than the
// running program: the checksum file and its bundle are fetched from the
// "latest" release, the bundle verified as a signature by this
// repository's release workflow at a release tag, and that tag (from the
// certificate, never from a file name) compared with Current. ErrUpToDate
// when it is not newer; ErrNotARelease when Current is not a release tag.
func (c *Client) Check(ctx context.Context) (*Release, error) {
	if !attest.ValidRelease(c.Current) {
		return nil, ErrNotARelease
	}
	checksums, err := c.fetch(ctx, c.latestURL("checksums.txt"), 1<<20)
	if err != nil {
		return nil, fmt.Errorf("update: fetching the latest release's checksums: %w", err)
	}
	bundle, err := c.fetch(ctx, c.latestURL("checksums.txt.sigstore.json"), 4<<20)
	if err != nil {
		return nil, fmt.Errorf("update: fetching the latest release's signature: %w", err)
	}
	sig, err := c.verifier().VerifyBlob(ctx, checksums, bundle, provenance.BlobSigner{SourceURI: SourceURI})
	if err != nil {
		return nil, fmt.Errorf("update: the latest release's checksums do not verify: %w", err)
	}
	if strings.Contains(sig.Tag, "-") {
		// A pre-release is never installed on its own.
		return nil, ErrUpToDate
	}
	if attest.CompareRelease(sig.Tag, c.Current) <= 0 {
		return nil, ErrUpToDate
	}
	return &Release{Tag: sig.Tag, Checksums: checksums, Signature: sig}, nil
}

// Download fetches the artifact for this install from the release and
// verifies it: the artifact's checksum file (checksums.txt from the
// Release, or the darwin or windows one fetched and verified at the exact
// tag), free space, the download to a .part file under updates/<tag>/,
// the SHA-256 against the signed list, and the SLSA provenance naming the
// artifact. Only then is the file given its name.
func (c *Client) Download(ctx context.Context, rel *Release) (*Staged, error) {
	name, checksumsName, provName := c.Install.Artifact(rel.Tag)
	signer := provenance.BlobSigner{SourceURI: SourceURI, Tag: rel.Tag}
	checksums := rel.Checksums
	if checksumsName != "checksums.txt" {
		list, err := c.fetch(ctx, c.tagURL(rel.Tag, checksumsName), 1<<20)
		if err != nil {
			return nil, fmt.Errorf("update: fetching %s: %w", checksumsName, err)
		}
		bundle, err := c.fetch(ctx, c.tagURL(rel.Tag, checksumsName+".sigstore.json"), 4<<20)
		if err != nil {
			return nil, fmt.Errorf("update: fetching %s's signature: %w", checksumsName, err)
		}
		if _, err := c.verifier().VerifyBlob(ctx, list, bundle, signer); err != nil {
			return nil, fmt.Errorf("update: %s does not verify: %w", checksumsName, err)
		}
		checksums = list
	}
	want, err := ChecksumOf(checksums, name)
	if err != nil {
		return nil, fmt.Errorf("update: %w", err)
	}
	prov, err := c.fetch(ctx, c.tagURL(rel.Tag, provName), 4<<20)
	if err != nil {
		return nil, fmt.Errorf("update: fetching %s: %w", provName, err)
	}

	dir := filepath.Join(c.UpdatesDir(), rel.Tag)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("update: %w", err)
	}
	final := filepath.Join(dir, name)
	if st, err := os.Stat(final); err == nil && st.Size() > 0 {
		// Downloaded on an earlier pass: still checked below.
		if got, err := fileSHA256(final); err == nil && got == want {
			c.logger().Info("update: already downloaded", "tag", rel.Tag, "name", name)
			return c.finish(ctx, rel, name, final, want, prov, provName, signer)
		}
		_ = os.Remove(final)
	}

	url := c.tagURL(rel.Tag, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "masseuse-camlink/"+c.Current)
	res, err := c.http().Do(req)
	if err != nil {
		return nil, fmt.Errorf("update: fetching %s: %w", name, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("update: fetching %s: HTTP %d", name, res.StatusCode)
	}
	if res.ContentLength > MaxArtifactSize {
		return nil, fmt.Errorf("update: %s is %d bytes, more than expected", name, res.ContentLength)
	}
	size := res.ContentLength
	if size < 0 {
		size = 64 << 20 // unknown: assume a large one
	}
	if err := c.checkSpace(dir, size); err != nil {
		return nil, err
	}

	part := final + ".part"
	f, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("update: %w", err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(res.Body, MaxArtifactSize+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(part)
		return nil, fmt.Errorf("update: downloading %s: %w", name, err)
	}
	if n > MaxArtifactSize {
		_ = os.Remove(part)
		return nil, fmt.Errorf("update: %s is larger than %d bytes", name, MaxArtifactSize)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		_ = os.Remove(part)
		return nil, fmt.Errorf("update: %s: sha256 %s, the signed checksums say %s", name, got, want)
	}
	if err := os.Rename(part, final); err != nil {
		_ = os.Remove(part)
		return nil, fmt.Errorf("update: %w", err)
	}
	return c.finish(ctx, rel, name, final, want, prov, provName, signer)
}

// finish verifies the provenance for a downloaded artifact and describes it.
func (c *Client) finish(ctx context.Context, rel *Release, name, path, want string, prov []byte, provName string, signer provenance.BlobSigner) (*Staged, error) {
	digest, _ := hex.DecodeString(want)
	var (
		res  *provenance.BlobResult
		errs []error
	)
	for _, line := range strings.Split(string(prov), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		r, err := c.verifier().VerifyBlobProvenance(ctx, name, digest, []byte(line), signer)
		if err == nil {
			res = r
			break
		}
		errs = append(errs, err)
	}
	if res == nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("update: %s: no SLSA provenance for %s verifies: %w", provName, name, errors.Join(errs...))
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	return &Staged{Tag: rel.Tag, Name: name, Path: path, SHA256: want, Size: st.Size(), Provenance: res}, nil
}

// checkSpace refuses a download the volume cannot hold: the artifact, twice
// that unpacked, and the margin.
func (c *Client) checkSpace(dir string, size int64) error {
	need := uint64(size)*3 + SpaceMargin
	free, err := c.freeSpace(dir)
	if err != nil {
		c.logger().Debug("update: free space unknown", "dir", dir, "err", err)
		return nil
	}
	if free < need {
		return &SpaceError{Need: need, Free: free, Dir: dir}
	}
	installFree, err := c.freeSpace(filepath.Dir(c.Install.Root))
	if err == nil && installFree < need {
		return &SpaceError{Need: need, Free: installFree, Dir: filepath.Dir(c.Install.Root)}
	}
	return nil
}

// SpaceError says the volume is too full for the update.
type SpaceError struct {
	Need, Free uint64
	Dir        string
}

func (e *SpaceError) Error() string {
	return fmt.Sprintf("update: not enough free space in %s: %d MB free, %d MB needed", e.Dir, e.Free>>20, e.Need>>20)
}

// ChecksumOf reads name's SHA-256 (hex) off a checksum file in sha256sum's
// two-space form (or the one-space, asterisk form Windows writes).
func ChecksumOf(checksums []byte, name string) (string, error) {
	for _, line := range strings.Split(string(checksums), "\n") {
		line = strings.TrimSpace(line)
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		file := strings.TrimPrefix(fields[1], "*")
		if file == name {
			sum := strings.ToLower(fields[0])
			if len(sum) != 64 {
				return "", fmt.Errorf("checksum of %s is not a sha256", name)
			}
			if _, err := hex.DecodeString(sum); err != nil {
				return "", fmt.Errorf("checksum of %s is not hex", name)
			}
			return sum, nil
		}
	}
	return "", fmt.Errorf("the checksums list no %s", name)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Prune removes staged downloads of releases other than keep (the one
// about to be installed, or "" for all), and leftovers of interrupted ones.
func (c *Client) Prune(keep string) {
	entries, err := os.ReadDir(c.UpdatesDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.Name() == keep {
			continue
		}
		_ = os.RemoveAll(filepath.Join(c.UpdatesDir(), e.Name()))
	}
}

// InPlaceError says why this install cannot be updated where it is.
type InPlaceError struct {
	Reason string
	Err    error
}

func (e *InPlaceError) Error() string { return "update: " + e.Reason }
func (e *InPlaceError) Unwrap() error { return e.Err }

// CheckInPlace says whether the install can be replaced where it is: its
// parent directory must be writable by this user (a probe file is created
// and removed). A bundle on a mounted disk image, or a binary under a
// system directory, cannot be.
func CheckInPlace(in Install) error {
	parent := filepath.Dir(in.Root)
	if in.Layout != LayoutBundle {
		parent = in.Root
	}
	probe, err := os.CreateTemp(parent, ".masseuse-update-*")
	if err != nil {
		reason := fmt.Sprintf("%s is not writable by this user", parent)
		if in.Layout == LayoutBundle && strings.HasPrefix(in.Root, "/Volumes/") {
			reason = "running from the disk image; drag Masseuse to Applications and open it from there to get updates"
		}
		return &InPlaceError{Reason: reason, Err: err}
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
}
