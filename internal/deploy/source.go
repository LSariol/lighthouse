package deploy

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// MaxArchiveSize is the most a repository may unpack to. It protects the
// server's disk from a runaway archive.
const MaxArchiveSize = 2 << 30 // 2 GB

// extract unpacks a GitHub tarball (gzipped tar, every entry in one
// top-level folder) into dest, dropping that folder. File modes are kept, so
// scripts stay executable. Entries that would land outside dest, and links
// pointing outside it, are refused.
func extract(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("the archive isn't a gzipped tarball: %w", err)
	}
	defer gz.Close()

	root, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}

	tr := tar.NewReader(gz)
	var total int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read the archive: %w", err)
		}

		// GitHub's pax_global_header carries the commit; it isn't a file.
		if hdr.Typeflag == tar.TypeXGlobalHeader || hdr.Typeflag == tar.TypeXHeader {
			continue
		}

		rel := stripTop(hdr.Name)
		if rel == "" {
			continue // the top-level folder itself
		}
		path, err := inside(root, rel)
		if err != nil {
			return err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			total += hdr.Size
			if total > MaxArchiveSize {
				return fmt.Errorf("the repository unpacks to more than %d MB", MaxArchiveSize>>20)
			}
			if err := writeFile(path, tr, hdr.FileInfo().Mode().Perm()); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// A link may point anywhere inside the repository, never out of it.
			target := hdr.Linkname
			if filepath.IsAbs(target) {
				return fmt.Errorf("%q links to an absolute path (%s)", rel, target)
			}
			if _, err := inside(root, filepath.Join(filepath.Dir(rel), target)); err != nil {
				return fmt.Errorf("%q links outside the repository (%s)", rel, target)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(target, path); err != nil {
				return fmt.Errorf("create link %q: %w", rel, err)
			}
		default:
			// Hard links, devices and the like don't belong in a repository.
		}
	}
}

// stripTop removes the archive's top-level folder from name.
func stripTop(name string) string {
	name = strings.TrimPrefix(filepath.ToSlash(name), "./")
	_, rest, _ := strings.Cut(name, "/")
	return strings.TrimSuffix(rest, "/")
}

// inside joins root and rel, and refuses a result outside root ("zip slip").
func inside(root string, rel string) (string, error) {
	path := filepath.Join(root, filepath.FromSlash(rel))
	if path != root && !strings.HasPrefix(path, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("%q would be written outside the deploy folder", rel)
	}
	return path, nil
}

func writeFile(path string, r io.Reader, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// Owner can always read and write; group and others get what the
	// repository says, at most read and execute.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode&0o755|0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
