package update

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Unpack extracts a release archive (tar.gz or zip) into dir, refusing
// paths that would leave it, links, and anything past maxTotal bytes.
// Executables keep their mode bits (zip entries from goreleaser carry
// them; a Windows zip has none and needs none).
func Unpack(archive, dir string, maxTotal int64) error {
	switch {
	case strings.HasSuffix(archive, ".tar.gz"), strings.HasSuffix(archive, ".tgz"):
		return untar(archive, dir, maxTotal)
	case strings.HasSuffix(archive, ".zip"):
		return unzip(archive, dir, maxTotal)
	}
	return fmt.Errorf("update: %s is not an archive this program unpacks", filepath.Base(archive))
}

// safeJoin is dir/name when name stays inside dir.
func safeJoin(dir, name string) (string, error) {
	if name == "" || strings.HasPrefix(name, "/") || strings.HasPrefix(name, "\\") || filepath.VolumeName(name) != "" || strings.Contains(name, "..") {
		return "", fmt.Errorf("update: archive entry %q is not a plain relative path", name)
	}
	name = filepath.FromSlash(name)
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("update: archive entry %q is not a plain relative path", name)
	}
	p := filepath.Join(dir, name)
	rel, err := filepath.Rel(dir, p)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("update: archive entry %q leaves the directory", name)
	}
	return p, nil
}

func writeEntry(path string, mode os.FileMode, r io.Reader, budget *int64) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	perm := os.FileMode(0o644)
	if mode&0o111 != 0 {
		perm = 0o755
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, *budget+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	*budget -= n
	if *budget < 0 {
		return errors.New("update: the archive unpacks to more than allowed")
	}
	return nil
}

func untar(archive, dir string, maxTotal int64) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("update: %s: %w", filepath.Base(archive), err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	budget := maxTotal
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("update: %s: %w", filepath.Base(archive), err)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			p, err := safeJoin(dir, h.Name)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			p, err := safeJoin(dir, h.Name)
			if err != nil {
				return err
			}
			if err := writeEntry(p, os.FileMode(h.Mode), tr, &budget); err != nil {
				return err
			}
		default:
			return fmt.Errorf("update: %s: entry %q is a %c, not a file", filepath.Base(archive), h.Name, h.Typeflag)
		}
	}
}

func unzip(archive, dir string, maxTotal int64) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return fmt.Errorf("update: %s: %w", filepath.Base(archive), err)
	}
	defer zr.Close()
	budget := maxTotal
	for _, e := range zr.File {
		if strings.HasSuffix(e.Name, "/") || e.Mode().IsDir() {
			p, err := safeJoin(dir, strings.TrimSuffix(e.Name, "/"))
			if err != nil {
				return err
			}
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
			continue
		}
		if !e.Mode().IsRegular() {
			return fmt.Errorf("update: %s: entry %q is not a file", filepath.Base(archive), e.Name)
		}
		p, err := safeJoin(dir, e.Name)
		if err != nil {
			return err
		}
		rc, err := e.Open()
		if err != nil {
			return err
		}
		err = writeEntry(p, e.Mode(), rc, &budget)
		_ = rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
