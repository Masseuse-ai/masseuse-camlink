package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/payload"
	"github.com/FemLed/masseuse-camlink/internal/pesig/petest"
	"github.com/FemLed/masseuse-camlink/internal/provenance"
)

func TestDetectTellsTheLayoutsApart(t *testing.T) {
	cases := []struct {
		exe, goos, goarch string
		layout            Layout
		root              string
	}{
		{"/Applications/Masseuse.app/Contents/MacOS/Masseuse", "darwin", "arm64", LayoutBundle, "/Applications/Masseuse.app"},
		{"/Users/x/Downloads/masseuse-camlink", "darwin", "amd64", LayoutArchive, "/Users/x/Downloads"},
		// The package's name alone does not make a package: the payload does.
		{`C:\Users\x\Masseuse\Masseuse.exe`, "windows", "amd64", LayoutArchive, `C:\Users\x\Masseuse`},
		{`C:\tools\masseuse-camlink.exe`, "windows", "arm64", LayoutArchive, `C:\tools`},
		{"/usr/local/bin/masseuse-camlink", "linux", "amd64", LayoutArchive, "/usr/local/bin"},
		{"/home/x/Masseuse.app/Contents/MacOS/Masseuse", "linux", "amd64", LayoutArchive, "/home/x/Masseuse.app/Contents/MacOS"},
	}
	for _, c := range cases {
		if runtime.GOOS != "windows" && strings.HasPrefix(c.exe, `C:`) {
			continue // filepath is the host's
		}
		if runtime.GOOS == "windows" && strings.HasPrefix(c.exe, "/") {
			continue
		}
		in := Detect(c.exe, c.goos, c.goarch)
		if in.Layout != c.layout || in.Root != c.root {
			t.Fatalf("%s on %s: %s at %s", c.exe, c.goos, in.Layout, in.Root)
		}
	}
	if in := Detect("/x/masseuse-camlink", "linux", "arm"); in.GOARM != "7" {
		t.Fatalf("arm: GOARM %q", in.GOARM)
	}
	// An executable carrying a payload is the Windows package, whatever it
	// is called; the same bytes without one are an archive install.
	dir := t.TempDir()
	renamed := filepath.Join(dir, "Masseuse (1).exe")
	if err := os.WriteFile(renamed, packagedExe(t), 0o755); err != nil {
		t.Fatal(err)
	}
	if in := Detect(renamed, "windows", "amd64"); in.Layout != LayoutPackage || in.Root != dir {
		t.Fatalf("a renamed package: %s at %s", in.Layout, in.Root)
	}
	// An install that came through the zip of releases before 0.13 is
	// still called Masseuse.ai.exe, and is the package all the same.
	viaZip := filepath.Join(dir, "Masseuse.ai.exe")
	_ = os.WriteFile(viaZip, packagedExe(t), 0o755)
	if in := Detect(viaZip, "windows", "amd64"); in.Layout != LayoutPackage {
		t.Fatalf("the package under its pre-0.13 name: %s", in.Layout)
	}
	bare := filepath.Join(dir, "Masseuse.exe")
	_ = os.WriteFile(bare, petest.Image([]byte("connector")), 0o755)
	if in := Detect(bare, "windows", "amd64"); in.Layout != LayoutArchive {
		t.Fatalf("a bare connector under the package's name: %s", in.Layout)
	}
	if in := Detect(renamed, "linux", "amd64"); in.Layout != LayoutArchive {
		t.Fatalf("a payload on linux: %s", in.Layout)
	}
}

// packagedExe is a Windows package: a synthetic connector with a payload.
func packagedExe(t *testing.T) []byte {
	t.Helper()
	packed, err := payload.Append(petest.Image([]byte("connector")), "0.13.0", []payload.Entry{
		{Name: "ffmpeg.exe", Data: petest.Image([]byte("ffmpeg"))},
		{Name: "units/camlink-unit-mk312.exe", Data: petest.Image([]byte("helper"))},
	})
	if err != nil {
		t.Fatal(err)
	}
	return packed
}

