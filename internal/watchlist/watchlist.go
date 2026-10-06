// Package watchlist holds the watched projects and what Lighthouse knows
// about each: its repository, deployed commit and last check. It's stored as
// a JSON file (repos.json) until v1.0.0 moves it into Postgres.
//
// A List is safe to use from several goroutines. Its lock is never held while
// anything slow happens (network calls, deploys).
package watchlist

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/LSariol/LightHouse/internal/github"
)

var (
	ErrNotFound    = errors.New("no such project")
	ErrNameTaken   = errors.New("name already in use")
	ErrURLWatched  = errors.New("repository already watched")
	ErrInvalidName = errors.New("invalid project name")
)

// validName is what a project name may look like: it's typed in the CLI and
// matched without regard to case.
var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// List is the watchlist, backed by a JSON file.
type List struct {
	path string

	mu       sync.Mutex
	projects []Project
}

// Load reads the watchlist file at path.
func Load(path string) (*List, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var projects []Project
	if err := json.Unmarshal(data, &projects); err != nil {
		return nil, fmt.Errorf("%s isn't a valid watchlist: %v", path, err)
	}
	return &List{path: path, projects: projects}, nil
}

// Save writes the watchlist file.
func (l *List) Save() error {
	l.mu.Lock()
	data, err := json.MarshalIndent(l.projects, "", "\t")
	l.mu.Unlock()
	if err != nil {
		return fmt.Errorf("save watchlist: %w", err)
	}

	// Rewritten in place: the file is a single-file bind mount in Docker, which
	// can't be replaced by a rename.
	if err := os.WriteFile(l.path, data, 0o644); err != nil {
		return fmt.Errorf("save watchlist: %w", err)
	}
	return nil
}

// All returns a copy of every project.
func (l *List) All() []Project {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Project(nil), l.projects...)
}

// Find returns the project called name (not case-sensitive).
func (l *List) Find(name string) (Project, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if i := l.index(name); i >= 0 {
		return l.projects[i], true
	}
	return Project{}, false
}

// Update applies fn to the project called name and reports whether it's
// still watched (it may have been removed while a scan was running). It
// doesn't save; call Save.
func (l *List) Update(name string, fn func(*Project)) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	i := l.index(name)
	if i < 0 {
		return false
	}
	fn(&l.projects[i])
	return true
}

// Add starts watching the repository at url under name, and saves.
func (l *List) Add(name string, url string) (Project, error) {
	if !validName.MatchString(name) {
		return Project{}, ErrInvalidName
	}
	repo, err := github.ParseRepoURL(url)
	if err != nil {
		return Project{}, err
	}

	l.mu.Lock()
	if l.index(name) >= 0 {
		l.mu.Unlock()
		return Project{}, ErrNameTaken
	}
	if l.watchedBy(repo.URL()) != "" {
		l.mu.Unlock()
		return Project{}, ErrURLWatched
	}
	p := newProject(name, repo)
	l.projects = append(l.projects, p)
	l.mu.Unlock()

	return p, l.Save()
}

// Remove stops watching the project called name, and saves.
func (l *List) Remove(name string) error {
	l.mu.Lock()
	i := l.index(name)
	if i < 0 {
		l.mu.Unlock()
		return ErrNotFound
	}
	l.projects = append(l.projects[:i], l.projects[i+1:]...)
	l.mu.Unlock()

	return l.Save()
}

// Rename changes a project's name, and saves.
func (l *List) Rename(name string, newName string) error {
	if !validName.MatchString(newName) {
		return ErrInvalidName
	}

	l.mu.Lock()
	i := l.index(name)
	if i < 0 {
		l.mu.Unlock()
		return ErrNotFound
	}
	if j := l.index(newName); j >= 0 && j != i {
		l.mu.Unlock()
		return ErrNameTaken
	}
	l.projects[i].Name = newName
	l.projects[i].touch()
	l.mu.Unlock()

	return l.Save()
}

// SetURL points a project at another repository, and saves. Its container and
// folder names follow the new repository's name.
func (l *List) SetURL(name string, url string) error {
	repo, err := github.ParseRepoURL(url)
	if err != nil {
		return err
	}

	l.mu.Lock()
	i := l.index(name)
	if i < 0 {
		l.mu.Unlock()
		return ErrNotFound
	}
	if other := l.watchedBy(repo.URL()); other != "" && !strings.EqualFold(other, name) {
		l.mu.Unlock()
		return ErrURLWatched
	}
	l.projects[i].setRepo(repo)
	l.projects[i].touch()
	l.mu.Unlock()

	return l.Save()
}

// index returns the position of the project called name, or -1. l.mu must
// be held.
func (l *List) index(name string) int {
	for i, p := range l.projects {
		if strings.EqualFold(p.Name, name) {
			return i
		}
	}
	return -1
}

// watchedBy returns the name of the project watching url, or "". l.mu must
// be held.
func (l *List) watchedBy(url string) string {
	for _, p := range l.projects {
		if strings.EqualFold(p.URL, url) {
			return p.Name
		}
	}
	return ""
}

func now() *time.Time {
	t := time.Now()
	return &t
}
