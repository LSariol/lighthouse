package deploy

import (
	"archive/zip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// download saves the ZIP at url as <download folder>/<name>.zip and returns
// its path.
func (d *Deployer) download(url string, name string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	path := filepath.Join(d.downloadPath, name+".zip")
	out, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer out.Close()

	if _, err := io.Copy(out, resp.Body); err != nil {
		return "", err
	}
	return path, nil
}

// unpack extracts the ZIP at archive into the staging folder, refusing any
// entry that would land outside it ("zip slip").
func (d *Deployer) unpack(archive string) error {
	r, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer r.Close()

	root := filepath.Clean(d.stagingPath) + string(os.PathSeparator)
	for _, file := range r.File {
		path := filepath.Join(d.stagingPath, file.Name)
		if !strings.HasPrefix(path, root) {
			return fmt.Errorf("%q would be written outside the staging folder", file.Name)
		}

		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := extract(file, path); err != nil {
			return err
		}
	}
	return nil
}

func extract(file *zip.File, path string) error {
	rc, err := file.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	out, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