func TestStageAndSwapTheWindowsPackage(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	// The install as it stands: the package under the name it kept through
	// a 0.12 install's zip update, and the files that used to lie beside it.
	exe := filepath.Join(root, "Masseuse.ai.exe")
	_ = os.WriteFile(exe, packagedExe(t), 0o755)
	_ = os.WriteFile(filepath.Join(root, "ffmpeg.exe"), []byte("old ffmpeg"), 0o755)
	in := Detect(exe, "windows", "amd64")
	if in.Layout != LayoutPackage {
		t.Fatalf("layout %s", in.Layout)
	}
	name, sums, prov := in.Artifact("v0.13.0")
	if name != PackageExe || sums != "checksums-windows.txt" || prov != "windows.intoto.jsonl" {
		t.Fatalf("artifact %s %s %s", name, sums, prov)
	}
	// The download: the release's Masseuse.exe, a package too.
	newPackage, err := payload.Append(petest.Image([]byte("connector v2")), "0.13.0", []payload.Entry{{Name: "ffmpeg.exe", Data: petest.Image([]byte("ffmpeg v2"))}})
	if err != nil {
		t.Fatal(err)
	}
	staged := &Staged{Tag: "v0.13.0", Name: PackageExe, Path: filepath.Join(t.TempDir(), PackageExe)}
	_ = os.WriteFile(staged.Path, newPackage, 0o600)
	var ran []string
	inst := &Installer{Install: in, Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		ran = append(ran, name+" "+strings.Join(args, " "))
		if filepath.Base(name) == PackageExe && len(args) == 1 && args[0] == "--version" {
			return []byte("v0.13.0 go1.27.1\n"), nil
		}
		return nil, errors.New("unexpected")
	}}
	stageDir := filepath.Join(t.TempDir(), "staged")
	sroot, err := inst.Stage(ctx, staged, stageDir)
	if err != nil {
		t.Fatal(err)
	}
	if sroot != stageDir || len(ran) != 1 {
		t.Fatalf("stage: root %s, ran %v", sroot, ran)
	}
	if _, err := os.Stat(staged.Path); err != nil {
		t.Fatal("the download was moved rather than copied")
	}
	// A download that is not a package is refused before anything moves.
	bare := &Staged{Tag: "v0.13.0", Name: PackageExe, Path: filepath.Join(t.TempDir(), PackageExe)}
	_ = os.WriteFile(bare.Path, petest.Image([]byte("connector v2")), 0o600)
	if _, err := inst.Stage(ctx, bare, filepath.Join(t.TempDir(), "s2")); err == nil || !strings.Contains(err.Error(), "not the Windows package") {
		t.Fatalf("a bare connector staged as the package: %v", err)
	}

	previous, err := inst.Swap(sroot)
	if err != nil {
		t.Fatal(err)
	}
	if previous != filepath.Join(root, ".previous") {
		t.Fatalf("previous at %s", previous)
	}
	read := func(p string) []byte { b, _ := os.ReadFile(p); return b }
	// The running program keeps its name; the old one is aside; nothing
	// else in the directory is touched.
	if !bytes.Equal(read(exe), newPackage) {
		t.Fatal("the new package is not in place under the running name")
	}
	if !bytes.Equal(read(filepath.Join(previous, filepath.Base(exe))), packagedExe(t)) {
		t.Fatal("the old package was not kept aside under its own name")
	}
	if string(read(filepath.Join(root, "ffmpeg.exe"))) != "old ffmpeg" {
		t.Fatal("a file beside the package was touched")
	}
	if entries, _ := os.ReadDir(root); len(entries) != 3 { // the package, ffmpeg.exe, .previous
		t.Fatalf("%d entries in the install directory", len(entries))
	}
}

func TestArtifactNamesFollowTheRelease(t *testing.T) {
	cases := []struct {
		in                    Install
		name, checksums, prov string
	}{
		{Install{Layout: LayoutBundle, GOOS: "darwin", GOARCH: "arm64"}, "Masseuse.ai-0.11.0.dmg", "checksums-darwin.txt", "darwin.intoto.jsonl"},
		{Install{Layout: LayoutPackage, GOOS: "windows", GOARCH: "amd64"}, "Masseuse.exe", "checksums-windows.txt", "windows.intoto.jsonl"},
		{Install{Layout: LayoutArchive, GOOS: "linux", GOARCH: "amd64"}, "masseuse-camlink_0.11.0_linux_amd64.tar.gz", "checksums.txt", "multiple.intoto.jsonl"},
		{Install{Layout: LayoutArchive, GOOS: "linux", GOARCH: "arm", GOARM: "7"}, "masseuse-camlink_0.11.0_linux_armv7.tar.gz", "checksums.txt", "multiple.intoto.jsonl"},
		{Install{Layout: LayoutArchive, GOOS: "windows", GOARCH: "arm64"}, "masseuse-camlink_0.11.0_windows_arm64.zip", "checksums.txt", "multiple.intoto.jsonl"},
		{Install{Layout: LayoutArchive, GOOS: "darwin", GOARCH: "amd64"}, "masseuse-camlink_0.11.0_darwin_amd64.tar.gz", "checksums.txt", "multiple.intoto.jsonl"},
		{Install{Layout: LayoutDesktop, GOOS: "linux", GOARCH: "amd64"}, "Masseuse.ai-0.11.0-linux-amd64.tar.gz", "checksums-linux.txt", "linux.intoto.jsonl"},
	}
	for _, c := range cases {
		name, sums, prov := c.in.Artifact("v0.11.0")
		if name != c.name || sums != c.checksums || prov != c.prov {
			t.Fatalf("%+v: %s %s %s", c.in, name, sums, prov)
		}
	}
}

