package deploy

import (
	"archive/zip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPlaceholderKeys(t *testing.T) {
	config := `
services:
  app:
    environment:
      DATABASE_URL: ${PLOP_DATABASE_URL}
      TMDB: ${SHARED_TMDB_API_KEY}
      AGAIN: ${PLOP_DATABASE_URL}
      LEVEL: info
      OPTIONAL: ${PLOP_OPTIONAL:-x}
`
	got := placeholderKeys(config)
	want := []string{"PLOP_DATABASE_URL", "PLOP_OPTIONAL", "SHARED_TMDB_API_KEY"}
	if !slices.Equal(got, want) {
		t.Errorf("placeholderKeys = %v, want %v", got, want)
	}

	// Today's known mistakes (B6), pinned so the step-3 fix changes this
	// test on purpose: an escaped $${X} is read as a key, $X is missed.
	got = placeholderKeys(`test: pg_isready -U $${POSTGRES_USER} $BARE`)
	if !slices.Equal(got, []string{"POSTGRES_USER"}) {
		t.Errorf("placeholderKeys(escaped) = %v", got)
	}
}

// writeZip creates a ZIP with the given entries (name → contents; a name
// ending in / is a folder).
func writeZip(t *testing.T, entries map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "a.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, contents := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(contents))
	}
	zw.Close()
	f.Close()
	return path
}

func TestUnpack(t *testing.T) {
	d := &Deployer{stagingPath: t.TempDir()}
	archive := writeZip(t, map[string]string{
		"plop-main/":                   "",
		"plop-main/docker-compose.yml": "services: {}",
		"plop-main/src/main.go":        "package main",
	})
	if err := d.unpack(archive); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(d.stagingPath, "plop-main", "src", "main.go"))
	if err != nil || string(data) != "package main" {
		t.Errorf("unpacked file: %q, %v", data, err)
	}
}

func TestUnpackRefusesZipSlip(t *testing.T) {
	d := &Deployer{stagingPath: t.TempDir()}
	archive := writeZip(t, map[string]string{"../escaped.txt": "x"})
	err := d.unpack(archive)
	if err == nil || !strings.Contains(err.Error(), "outside the staging folder") {
		t.Fatalf("unpack = %v, want a refusal", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(d.stagingPath), "escaped.txt")); err == nil {
		t.Error("the file was written outside the staging folder")
	}
}

func TestCleanUp(t *testing.T) {
	root := t.TempDir()
	d := &Deployer{stagingPath: filepath.Join(root, "staging"), downloadPath: filepath.Join(root, "download")}

	// Missing folders are created.
	if err := d.cleanUp(); err != nil {
		t.Fatal(err)
	}

	os.MkdirAll(filepath.Join(d.stagingPath, "old", "deep"), 0o755)
	os.WriteFile(filepath.Join(d.downloadPath, "old.zip"), []byte("x"), 0o644)
	if err := d.cleanUp(); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{d.stagingPath, d.downloadPath} {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Errorf("%s after cleanUp: %d entries, %v", dir, len(entries), err)
		}
	}
}
