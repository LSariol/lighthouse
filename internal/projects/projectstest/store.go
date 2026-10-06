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
	deployments []projects.Deployment // oldest first
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

func (s *Store) SetComposeProject(ctx context.Context, name string, composeProject string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(name)
	if i < 0 {
		return projects.ErrNotFound
	}
	for j, p := range s.projects {
		if j != i && p.ComposeProject == composeProject {
			return projects.ErrComposeProjectTaken
		}
	}
	s.projects[i].ComposeProject = composeProject
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
	if checkErr == nil {
		p.LastError, p.LastErrorAt = "", nil
	} else {
		p.LastError, p.LastErrorAt = projects.ErrorText(checkErr), &now
	}
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
	d.Error = projects.ErrorText(projects.ErrorString(d.Error))
	steps := make([]projects.Step, len(d.Steps))
	for k, st := range d.Steps {
		st.Log = projects.LogText(st.Log)
		steps[k] = st
	}
	d.Steps = steps
	s.deployments = append(s.deployments, d)

	finished := d.FinishedAt
	if d.Status == projects.StatusSucceeded {
		p.DeployedSHA, p.DeployedAt = d.SHA, &finished
		p.LastError, p.LastErrorAt = "", nil
		p.FailureCount, p.FailingSHA, p.Broken = 0, "", false
		return nil
	}

	p.LastError, p.LastErrorAt = d.Error, &finished
	if d.FailureKind == projects.FailurePermanent {
		if p.FailingSHA == d.SHA {
			p.FailureCount++
		} else {
			p.FailureCount, p.FailingSHA = 1, d.SHA
		}
		p.Broken = p.FailureCount >= projects.BrokenAfter
	}
	return nil
}

func (s *Store) ClearFailures(ctx context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(name)
	if i < 0 {
		return projects.ErrNotFound
	}
	p := &s.projects[i]
	p.FailureCount, p.FailingSHA, p.Broken = 0, "", false
	return nil
}

// newest returns the project's deployments, newest first. s.mu must be held.
func (s *Store) newest(project string) []projects.Deployment {
	var list []projects.Deployment
	for k := len(s.deployments) - 1; k >= 0; k-- {
		if s.deployments[k].Project == project {
			list = append(list, s.deployments[k])
		}
	}
	sort.SliceStable(list, func(a, b int) bool { return list[a].StartedAt.After(list[b].StartedAt) })
	return list
}

func (s *Store) History(ctx context.Context, name string, limit int) ([]projects.Deployment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(name)
	if i < 0 {
		return nil, projects.ErrNotFound
	}
	list := s.newest(s.projects[i].Name)
	if len(list) > limit {
		list = list[:limit]
	}
	history := make([]projects.Deployment, len(list))
	for k, d := range list {
		d.Steps = nil
		history[k] = d
	}
	return history, nil
}

func (s *Store) Deployment(ctx context.Context, name string, n int) (projects.Deployment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(name)
	if i < 0 {
		return projects.Deployment{}, projects.ErrNotFound
	}
	list := s.newest(s.projects[i].Name)
	if n < 1 || n > len(list) {
		return projects.Deployment{}, projects.ErrNoDeployment
	}
	return list[n-1], nil
}
