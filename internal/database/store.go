package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lsariol/lighthouse/internal/github"
	"github.com/lsariol/lighthouse/internal/projects"
	"github.com/lsariol/lighthouse/internal/release"
)

// The Database is the projects.Store.
var _ projects.Store = (*Database)(nil)

// Postgres error codes and constraint names the store turns into
// projects errors.
const (
	uniqueViolation = "23505"
	checkViolation  = "23514"
	nameKey         = "projects_name_key"            // unique index on lower(name)
	repoKey         = "projects_repo_key"            // unique index on the repository
	composeKey      = "projects_compose_project_key" // unique index on compose_project
	nameCheck       = "projects_name_check"          // the name's format
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
	case pgErr.Code == uniqueViolation && pgErr.ConstraintName == composeKey:
		return projects.ErrComposeProjectTaken
	case pgErr.Code == checkViolation && pgErr.ConstraintName == nameCheck:
		return projects.ErrInvalidName
	}
	return err
}

const projectColumns = `name, repo_owner, repo_name, created_at, coalesce(compose_project, ''),
	coalesce(deployed_sha, ''), coalesce(deployed_version, ''), coalesce(highest_version, ''), deployed_at, last_checked_at, check_count, coalesce(last_error, ''), last_error_at,
	failure_count, coalesce(failing_sha, ''), broken, deploy_mode, tier, stopped, coalesce(held_sha, '')`

func scanProject(row pgx.Row) (projects.Project, error) {
	var p projects.Project
	err := row.Scan(&p.Name, &p.Repo.Owner, &p.Repo.Name, &p.CreatedAt, &p.ComposeProject,
		&p.DeployedSHA, &p.DeployedVersion, &p.HighestVersion, &p.DeployedAt, &p.LastCheckedAt, &p.Checks, &p.LastError, &p.LastErrorAt,
		&p.FailureCount, &p.FailingSHA, &p.Broken, &p.Mode, &p.Tier, &p.Stopped, &p.HeldSHA)
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

func (d *Database) SetComposeProject(ctx context.Context, name string, composeProject string) error {
	return d.exec(ctx, `UPDATE lighthouse.projects SET compose_project = $2 WHERE lower(name) = lower($1)`, name, composeProject)
}

func (d *Database) SetSettings(ctx context.Context, name string, mode string, tier string) error {
	return d.exec(ctx, `UPDATE lighthouse.projects SET deploy_mode = $2, tier = $3 WHERE lower(name) = lower($1)`, name, mode, tier)
}

func (d *Database) SetHeld(ctx context.Context, name string, sha string) error {
	return d.exec(ctx, `UPDATE lighthouse.projects SET held_sha = nullif($2, '') WHERE lower(name) = lower($1)`, name, sha)
}

func (d *Database) SetStopped(ctx context.Context, name string, stopped bool) error {
	return d.exec(ctx, `UPDATE lighthouse.projects SET stopped = $2, updated_at = now() WHERE lower(name) = lower($1)`, name, stopped)
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
	errText := projects.ErrorText(projects.ErrorString(dep.Error))

	return pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
		var id int64
		var highest string
		err := tx.QueryRow(ctx, `SELECT id, coalesce(highest_version, '') FROM lighthouse.projects WHERE lower(name) = lower($1) FOR UPDATE`,
			dep.Project).Scan(&id, &highest)
		if errors.Is(err, pgx.ErrNoRows) {
			return projects.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("record deployment: %w", err)
		}

		var depID int64
		err = tx.QueryRow(ctx, `INSERT INTO lighthouse.deployments
			(project_id, sha, version, trigger, status, failure_kind, failed_step, started_at, finished_at, error)
			VALUES ($1, nullif($2, ''), nullif($3, ''), $4, $5, nullif($6, ''), nullif($7, ''), $8, $9, nullif($10, ''))
			RETURNING id`,
			id, dep.SHA, dep.Version, dep.Trigger, dep.Status, dep.FailureKind, dep.FailedStep, dep.StartedAt, dep.FinishedAt, errText).Scan(&depID)
		if err != nil {
			return fmt.Errorf("record deployment: %w", err)
		}

		for i, st := range dep.Steps {
			if _, err := tx.Exec(ctx, `INSERT INTO lighthouse.deployment_steps
				(deployment_id, position, step, status, started_at, finished_at, log)
				VALUES ($1, $2, $3, $4, $5, $6, nullif($7, ''))`,
				depID, i+1, st.Name, st.Status, st.StartedAt, st.FinishedAt, projects.LogText(st.Log)); err != nil {
				return fmt.Errorf("record deployment step %s: %w", st.Name, err)
			}
		}

		switch {
		case dep.Status == projects.StatusSucceeded:
			_, err = tx.Exec(ctx, `UPDATE lighthouse.projects
				SET deployed_sha = $2, deployed_version = nullif($4, ''), highest_version = nullif($5, ''), deployed_at = $3,
				    last_error = NULL, last_error_at = NULL, failure_count = 0, failing_sha = NULL, broken = false
				WHERE id = $1`, id, dep.SHA, dep.FinishedAt, dep.Version, release.Higher(highest, dep.Version))
		case dep.FailureKind == projects.FailurePermanent:
			// The count restarts with a new commit; at BrokenAfter the
			// project is broken.
			_, err = tx.Exec(ctx, `UPDATE lighthouse.projects
				SET last_error = nullif($2, ''), last_error_at = $3,
				    failure_count = CASE WHEN failing_sha IS NOT DISTINCT FROM nullif($4, '') THEN failure_count + 1 ELSE 1 END,
				    failing_sha   = nullif($4, ''),
				    broken        = (CASE WHEN failing_sha IS NOT DISTINCT FROM nullif($4, '') THEN failure_count + 1 ELSE 1 END) >= $5
				WHERE id = $1`, id, errText, dep.FinishedAt, dep.SHA, projects.BrokenAfter)
		default:
			_, err = tx.Exec(ctx, `UPDATE lighthouse.projects SET last_error = nullif($2, ''), last_error_at = $3 WHERE id = $1`,
				id, errText, dep.FinishedAt)
		}
		if err != nil {
			return fmt.Errorf("record deployment: %w", err)
		}
		return nil
	})
}

