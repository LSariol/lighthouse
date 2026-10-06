package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/LSariol/LightHouse/internal/github"
	"github.com/LSariol/LightHouse/internal/projects"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The Database is the projects.Store.
var _ projects.Store = (*Database)(nil)

// Postgres error codes and constraint names the store turns into
// projects errors.
const (
	uniqueViolation = "23505"
	checkViolation  = "23514"
	nameKey         = "projects_name_key"   // unique index on lower(name)
	repoKey         = "projects_repo_key"   // unique index on the repository
	nameCheck       = "projects_name_check" // the name's format
)

// constraintError maps a unique or check violation to a projects error.
func constraintError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch {
	case pgErr.Code == uniqueViolation && pgErr.ConstraintName == nameKey:
		return projects.ErrNameTaken
	case pgErr.Code == uniqueViolation && pgErr.ConstraintName == repoKey:
		return projects.ErrRepoWatched
	case pgErr.Code == checkViolation && pgErr.ConstraintName == nameCheck:
		return projects.ErrInvalidName
	}
	return err
}

const projectColumns = `name, repo_owner, repo_name, created_at, coalesce(deployed_sha, ''), deployed_at,
	last_checked_at, check_count, coalesce(last_error, ''), last_error_at`

func scanProject(row pgx.Row) (projects.Project, error) {
	var p projects.Project
	err := row.Scan(&p.Name, &p.Repo.Owner, &p.Repo.Name, &p.CreatedAt, &p.DeployedSHA, &p.DeployedAt,
		&p.LastCheckedAt, &p.Checks, &p.LastError, &p.LastErrorAt)
	return p, err
}

func (d *Database) List(ctx context.Context) ([]projects.Project, error) {
	rows, err := d.pool.Query(ctx, `SELECT `+projectColumns+` FROM lighthouse.projects ORDER BY lower(name)`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	var list []projects.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("list projects: %w", err)
		}
		list = append(list, p)
	}
	return list, rows.Err()
}

func (d *Database) Get(ctx context.Context, name string) (projects.Project, error) {
	p, err := scanProject(d.pool.QueryRow(ctx,
		`SELECT `+projectColumns+` FROM lighthouse.projects WHERE lower(name) = lower($1)`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return projects.Project{}, projects.ErrNotFound
	}
	if err != nil {
		return projects.Project{}, fmt.Errorf("get project %q: %w", name, err)
	}
	return p, nil
}

func (d *Database) Add(ctx context.Context, name string, repo github.Repo) (projects.Project, error) {
	if err := projects.ValidateName(name); err != nil {
		return projects.Project{}, err
	}
	p, err := scanProject(d.pool.QueryRow(ctx,
		`INSERT INTO lighthouse.projects (name, repo_owner, repo_name) VALUES ($1, $2, $3)
		 RETURNING `+projectColumns, name, repo.Owner, repo.Name))
	if err != nil {
		return projects.Project{}, constraintError(err)
	}
	return p, nil
}

// exec runs a statement that changes one project, and returns ErrNotFound if
// it matched none.
func (d *Database) exec(ctx context.Context, query string, args ...any) error {
	tag, err := d.pool.Exec(ctx, query, args...)
	if err != nil {
		return constraintError(err)
	}
	if tag.RowsAffected() == 0 {
		return projects.ErrNotFound
	}
	return nil
}

func (d *Database) Remove(ctx context.Context, name string) error {
	return d.exec(ctx, `DELETE FROM lighthouse.projects WHERE lower(name) = lower($1)`, name)
}

func (d *Database) Rename(ctx context.Context, name string, newName string) error {
	if err := projects.ValidateName(newName); err != nil {
		return err
	}
	return d.exec(ctx, `UPDATE lighthouse.projects SET name = $2, updated_at = now() WHERE lower(name) = lower($1)`, name, newName)
}

func (d *Database) SetRepo(ctx context.Context, name string, repo github.Repo) error {
	return d.exec(ctx, `UPDATE lighthouse.projects SET repo_owner = $2, repo_name = $3, updated_at = now()
		WHERE lower(name) = lower($1)`, name, repo.Owner, repo.Name)
}

func (d *Database) RecordCheck(ctx context.Context, name string, checkErr error) error {
	return d.exec(ctx, `UPDATE lighthouse.projects
		SET last_checked_at = now(),
		    check_count     = check_count + 1,
		    last_error      = nullif($2, ''),
		    last_error_at   = CASE WHEN $2 = '' THEN NULL ELSE now() END
		WHERE lower(name) = lower($1)`, name, projects.ErrorText(checkErr))
}

func (d *Database) RecordDeployment(ctx context.Context, dep projects.Deployment) error {
	errText := projects.ErrorText(errorString(dep.Error))

	return pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
		var id int64
		err := tx.QueryRow(ctx, `SELECT id FROM lighthouse.projects WHERE lower(name) = lower($1) FOR UPDATE`, dep.Project).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return projects.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("record deployment: %w", err)
		}

		if _, err := tx.Exec(ctx, `INSERT INTO lighthouse.deployments
			(project_id, sha, trigger, status, started_at, finished_at, error)
			VALUES ($1, nullif($2, ''), $3, $4, $5, $6, nullif($7, ''))`,
			id, dep.SHA, dep.Trigger, dep.Status, dep.StartedAt, dep.FinishedAt, errText); err != nil {
			return fmt.Errorf("record deployment: %w", err)
		}

		if dep.Status == projects.StatusSucceeded {
			_, err = tx.Exec(ctx, `UPDATE lighthouse.projects
				SET deployed_sha = $2, deployed_at = $3, last_error = NULL, last_error_at = NULL WHERE id = $1`,
				id, dep.SHA, dep.FinishedAt)
		} else {
			_, err = tx.Exec(ctx, `UPDATE lighthouse.projects SET last_error = nullif($2, ''), last_error_at = $3 WHERE id = $1`,
				id, errText, dep.FinishedAt)
		}
		if err != nil {
			return fmt.Errorf("record deployment: %w", err)
		}
		return nil
	})
}

