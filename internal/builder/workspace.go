package builder

import (
	"fmt"
	"os"
	"path/filepath"
)

func cleanUp() error {

	// Clean Staging Folder
	err := cleanupAll("STAGING_PATH")
	if err != nil {
		return fmt.Errorf("failed to clean staging area at %s: %w", os.Getenv("STAGING_PATH"), err)
	}
	err = os.MkdirAll(os.Getenv("STAGING_PATH")+"/Working", 0755)
	if err != nil {
		return fmt.Errorf("failed to recreate staging area at %s: %w", os.Getenv("STAGING_PATH"), err)
	}

	//Clean Download Folder
	err = cleanupAll("DOWNLOAD_PATH")
	if err != nil {
		return fmt.Errorf("failed to clean download area at %s: %w", os.Getenv("DOWNLOAD_PATH"), err)
	}
	err = os.MkdirAll(os.Getenv("DOWNLOAD_PATH"), 0755)
	if err != nil {
		return fmt.Errorf("failed to recreate download area at %s: %w", os.Getenv("DOWNLOAD_PATH"), err)
	}

	return nil
}

func cleanupAll(base string) error {
	base = os.Getenv(base)
	if base == "" {
		return fmt.Errorf("STAGING_PATH not set")
	}

	// Read all entries inside the staging path
	entries, err := os.ReadDir(base)
	if err != nil {
		return fmt.Errorf("failed to read staging dir: %w", err)
	}

	for _, e := range entries {
		p := filepath.Join(base, e.Name())
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("failed to remove %s: %w", p, err)
		}
	}

	return nil
}
