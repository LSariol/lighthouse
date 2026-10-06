// Package projectstest has an in-memory projects.Store for tests, and the
// tests every Store must pass (RunStoreTests), so the in-memory one and the
// Postgres one behave the same.
package projectstest

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/LSariol/LightHouse/internal/github"
	"github.com/LSariol/LightHouse/internal/projects"
)

// Store is an in-memory projects.Store. The zero value is ready to use.
type Store struct {
	mu          sync.Mutex
	projects    []projects.Project
	deployments []projects.Deployment
}

var _ projects.Store = (*Store)(nil)

func (s *Store) index(name string) int {
	for i, p := range s.projects {
		if strings.EqualFold(p.Name, name) {
			return i
		}
	}
	return -1
}

func (s *Store) repoIndex(repo github.Repo) int {
	for i, p := range s.projects {
		if strings.EqualFold(p.Repo.Owner, repo.Owner) && strings.EqualFold(p.Repo.Name, repo.Name) {
			return i
		}
	}
	return -1
}

func (s *Store) List(ctx context.Context) ([]projects.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := append([]projects.Project(nil), s.projects...)
	sort.Slice(list, func(i, j int) bool { return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name) })
	return list, nil
}

func (s *Store) Get(ctx context.Context, name string) (projects.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := s.index(name); i >= 0 {
		return s.projects[i], nil
	}
	return projects.Project{}, projects.ErrNotFound
}

func (s *Store) Add(ctx context.Context, name string, repo github.Repo) (projects.Project, error) {
	if err := projects.ValidateName(name); err != nil {
		return projects.Project{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.index(name) >= 0 {
		return projects.Project{}, projects.ErrNameTaken
	}
	if s.repoIndex(repo) >= 0 {
		return projects.Project{}, projects.ErrRepoWatched
	}
	p := projects.Project{Name: name, Repo: repo, CreatedAt: time.Now()}
	s.projects = append(s.projects, p)
	return p, nil
}

func (s *Store) Remove(ctx context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(name)
	if i < 0 {
		return projects.ErrNotFound
	}
	removed := s.projects[i].Name
	s.projects = append(s.projects[:i], s.projects[i+1:]...)

	// Its history goes with it.
	kept := s.deployments[:0]
	for _, d := range s.deployments {
		if d.Project != removed {
			kept = append(kept, d)
		}
	}
	s.deployments = kept
	return nil
}

func (s *Store) Rename(ctx context.Context, name string, newName string) error {
	if err := projects.ValidateName(newName); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(name)
	if i < 0 {
		return projects.ErrNotFound
	}
	if j := s.index(newName); j >= 0 && j != i {
		return projects.ErrNameTaken
	}
	old := s.projects[i].Name
	s.projects[i].Name = newName
	for k := range s.deployments {
		if s.deployments[k].Project == old {
			s.deployments[k].Project = newName
		}
	}
	return nil
}

func (s *Store) SetRepo(ctx context.Context, name string, repo github.Repo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(name)
	if i < 0 {
		return projects.ErrNotFound
	}
	if j := s.repoIndex(repo); j >= 0 && j != i {
		return projects.ErrRepoWatched
	}
	s.projects[i].Repo = repo
	return nil
}

func (s *Store) RecordCheck(ctx context.Context, name string, checkErr error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(name)
	if i < 0 {
		return projects.ErrNotFound
	}
	p := &s.projects[i]
	now := time.Now()
	p.LastCheckedAt = &now
	p.Checks++
	setError(p, checkErr, now)
	return nil
}

func (s *Store) RecordDeployment(ctx context.Context, d projects.Deployment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(d.Project)
	if i < 0 {
		return projects.ErrNotFound
	}
	p := &s.projects[i]
	d.Project = p.Name
	d.Error = projects.ErrorText(errorString(d.Error))
	s.deployments = append(s.deployments, d)

	if d.Status == projects.StatusSucceeded {
		finished := d.FinishedAt
		p.DeployedSHA = d.SHA
		p.DeployedAt = &finished
		p.LastError, p.LastErrorAt = "", nil
	} else {
		finished := d.FinishedAt
		p.LastError, p.LastErrorAt = d.Error, &finished
	}
	return nil
}

func (s *Store) History(ctx context.Context, name string, limit int) ([]projects.Deployment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(name)
	if i < 0 {
		return nil, projects.ErrNotFound
	}
	var history []projects.Deployment
	for k := len(s.deployments) - 1; k >= 0 && len(history) < limit; k-- {
		if s.deployments[k].Project == s.projects[i].Name {
			history = append(history, s.deployments[k])
		}
	}
	return history, nil
}

func setError(p *projects.Project, err error, at time.Time) {
	if err == nil {
		p.LastError, p.LastErrorAt = "", nil
		return
	}
	p.LastError, p.LastErrorAt = projects.ErrorText(err), &at
}

type errorString string

func (e errorString) Error() string { return string(e) }
