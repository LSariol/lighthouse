package database

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lsariol/lighthouse/internal/github"
	"github.com/lsariol/lighthouse/internal/projects"
	"github.com/lsariol/lighthouse/internal/projects/projectstest"
)

// The integration tests run against a real, disposable Postgres set up with
// scripts/db/setup.sql (scripts/test-db.sh does it; CI too). Without these
// variables they're skipped.
var (
	migratorURL = os.Getenv("LIGHTHOUSE_TEST_MIGRATOR_DATABASE_URL")
	appURL      = os.Getenv("LIGHTHOUSE_TEST_DATABASE_URL")
	readerURL   = os.Getenv("LIGHTHOUSE_TEST_READER_DATABASE_URL")
)

var migrateOnce sync.Once

// open migrates once, connects as the app role and empties the tables.
func open(t *testing.T) *Database {
	t.Helper()
	if migratorURL == "" || appURL == "" {
		t.Skip("LIGHTHOUSE_TEST_MIGRATOR_DATABASE_URL and LIGHTHOUSE_TEST_DATABASE_URL aren't set (see scripts/test-db.sh)")
	}
	ctx := context.Background()

	var migrateErr error
	migrateOnce.Do(func() { migrateErr = Migrate(ctx, migratorURL) })
	if migrateErr != nil {
		t.Fatal(migrateErr)
	}

	db, err := Connect(ctx, appURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)

	if _, err := db.pool.Exec(ctx, `DELETE FROM lighthouse.projects`); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestStore(t *testing.T) {
	projectstest.RunStoreTests(t, func(t *testing.T) projects.Store { return open(t) })
}

func TestMigrationsAreRepeatable(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	if err := Migrate(ctx, migratorURL); err != nil {
		t.Fatalf("a second Migrate: %v", err)
	}
	if err := db.CheckSchemaVersion(ctx); err != nil {
		t.Fatal(err)
	}
	have, want, _ := db.SchemaVersion(ctx)
	if have != want || want < 3 {
		t.Errorf("schema version %d, want %d", have, want)
	}
}

// The grants in 00002: the app may change projects but only add history;
// it can't change the schema; everything is owned by lighthouse_owner.
func TestGrants(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	if _, err := db.Add(ctx, "plop", github.Repo{Owner: "o", Name: "plop"}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordDeployment(ctx, projects.Deployment{Project: "plop", Trigger: projects.TriggerCheck,
		Status: projects.StatusFailed, Steps: []projects.Step{{Name: "fetch", Status: projects.StepFailed}}}); err != nil {
		t.Fatalf("the app can't add history: %v", err)
	}

	denied := []string{
		`UPDATE lighthouse.deployments SET error = 'rewritten'`,
		`DELETE FROM lighthouse.deployments`,
		`UPDATE lighthouse.deployment_steps SET log = 'rewritten'`,
		`DELETE FROM lighthouse.deployment_steps`,
		`CREATE TABLE lighthouse.sneaky (x int)`,
		`CREATE TABLE public.sneaky (x int)`,
		`DELETE FROM lighthouse.goose_db_version`,
	}
	for _, stmt := range denied {
		if _, err := db.pool.Exec(ctx, stmt); !isPermissionDenied(err) {
			t.Errorf("the app ran %q: %v", stmt, err)
		}
	}

	var owners []string
	rows, _ := db.pool.Query(ctx, `SELECT DISTINCT tableowner FROM pg_tables WHERE schemaname = 'lighthouse'`)
	owners, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(owners) != 1 || owners[0] != "lighthouse_owner" {
		t.Errorf("table owners = %v, %v; want only lighthouse_owner", owners, err)
	}

	if readerURL == "" {
		return
	}
	reader, err := Connect(ctx, readerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := reader.List(ctx); err != nil {
		t.Errorf("the reader can't read: %v", err)
	}
	if _, err := reader.Add(ctx, "x", github.Repo{Owner: "o", Name: "x"}); !isPermissionDenied(err) {
		t.Errorf("the reader could write: %v", err)
	}
}

func isPermissionDenied(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42501"
}

func TestImport(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	p := projects.Project{Name: "cove", Repo: github.Repo{Owner: "LSariol", Name: "Cove"}, DeployedSHA: "abc", Checks: 119}
	if err := db.Import(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := db.Import(ctx, p); !errors.Is(err, projects.ErrNameTaken) {
		t.Errorf("a second import = %v, want ErrNameTaken", err)
	}
	got, _ := db.Get(ctx, "cove")
	if got.DeployedSHA != "abc" || got.Checks != 119 || got.CreatedAt.IsZero() {
		t.Errorf("imported %+v", got)
	}
}

func TestUnreachable(t *testing.T) {
	const nowhere = "postgres://x:y@127.0.0.1:1/db?connect_timeout=2"
	_, err := Connect(context.Background(), nowhere)
	if err == nil || !IsUnreachable(err) || !strings.Contains(err.Error(), "LIGHTHOUSE_DATABASE_URL") {
		t.Errorf("Connect to nothing = %v (unreachable: %v)", err, IsUnreachable(err))
	}
	if err := Migrate(context.Background(), nowhere); err == nil || !IsUnreachable(err) {
		t.Errorf("Migrate against nothing = %v (unreachable: %v)", err, IsUnreachable(err))
	}
	if appURL == "" {
		return
	}
	wrong := strings.Replace(appURL, "lighthouse_app:", "lighthouse_app:wrong", 1)
	if _, err := Connect(context.Background(), wrong); err == nil || IsUnreachable(err) {
		t.Errorf("Connect with a wrong password = %v (unreachable: %v)", err, IsUnreachable(err))
	}
}
