package deploy

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lsariol/lighthouse/internal/compose"
)

// TestRealBackup dumps a real Postgres the way a deploy of sparkdb does. It
// needs a Docker daemon (and pulls postgres:16.4); it's skipped without one
// or with -short.
func TestRealBackup(t *testing.T) {
	if testing.Short() {
		t.Skip("-short")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("no Docker daemon")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	const container = "lighthouse-backup-test"
	exec.Command("docker", "rm", "-f", container).Run()
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", container).Run() })
	if out, err := exec.CommandContext(ctx, "docker", "run", "-d", "--name", container,
		"-e", "POSTGRES_USER=Admin", "-e", "POSTGRES_PASSWORD=backup-test", "-e", "POSTGRES_DB=Admin_DB",
		"postgres:16.4").CombinedOutput(); err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	for i := 0; ; i++ {
		// Ready once the server answers over its socket as the admin user.
		if exec.CommandContext(ctx, "docker", "exec", container, "psql", "-U", "Admin", "-d", "Admin_DB", "-c",
			"CREATE TABLE IF NOT EXISTS marker (id int)").Run() == nil {
			break
		}
		if i > 120 {
			t.Fatal("Postgres didn't start")
		}
		time.Sleep(500 * time.Millisecond)
	}

	path := filepath.Join(t.TempDir(), "dump.sql.gz")
	if err := dump(ctx, compose.Runner{}, container, path); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var sql bytes.Buffer
	io.Copy(&sql, gz)
	for _, want := range []string{"PostgreSQL database cluster dump complete", "CREATE TABLE public.marker", "CREATE ROLE \"Admin\""} {
		if !strings.Contains(sql.String(), want) {
			t.Errorf("the dump lacks %q", want)
		}
	}

	// A failing dump is an error, with what pg_dumpall said.
	err = compose.Runner{}.Exec(ctx, container, []string{"sh", "-c", `pg_dumpall -U nobody`}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "nobody") {
		t.Errorf("a failing dump = %v", err)
	}
}
