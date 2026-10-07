package database

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // the "pgx" driver for database/sql, which goose uses
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var embeddedMigrations embed.FS

// migrationsTable is where goose records applied migrations. It lives in the
// lighthouse schema, so everything Lighthouse owns stays in one place.
const migrationsTable = "lighthouse.goose_db_version"

// undefinedTable is Postgres's error code for a missing table.
const undefinedTable = "42P01"

// Migrate applies every pending migration. connString must log in as
// lighthouse_migrator (LIGHTHOUSE_MIGRATOR_DATABASE_URL), which acts as
// lighthouse_owner. A Postgres lock stops two Lighthouses migrating at once.
func Migrate(ctx context.Context, connString string) error {
	provider, db, err := newProvider(ctx, connString)
	if err != nil {
		return err
	}
	defer db.Close()

	results, err := provider.Up(ctx)
	for _, r := range results {
		slog.Info("migration applied", "migration", r.Source.Path, "took", r.Duration.String())
	}
	if err != nil {
		return fmt.Errorf("database: apply migrations: %w", err)
	}
	if len(results) == 0 {
		slog.Info("database schema is up to date")
	}
	return nil
}

// PrintMigrationStatus writes each migration and whether it has been applied.
func PrintMigrationStatus(ctx context.Context, connString string, w io.Writer) error {
	provider, db, err := newProvider(ctx, connString)
	if err != nil {
		return err
	}
	defer db.Close()

	statuses, err := provider.Status(ctx)
	if err != nil {
		return fmt.Errorf("database: migration status: %w", err)
	}
	for _, s := range statuses {
		appliedAt := "-"
		if !s.AppliedAt.IsZero() {
			appliedAt = s.AppliedAt.Local().Format("2006-01-02 15:04:05")
		}
		fmt.Fprintf(w, "%-8s %-19s %s\n", s.State, appliedAt, s.Source.Path)
	}
	return nil
}

func newProvider(ctx context.Context, connString string) (*goose.Provider, *sql.DB, error) {
	db, err := sql.Open("pgx", connString)
	if err != nil {
		return nil, nil, fmt.Errorf("LIGHTHOUSE_MIGRATOR_DATABASE_URL isn't a valid connection string: %w", err)
	}

	// goose creates its version table before running any migration, so the
	// schema that holds it has to exist first.
	if _, err := db.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS lighthouse"); err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("database: prepare migrations with LIGHTHOUSE_MIGRATOR_DATABASE_URL: %w", err)
	}

	fsys, err := fs.Sub(embeddedMigrations, "migrations")
	if err != nil {
		db.Close()
		return nil, nil, err
	}

	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		db.Close()
		return nil, nil, err
	}

	provider, err := goose.NewProvider(goose.DialectPostgres, db, fsys,
		goose.WithTableName(migrationsTable),
		goose.WithSessionLocker(locker),
	)
	if err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("database: prepare migrations: %w", err)
	}
	return provider, db, nil
}

// SchemaVersion returns the database's migration version and the version this
// build needs.
func (d *Database) SchemaVersion(ctx context.Context) (have int64, want int64, err error) {
	want, err = latestMigration()
	if err != nil {
		return 0, 0, err
	}

	const query = `SELECT COALESCE(MAX(version_id), 0) FROM lighthouse.goose_db_version WHERE is_applied`
	if err := d.pool.QueryRow(ctx, query).Scan(&have); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == undefinedTable {
			return 0, want, nil
		}
		return 0, want, fmt.Errorf("database: read the schema version: %w", err)
	}
	return have, want, nil
}

// CheckSchemaVersion refuses a database missing a migration this build needs,
// so Lighthouse stops at startup instead of failing on its first query. A
// newer database is fine: rolling back the code after an additive migration
// still works.
func (d *Database) CheckSchemaVersion(ctx context.Context) error {
	have, want, err := d.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	if have < want {
		return fmt.Errorf("the database schema is at version %d, but this Lighthouse needs %d. Check that LIGHTHOUSE_MIGRATOR_DATABASE_URL in Cove logs in as lighthouse_migrator, or run \"lighthouse migrate up\"", have, want)
	}
	return nil
}

func latestMigration() (int64, error) {
	names, err := fs.Glob(embeddedMigrations, "migrations/*.sql")
	if err != nil {
		return 0, err
	}
	var latest int64
	for _, name := range names {
		v, err := goose.NumericComponent(name)
		if err != nil {
			return 0, fmt.Errorf("migration %q: %w", name, err)
		}
		latest = max(latest, v)
	}
	return latest, nil
}