func (d *Database) History(ctx context.Context, name string, limit int) ([]projects.Deployment, error) {
	p, err := d.Get(ctx, name)
	if err != nil {
		return nil, err
	}

	rows, err := d.pool.Query(ctx, `SELECT coalesce(d.sha, ''), d.trigger, d.status, d.started_at, d.finished_at, coalesce(d.error, '')
		FROM lighthouse.deployments d JOIN lighthouse.projects p ON p.id = d.project_id
		WHERE lower(p.name) = lower($1)
		ORDER BY d.started_at DESC, d.id DESC
		LIMIT $2`, name, limit)
	if err != nil {
		return nil, fmt.Errorf("history of %q: %w", name, err)
	}
	defer rows.Close()

	var history []projects.Deployment
	for rows.Next() {
		dep := projects.Deployment{Project: p.Name}
		if err := rows.Scan(&dep.SHA, &dep.Trigger, &dep.Status, &dep.StartedAt, &dep.FinishedAt, &dep.Error); err != nil {
			return nil, fmt.Errorf("history of %q: %w", name, err)
		}
		history = append(history, dep)
	}
	return history, rows.Err()
}

// Import adds a project with its recorded state, as read from a pre-1.0
// repos.json. It returns projects.ErrNameTaken or ErrRepoWatched if it's
// already there, so importing twice changes nothing.
func (d *Database) Import(ctx context.Context, p projects.Project) error {
	if err := projects.ValidateName(p.Name); err != nil {
		return err
	}
	created := p.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	_, err := d.pool.Exec(ctx, `INSERT INTO lighthouse.projects
		(name, repo_owner, repo_name, created_at, deployed_sha, deployed_at, last_checked_at, check_count, last_error, last_error_at)
		VALUES ($1, $2, $3, $4, nullif($5, ''), $6, $7, $8, nullif($9, ''), $10)`,
		p.Name, p.Repo.Owner, p.Repo.Name, created, p.DeployedSHA, p.DeployedAt,
		p.LastCheckedAt, p.Checks, projects.ErrorText(errorString(p.LastError)), p.LastErrorAt)
	return constraintError(err)
}

// errorString lets a stored message go through projects.ErrorText.
type errorString string

func (e errorString) Error() string { return string(e) }