func TestDetectRootIsTheWindowsInstall(t *testing.T) {
	dir := t.TempDir()
	// The Linux desktop archive: the window and the connector in one
	// directory; the connector is what is version-checked and named in
	// the swap, the directory is what is replaced.
	exe := filepath.Join(dir, "masseuse-camlink")
	if err := os.WriteFile(exe, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	in, err := DetectRoot(exe, dir, "linux", "amd64")
	if err != nil || in.Layout != LayoutDesktop || in.Root != dir || in.Exe != exe {
		t.Fatalf("desktop: %+v %v", in, err)
	}
	if got := (&Installer{Install: in}).stagedExe("/staged"); got != filepath.Join("/staged", "masseuse-camlink") {
		t.Fatalf("staged exe %s", got)
	}
	// The macOS bundle, by its .app root, wherever the connector lies in it.
	app := filepath.Join(dir, "Masseuse.app")
	if err := os.MkdirAll(filepath.Join(app, "Contents", "MacOS"), 0o755); err != nil {
		t.Fatal(err)
	}
	in, err = DetectRoot(filepath.Join(app, "Contents", "MacOS", "masseuse-camlink"), app, "darwin", "arm64")
	if err != nil || in.Layout != LayoutBundle || in.Root != app {
		t.Fatalf("bundle: %+v %v", in, err)
	}
	// The Windows package, by the window's executable: that file is what
	// is swapped and whose --version is checked, so it becomes Exe.
	pkg := filepath.Join(dir, "Masseuse.exe")
	if err := os.WriteFile(pkg, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	in, err = DetectRoot(filepath.Join(dir, "bin", "abc", "masseuse-camlink.exe"), pkg, "windows", "amd64")
	if err != nil || in.Layout != LayoutPackage || in.Root != dir || in.Exe != pkg {
		t.Fatalf("package: %+v %v", in, err)
	}
	// Anything else is refused: a missing root, a plain file on Linux.
	if _, err := DetectRoot(exe, filepath.Join(dir, "gone"), "linux", "amd64"); err == nil {
		t.Fatal("a missing root was taken")
	}
	if _, err := DetectRoot(exe, exe, "linux", "amd64"); err == nil {
		t.Fatal("a plain file was taken as the root")
	}
}

func TestStagedExeFollowsTheBundleAcrossTheWindow(t *testing.T) {
	// A staged bundle from before the window: the connector is the
	// executable, under the running program's name.
	old := filepath.Join(t.TempDir(), "Masseuse.app")
	if err := os.MkdirAll(filepath.Join(old, "Contents", "MacOS"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(old, "Contents", "MacOS", "Masseuse"), []byte("x"), 0o755)
	// A staged bundle with the window: the connector beside it.
	newer := filepath.Join(t.TempDir(), "Masseuse.app")
	if err := os.MkdirAll(filepath.Join(newer, "Contents", "MacOS"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(newer, "Contents", "MacOS", "Masseuse"), []byte("w"), 0o755)
	_ = os.WriteFile(filepath.Join(newer, "Contents", "MacOS", "masseuse-camlink"), []byte("c"), 0o755)
	// Running as the connector beside the window: the new bundle's
	// connector is checked; an older bundle's executable stands in.
	in := Install{Exe: "/Applications/Masseuse.app/Contents/MacOS/masseuse-camlink", Layout: LayoutBundle, GOOS: "darwin", GOARCH: "arm64"}
	if got := (&Installer{Install: in}).stagedExe(newer); got != filepath.Join(newer, "Contents", "MacOS", "masseuse-camlink") {
		t.Fatalf("new from new: %s", got)
	}
	if got := (&Installer{Install: in}).stagedExe(old); got != filepath.Join(old, "Contents", "MacOS", "Masseuse") {
		t.Fatalf("old from new: %s", got)
	}
	// Running as the bundle's executable before the window: the new
	// bundle's executable, the window, is what is checked.
	in.Exe = "/Applications/Masseuse.app/Contents/MacOS/Masseuse"
	if got := (&Installer{Install: in}).stagedExe(newer); got != filepath.Join(newer, "Contents", "MacOS", "Masseuse") {
		t.Fatalf("new from old: %s", got)
	}
}

func TestRestartAnswersTheShellOnEverySystem(t *testing.T) {
	// The desktop window starts the connector with RelaunchEnv set and
	// starts the new version itself on RelaunchExitCode: Restart must not
	// exec or spawn anything, whatever the system and the layout.
	t.Setenv(RelaunchEnv, "1")
	for _, layout := range []Layout{LayoutArchive, LayoutBundle, LayoutPackage, LayoutDesktop} {
		in := Install{Exe: filepath.Join(t.TempDir(), "nothing-here"), Layout: layout, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}
		if err := (&Installer{Install: in}).Restart(nil, nil); !errors.Is(err, ErrRelaunch) {
			t.Fatalf("%s: Restart = %v, want ErrRelaunch", layout, err)
		}
	}
}

func TestChecksumOfReadsBothForms(t *testing.T) {
	sums := []byte("0dc247219e983125d82e241911b159493f386b62328feb99871d1a0e4002b179  masseuse-camlink_0.10.0_linux_amd64.tar.gz\n" +
		"9971776E36239D2D31414622D1B5E19FDEFA55A7A959CBB8580332C8A08022A6 *Masseuse.ai-0.10.0-windows.zip\r\n" +
		"garbage line\n" +
		"nothex  short.txt\n")
	if got, err := ChecksumOf(sums, "masseuse-camlink_0.10.0_linux_amd64.tar.gz"); err != nil || got != "0dc247219e983125d82e241911b159493f386b62328feb99871d1a0e4002b179" {
		t.Fatalf("two-space form: %q %v", got, err)
	}
	if got, err := ChecksumOf(sums, "Masseuse.ai-0.10.0-windows.zip"); err != nil || got != "9971776e36239d2d31414622d1b5e19fdefa55a7a959cbb8580332c8a08022a6" {
		t.Fatalf("asterisk form: %q %v", got, err)
	}
	if _, err := ChecksumOf(sums, "short.txt"); err == nil {
		t.Fatal("a short checksum was taken")
	}
	if _, err := ChecksumOf(sums, "missing"); err == nil {
		t.Fatal("a missing name was found")
	}
}

// The recorded v0.10.0 release files (internal/provenance/testdata/release).
const recorded = "../provenance/testdata/release"

// releaseServer serves a release's assets under /latest/download/ and
// /download/<tag>/ the way GitHub does.
func releaseServer(t *testing.T, tag string, files map[string][]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var name string
		switch {
		case strings.HasPrefix(r.URL.Path, "/latest/download/"):
			name = strings.TrimPrefix(r.URL.Path, "/latest/download/")
		case strings.HasPrefix(r.URL.Path, "/download/"+tag+"/"):
			name = strings.TrimPrefix(r.URL.Path, "/download/"+tag+"/")
		default:
			http.NotFound(w, r)
			return
		}
		b, ok := files[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(b)))
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func recordedFiles(t *testing.T) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	entries, err := os.ReadDir(recorded)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(recorded, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files[e.Name()] = b
	}
	return files
}

func TestCheckReadsTheLatestReleaseOffItsSignature(t *testing.T) {
	// The real verifier over the real v0.10.0 files: the tag comes from the
	// certificate, and the comparison with the running version decides.
	srv := releaseServer(t, "v0.10.0", recordedFiles(t))
	verifier := NewVerifier("", nil)
	for _, c := range []struct {
		current string
		want    error
	}{
		{"v0.9.4", nil},
		{"v0.10.0", ErrUpToDate},
		{"v0.11.0", ErrUpToDate},
		{"(devel)", ErrNotARelease},
	} {
		cl := &Client{ReleasesURL: srv.URL, Verifier: verifier, Current: c.current}
		rel, err := cl.Check(context.Background())
		if !errors.Is(err, c.want) {
			t.Fatalf("current %s: %v", c.current, err)
		}
		if c.want == nil && (rel == nil || rel.Tag != "v0.10.0" || rel.Signature == nil || !strings.Contains(rel.Signature.Identity, "release.yml@refs/tags/v0.10.0")) {
			t.Fatalf("current %s: release %+v", c.current, rel)
		}
	}
	// A checksums file altered on the way: not verified, not installed.
	files := recordedFiles(t)
	files["checksums.txt"] = append(files["checksums.txt"], []byte("deadbeef  extra\n")...)
	bad := releaseServer(t, "v0.10.0", files)
	cl := &Client{ReleasesURL: bad.URL, Verifier: verifier, Current: "v0.9.4"}
	if _, err := cl.Check(context.Background()); err == nil || errors.Is(err, ErrUpToDate) {
		t.Fatalf("a tampered checksums file passed: %v", err)
	}
	// No network: an error, not a panic, not a downgrade.
	down := releaseServer(t, "v0.10.0", map[string][]byte{})
	cl = &Client{ReleasesURL: down.URL, Verifier: verifier, Current: "v0.9.4"}
	if _, err := cl.Check(context.Background()); err == nil {
		t.Fatal("a release with no files was accepted")
	}
}

// fakeVerifier accepts every bundle and says the release is tag; the real
// verification is tested in internal/provenance.
type fakeVerifier struct {
	tag    string
	refuse bool
}

func (f *fakeVerifier) VerifyBlob(context.Context, []byte, []byte, provenance.BlobSigner) (*provenance.BlobResult, error) {
	if f.refuse {
		return nil, errors.New("refused")
	}
	return &provenance.BlobResult{Tag: f.tag, Identity: "fake"}, nil
}

func (f *fakeVerifier) VerifyBlobProvenance(_ context.Context, name string, _ []byte, bundle []byte, _ provenance.BlobSigner) (*provenance.BlobResult, error) {
	if f.refuse || !bytes.Contains(bundle, []byte(name)) {
		return nil, errors.New("provenance refused")
	}
	return &provenance.BlobResult{Tag: f.tag, Identity: "fake-builder"}, nil
}

// tarGz builds a tar.gz of files (name -> content), executables by name.
func tarGz(t *testing.T, files map[string]string, exec ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		mode := int64(0o644)
		for _, e := range exec {
			if e == name {
				mode = 0o755
			}
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// fakeRelease is a v0.11.0 for an archive install on this platform: the
// archive, its signed-looking checksums and a provenance naming it.
func fakeRelease(t *testing.T, in Install, archive []byte) (string, map[string][]byte) {
	t.Helper()
	name, _, prov := in.Artifact("v0.11.0")
	return name, map[string][]byte{
		"checksums.txt":               []byte(sha(archive) + "  " + name + "\nabc  other\n"),
		"checksums.txt.sigstore.json": []byte("{bundle}"),
		prov:                          []byte(`{"provenance for ` + name + `"}`),
		name:                          archive,
	}
}

func archiveInstall(t *testing.T) Install {
	t.Helper()
	in := Detect(filepath.Join(t.TempDir(), "masseuse-camlink"), runtime.GOOS, runtime.GOARCH)
	in.Layout = LayoutArchive
	return in
}

func TestDownloadVerifiesTheArtifactBeforeNamingIt(t *testing.T) {
	ctx := context.Background()
	in := archiveInstall(t)
	archive := tarGz(t, map[string]string{"masseuse-camlink": "new binary", "units/camlink-unit-x": "helper"}, "masseuse-camlink", "units/camlink-unit-x")
	name, files := fakeRelease(t, in, archive)
	srv := releaseServer(t, "v0.11.0", files)
	state := t.TempDir()
	cl := &Client{ReleasesURL: srv.URL, Verifier: &fakeVerifier{tag: "v0.11.0"}, Current: "v0.10.0", Install: in, StateDir: state}
	rel, err := cl.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s, err := cl.Download(ctx, rel)
	if err != nil {
		t.Fatal(err)
	}
	if s.Tag != "v0.11.0" || s.Name != name || s.SHA256 != sha(archive) || s.Size != int64(len(archive)) || s.Provenance == nil {
		t.Fatalf("staged %+v", s)
	}
	if s.Path != filepath.Join(state, "updates", "v0.11.0", name) {
		t.Fatalf("path %s", s.Path)
	}
	if _, err := os.Stat(s.Path + ".part"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the .part file was left behind")
	}
	// A second pass finds the file and re-checks it instead of downloading.
	again, err := cl.Download(ctx, rel)
	if err != nil || again.Path != s.Path {
		t.Fatalf("again: %+v %v", again, err)
	}
	// A file altered on disk since is downloaded afresh.
	if err := os.WriteFile(s.Path, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Download(ctx, rel); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(s.Path)
	if !bytes.Equal(got, archive) {
		t.Fatal("the corrupt copy was not replaced")
	}
	// Prune keeps the release named and drops the rest.
	_ = os.MkdirAll(filepath.Join(state, "updates", "v0.10.9"), 0o700)
	cl.Prune("v0.11.0")
	if _, err := os.Stat(filepath.Join(state, "updates", "v0.10.9")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an old download survived Prune")
	}
	if _, err := os.Stat(s.Path); err != nil {
		t.Fatal("Prune took the kept release")
	}
}

func TestDownloadRefuses(t *testing.T) {
	ctx := context.Background()
	in := archiveInstall(t)
	archive := tarGz(t, map[string]string{"masseuse-camlink": "new binary"}, "masseuse-camlink")
	name, files := fakeRelease(t, in, archive)

	// The bytes served are not the bytes signed.
	tampered := map[string][]byte{}
	for k, v := range files {
		tampered[k] = v
	}
	tampered[name] = append([]byte("x"), archive...)
	srv := releaseServer(t, "v0.11.0", tampered)
	cl := &Client{ReleasesURL: srv.URL, Verifier: &fakeVerifier{tag: "v0.11.0"}, Current: "v0.10.0", Install: in, StateDir: t.TempDir()}
	rel, err := cl.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Download(ctx, rel); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("a hash mismatch was accepted: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(cl.StateDir, "updates", "v0.11.0")); len(entries) != 0 {
		t.Fatalf("leftovers after a refused download: %v", entries)
	}

	// No provenance naming the artifact.
	noprov := map[string][]byte{}
	for k, v := range files {
		noprov[k] = v
	}
	_, _, prov := in.Artifact("v0.11.0")
	noprov[prov] = []byte(`{"provenance for something else"}`)
	srv2 := releaseServer(t, "v0.11.0", noprov)
	cl = &Client{ReleasesURL: srv2.URL, Verifier: &fakeVerifier{tag: "v0.11.0"}, Current: "v0.10.0", Install: in, StateDir: t.TempDir()}
	rel, _ = cl.Check(ctx)
	if _, err := cl.Download(ctx, rel); err == nil || !strings.Contains(err.Error(), "provenance") {
		t.Fatalf("missing provenance was accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cl.StateDir, "updates", "v0.11.0", name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an artifact without provenance was kept")
	}

	// Not enough disk.
	srv3 := releaseServer(t, "v0.11.0", files)
	cl = &Client{ReleasesURL: srv3.URL, Verifier: &fakeVerifier{tag: "v0.11.0"}, Current: "v0.10.0", Install: in, StateDir: t.TempDir(),
		FreeSpace: func(string) (uint64, error) { return 1 << 20, nil }}
	rel, _ = cl.Check(ctx)
	var space *SpaceError
	if _, err := cl.Download(ctx, rel); !errors.As(err, &space) {
		t.Fatalf("a full disk was not noticed: %v", err)
	}

	// The artifact missing from the release.
	partial := map[string][]byte{}
	for k, v := range files {
		if k != name {
			partial[k] = v
		}
	}
	srv4 := releaseServer(t, "v0.11.0", partial)
	cl = &Client{ReleasesURL: srv4.URL, Verifier: &fakeVerifier{tag: "v0.11.0"}, Current: "v0.10.0", Install: in, StateDir: t.TempDir()}
	rel, _ = cl.Check(ctx)
	if _, err := cl.Download(ctx, rel); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("a missing artifact: %v", err)
	}
}

func TestUnpackRefusesEscapesAndLinks(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"../escape", "/abs", "a/../../b"} {
		archive := filepath.Join(dir, "x.tar.gz")
		if err := os.WriteFile(archive, tarGz(t, map[string]string{name: "x"}), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := Unpack(archive, filepath.Join(dir, "out"), MaxUnpacked); err == nil {
			t.Fatalf("%q was unpacked", name)
		}
	}
	// A symlink entry.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"})
	_ = tw.Close()
	_ = gz.Close()
	archive := filepath.Join(dir, "link.tar.gz")
	_ = os.WriteFile(archive, buf.Bytes(), 0o600)
	if err := Unpack(archive, filepath.Join(dir, "out2"), MaxUnpacked); err == nil {
		t.Fatal("a symlink was unpacked")
	}
	// Over budget.
	big := filepath.Join(dir, "big.tar.gz")
	_ = os.WriteFile(big, tarGz(t, map[string]string{"f": strings.Repeat("x", 4096)}), 0o600)
	if err := Unpack(big, filepath.Join(dir, "out3"), 1024); err == nil {
		t.Fatal("an archive over budget was unpacked")
	}
	// A zip with the same escape.
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	w, _ := zw.Create("../escape.txt")
	_, _ = w.Write([]byte("x"))
	_ = zw.Close()
	z := filepath.Join(dir, "x.zip")
	_ = os.WriteFile(z, zb.Bytes(), 0o600)
	if err := Unpack(z, filepath.Join(dir, "out4"), MaxUnpacked); err == nil {
		t.Fatal("a zip escape was unpacked")
	}
	// A good zip keeps the executable bit and nests directories.
	zb.Reset()
	zw = zip.NewWriter(&zb)
	h := &zip.FileHeader{Name: "units/camlink-unit-x.exe", Method: zip.Deflate}
	h.SetMode(0o755)
	w, _ = zw.CreateHeader(h)
	_, _ = w.Write([]byte("exe"))
	_ = zw.Close()
	_ = os.WriteFile(z, zb.Bytes(), 0o600)
	if err := Unpack(z, filepath.Join(dir, "out5"), MaxUnpacked); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "out5", "units", "camlink-unit-x.exe")); err != nil || string(b) != "exe" {
		t.Fatalf("zip entry: %q %v", b, err)
	}
}

func TestStateRemembersAndForgets(t *testing.T) {
	dir := t.TempDir()
	if s := LoadState(dir); s.Staged != nil || len(s.Failed) != 0 {
		t.Fatalf("empty state = %+v", s)
	}
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	s := &State{LastCheck: now, Staged: &Staged{Tag: "v0.11.0", Name: "x", Path: "/x"}}
	s.MarkFailed("v0.10.9", now.Add(-8*24*time.Hour)) // long ago: pruned at the next mark
	s.MarkFailed("v0.11.1", now)
	if err := s.Save(dir); err != nil {
		t.Fatal(err)
	}
	got := LoadState(dir)
	if got.Staged == nil || got.Staged.Tag != "v0.11.0" || !got.LastCheck.Equal(now) {
		t.Fatalf("loaded %+v", got)
	}
	if !got.FailedRecently("v0.11.1", now.Add(time.Hour)) || got.FailedRecently("v0.11.1", now.Add(25*time.Hour)) {
		t.Fatal("FailedRecently is off")
	}
	if _, ok := got.Failed["v0.10.9"]; ok {
		t.Fatal("a week-old failure was kept")
	}
	// RemovePrevious deletes the directory and forgets it.
	prev := filepath.Join(dir, "prev")
	_ = os.MkdirAll(filepath.Join(prev, "sub"), 0o700)
	got.Previous, got.From = prev, "v0.10.0"
	if err := got.RemovePrevious(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(prev); !errors.Is(err, os.ErrNotExist) || got.Previous != "" {
		t.Fatal("the previous install stayed")
	}
	if err := got.RemovePrevious(); !errors.Is(err, ErrNoPrevious) {
		t.Fatalf("second remove = %v", err)
	}
}

func TestCheckInPlace(t *testing.T) {
	dir := t.TempDir()
	in := Detect(filepath.Join(dir, "masseuse-camlink"), runtime.GOOS, runtime.GOARCH)
	if err := CheckInPlace(in); err != nil {
		t.Fatalf("a writable directory: %v", err)
	}
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("read-only directories need a non-root Unix user")
	}
	ro := filepath.Join(dir, "ro")
	_ = os.Mkdir(ro, 0o500)
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
	var inPlace *InPlaceError
	if err := CheckInPlace(Detect(filepath.Join(ro, "masseuse-camlink"), runtime.GOOS, runtime.GOARCH)); !errors.As(err, &inPlace) {
		t.Fatalf("a read-only directory: %v", err)
	}
	// A bundle on the disk image gets the drag-to-Applications word.
	if runtime.GOOS == "darwin" {
		err := CheckInPlace(Install{Layout: LayoutBundle, Root: "/Volumes/Masseuse.ai/Masseuse.app", Exe: "/Volumes/Masseuse.ai/Masseuse.app/Contents/MacOS/Masseuse"})
		if err == nil || !strings.Contains(err.Error(), "drag Masseuse to Applications") {
			t.Fatalf("disk image: %v", err)
		}
	}
}

func TestStageAndSwapAnArchiveInstall(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	exe := filepath.Join(root, "masseuse-camlink")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	// The install as it stands: the program, a helper, a note of our own.
	_ = os.WriteFile(exe, []byte("old binary"), 0o755)
	_ = os.MkdirAll(filepath.Join(root, "units"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "units", "camlink-unit-old"), []byte("old helper"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "mine.txt"), []byte("kept"), 0o644)
	in := Detect(exe, runtime.GOOS, runtime.GOARCH)
	in.Layout = LayoutArchive

	binName := "masseuse-camlink"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	archive := tarGz(t, map[string]string{binName: "new binary", "units/camlink-unit-new": "new helper", "LICENSE": "l"}, binName, "units/camlink-unit-new")
	staged := &Staged{Tag: "v0.11.0", Name: "a.tar.gz", Path: filepath.Join(t.TempDir(), "a.tar.gz")}
	_ = os.WriteFile(staged.Path, archive, 0o600)

	var ran []string
	inst := &Installer{Install: in, Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		ran = append(ran, name+" "+strings.Join(args, " "))
		if strings.HasSuffix(name, binName) && len(args) == 1 && args[0] == "--version" {
			return []byte("v0.11.0 go1.27.1\n"), nil
		}
		return nil, errors.New("unexpected")
	}}
	stageDir := filepath.Join(t.TempDir(), "staged")
	sroot, err := inst.Stage(ctx, staged, stageDir)
	if err != nil {
		t.Fatal(err)
	}
	if sroot != stageDir || len(ran) != 1 || !strings.Contains(ran[0], "--version") {
		t.Fatalf("stage: root %s, ran %v", sroot, ran)
	}
	// The wrong version is refused before anything moves.
	wrong := &Installer{Install: in, Run: func(context.Context, string, ...string) ([]byte, error) { return []byte("v0.10.0 go1.27.1\n"), nil }}
	if _, err := wrong.Stage(ctx, staged, filepath.Join(t.TempDir(), "s2")); err == nil || !strings.Contains(err.Error(), "says it is") {
		t.Fatalf("a wrong version staged: %v", err)
	}

	previous, err := inst.Swap(sroot)
	if err != nil {
		t.Fatal(err)
	}
	if previous != filepath.Join(root, ".previous") {
		t.Fatalf("previous at %s", previous)
	}
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	if read(exe) != "new binary" || read(filepath.Join(root, "units", "camlink-unit-new")) != "new helper" || read(filepath.Join(root, "LICENSE")) != "l" {
		t.Fatal("the new files are not in place")
	}
	if read(filepath.Join(root, "mine.txt")) != "kept" {
		t.Fatal("a file of the person's own was touched")
	}
	if read(filepath.Join(previous, filepath.Base(exe))) != "old binary" || read(filepath.Join(previous, "units", "camlink-unit-old")) != "old helper" {
		t.Fatal("the old install was not kept aside")
	}
	if _, err := os.Stat(filepath.Join(root, "units", "camlink-unit-old")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the old helper stayed beside the new")
	}
	if fi, _ := os.Stat(exe); runtime.GOOS != "windows" && fi.Mode()&0o111 == 0 {
		t.Fatal("the new program is not executable")
	}
	// The new program, once it runs, removes the previous install.
	st := &State{Previous: previous, From: "v0.10.0"}
	if err := st.RemovePrevious(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(previous); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("previous stayed")
	}
}

func TestSwapPutsThingsBackOnFailure(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("needs a directory this user cannot write")
	}
	root := t.TempDir()
	exe := filepath.Join(root, "masseuse-camlink")
	_ = os.WriteFile(exe, []byte("old"), 0o755)
	in := Detect(exe, runtime.GOOS, runtime.GOARCH)
	in.Layout = LayoutArchive
	staged := t.TempDir()
	_ = os.WriteFile(filepath.Join(staged, "masseuse-camlink"), []byte("new"), 0o755)
	// The install directory cannot take the previous directory: nothing moves.
	_ = os.Chmod(root, 0o500)
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
	inst := &Installer{Install: in}
	if _, err := inst.Swap(staged); err == nil {
		t.Fatal("a swap into a read-only directory succeeded")
	}
	_ = os.Chmod(root, 0o700)
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Fatal("the old program was touched")
	}
	if _, err := os.Stat(filepath.Join(root, ".previous")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a previous directory was left")
	}
}

func TestDownloadReadsContentLengthForSpace(t *testing.T) {
	// A server that says a huge Content-Length is refused before a byte is read.
	in := archiveInstall(t)
	name, _, _ := in.Artifact("v0.11.0")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "checksums.txt"):
			_, _ = io.WriteString(w, sha([]byte("x"))+"  "+name+"\n")
		case strings.HasSuffix(r.URL.Path, name):
			w.Header().Set("Content-Length", fmt.Sprint(MaxArtifactSize+1))
			w.WriteHeader(200)
		default:
			_, _ = io.WriteString(w, "{}")
		}
	}))
	t.Cleanup(srv.Close)
	cl := &Client{ReleasesURL: srv.URL, Verifier: &fakeVerifier{tag: "v0.11.0"}, Current: "v0.10.0", Install: in, StateDir: t.TempDir()}
	rel, err := cl.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Download(context.Background(), rel); err == nil || !strings.Contains(err.Error(), "more than expected") {
		t.Fatalf("an oversized artifact: %v", err)
	}
}