func (d *Database) ClearFailures(ctx context.Context, name string) error {
	return d.exec(ctx, `UPDATE lighthouse.projects SET failure_count = 0, failing_sha = NULL, broken = false
		WHERE lower(name) = lower($1)`, name)
}

const deploymentColumns = `d.id, coalesce(d.sha, ''), coalesce(d.version, ''), d.trigger, d.status, coalesce(d.failure_kind, ''), coalesce(d.failed_step, ''),
	d.started_at, d.finished_at, coalesce(d.error, '')`

// deployments returns the project's deployments, newest first, with their
// IDs.
func (d *Database) deployments(ctx context.Context, p projects.Project, limit int, offset int) ([]projects.Deployment, []int64, error) {
	rows, err := d.pool.Query(ctx, `SELECT `+deploymentColumns+`
		FROM lighthouse.deployments d JOIN lighthouse.projects p ON p.id = d.project_id
		WHERE lower(p.name) = lower($1)
		ORDER BY d.started_at DESC, d.id DESC
		LIMIT $2 OFFSET $3`, p.Name, limit, offset)
	if err != nil {
		return nil, nil, fmt.Errorf("history of %q: %w", p.Name, err)
	}
	defer rows.Close()

	var list []projects.Deployment
	var ids []int64
	for rows.Next() {
		dep := projects.Deployment{Project: p.Name}
		var id int64
		if err := rows.Scan(&id, &dep.SHA, &dep.Version, &dep.Trigger, &dep.Status, &dep.FailureKind, &dep.FailedStep,
			&dep.StartedAt, &dep.FinishedAt, &dep.Error); err != nil {
			return nil, nil, fmt.Errorf("history of %q: %w", p.Name, err)
		}
		list = append(list, dep)
		ids = append(ids, id)
	}
	return list, ids, rows.Err()
}

func (d *Database) History(ctx context.Context, name string, limit int) ([]projects.Deployment, error) {
	p, err := d.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	list, _, err := d.deployments(ctx, p, limit, 0)
	return list, err
}

func (d *Database) Deployment(ctx context.Context, name string, n int) (projects.Deployment, error) {
	p, err := d.Get(ctx, name)
	if err != nil {
		return projects.Deployment{}, err
	}
	if n < 1 {
		return projects.Deployment{}, projects.ErrNoDeployment
	}
	list, ids, err := d.deployments(ctx, p, 1, n-1)
	if err != nil {
		return projects.Deployment{}, err
	}
	if len(list) == 0 {
		return projects.Deployment{}, projects.ErrNoDeployment
	}
	dep := list[0]

	rows, err := d.pool.Query(ctx, `SELECT step, status, started_at, finished_at, coalesce(log, '')
		FROM lighthouse.deployment_steps WHERE deployment_id = $1 ORDER BY position`, ids[0])
	if err != nil {
		return projects.Deployment{}, fmt.Errorf("steps of a deployment of %q: %w", p.Name, err)
	}
	defer rows.Close()
	for rows.Next() {
		var st projects.Step
		if err := rows.Scan(&st.Name, &st.Status, &st.StartedAt, &st.FinishedAt, &st.Log); err != nil {
			return projects.Deployment{}, fmt.Errorf("steps of a deployment of %q: %w", p.Name, err)
		}
		dep.Steps = append(dep.Steps, st)
	}
	return dep, rows.Err()
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
		p.LastCheckedAt, p.Checks, projects.ErrorText(projects.ErrorString(p.LastError)), p.LastErrorAt)
	return constraintError(err)
}
