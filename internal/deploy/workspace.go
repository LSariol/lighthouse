package deploy

import (
	"fmt"
	"os"
	"path/filepath"
)

// cleanUp empties the staging and download folders, creating them if needed.
// Their paths are checked at startup (config.ValidateServe), so neither can be
// the filesystem root or a system folder.
func (d *Deployer) cleanUp() error {
	for _, dir := range []string{d.stagingPath, d.downloadPath} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
		if err := emptyDir(dir); err != nil {
			return err
		}
	}
	return nil
}

// emptyDir removes everything inside dir, keeping dir itself.
func emptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("remove %s: %w", p, err)
		}
	}
	return nil
}
