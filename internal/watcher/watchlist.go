package watcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/LSariol/LightHouse/internal/models"
)

var (
	ErrNotFound    = errors.New("no such project")
	ErrNameTaken   = errors.New("name already in use")
	ErrURLWatched  = errors.New("repository already watched")
	ErrInvalidName = errors.New("invalid project name")
	ErrInvalidURL  = errors.New("invalid GitHub URL")
)

// validName is what a project name may look like: it's typed in the CLI and
// matched without regard to case.
var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// Load reads the watchlist file.
func (w *Watcher) Load() error {
	data, err := os.ReadFile(w.repoPath)
	if err != nil {
		return err
	}

	var list []models.WatchedRepo
	if err := json.Unmarshal(data, &list); err != nil {
		return fmt.Errorf("%s isn't a valid watchlist: %v", w.repoPath, err)
	}

	w.mu.Lock()
	w.watchList = list
	w.mu.Unlock()
	return nil
}

// store writes the watchlist file.
func (w *Watcher) store() error {
	w.mu.Lock()
	data, err := json.MarshalIndent(w.watchList, "", "\t")
	w.mu.Unlock()
	if err != nil {
		return fmt.Errorf("save watchlist: %w", err)
	}

	if err := os.WriteFile(w.repoPath, data, 0644); err != nil {
		return fmt.Errorf("save watchlist: %w", err)
	}
	return nil
}

// Repos returns a copy of the watchlist.
func (w *Watcher) Repos() []models.WatchedRepo {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]models.WatchedRepo(nil), w.watchList...)
}

// Find returns the project called name (not case-sensitive).
func (w *Watcher) Find(name string) (models.WatchedRepo, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if i := w.index(name); i >= 0 {
		return w.watchList[i], true
	}
	return models.WatchedRepo{}, false
}

// index returns the position of the project called name, or -1. w.mu must
// be held.
func (w *Watcher) index(name string) int {
	for i, repo := range w.watchList {
		if strings.EqualFold(repo.DisplayName, name) {
			return i
		}
	}
	return -1
}

// Add starts watching the repository at url under name.
func (w *Watcher) Add(name string, url string) (models.WatchedRepo, error) {
	if !validName.MatchString(name) {
		return models.WatchedRepo{}, ErrInvalidName
	}
	gh, err := ParseRepoURL(url)
	if err != nil {
		return models.WatchedRepo{}, err
	}

	w.mu.Lock()
	if w.index(name) >= 0 {
		w.mu.Unlock()
		return models.WatchedRepo{}, ErrNameTaken
	}
	if w.watchedBy(gh.URL()) != "" {
		w.mu.Unlock()
		return models.WatchedRepo{}, ErrURLWatched
	}
	repo := models.NewWatchedRepo(name, gh.Repo, gh.URL(), gh.APIURL(), gh.DownloadURL())
	w.watchList = append(w.watchList, repo)
	w.mu.Unlock()

	return repo, w.store()
}

// Remove stops watching the project called name.
func (w *Watcher) Remove(name string) error {
	w.mu.Lock()
	i := w.index(name)
	if i < 0 {
		w.mu.Unlock()
		return ErrNotFound
	}
	w.watchList = append(w.watchList[:i], w.watchList[i+1:]...)
	w.mu.Unlock()

	return w.store()
}

// Rename changes a project's name.
func (w *Watcher) Rename(name string, newName string) error {
	if !validName.MatchString(newName) {
		return ErrInvalidName
	}

	w.mu.Lock()
	i := w.index(name)
	if i < 0 {
		w.mu.Unlock()
		return ErrNotFound
	}
	if j := w.index(newName); j >= 0 && j != i {
		w.mu.Unlock()
		return ErrNameTaken
	}
	w.watchList[i].DisplayName = newName
	touch(&w.watchList[i])
	w.mu.Unlock()

	return w.store()
}

// SetURL points a project at another repository. Its container and folder
// names follow the new repository's name.
func (w *Watcher) SetURL(name string, url string) error {
	gh, err := ParseRepoURL(url)
	if err != nil {
		return err
	}

	w.mu.Lock()
	i := w.index(name)
	if i < 0 {
		w.mu.Unlock()
		return ErrNotFound
	}
	if other := w.watchedBy(gh.URL()); other != "" && !strings.EqualFold(other, name) {
		w.mu.Unlock()
		return ErrURLWatched
	}
	r := &w.watchList[i]
	r.URL, r.APIURL, r.DownloadURL, r.ContainerName = gh.URL(), gh.APIURL(), gh.DownloadURL(), gh.Repo
	touch(r)
	w.mu.Unlock()

	return w.store()
}

// watchedBy returns the name of the project watching url, or "". w.mu must
// be held.
func (w *Watcher) watchedBy(url string) string {
	for _, repo := range w.watchList {
		if strings.EqualFold(repo.URL, url) {
			return repo.DisplayName
		}
	}
	return ""
}

func touch(r *models.WatchedRepo) {
	now := time.Now()
	r.Stats.Meta.LastModifiedAt = &now
}

// GitHubRepo is a repository on GitHub.
type GitHubRepo struct {
	Owner string
	Repo  string
}

func (g GitHubRepo) URL() string    { return "https://github.com/" + g.Owner + "/" + g.Repo }
func (g GitHubRepo) APIURL() string { return "https://api.github.com/repos/" + g.Owner + "/" + g.Repo }
func (g GitHubRepo) DownloadURL() string {
	return g.URL() + "/archive/refs/heads/main.zip"
}

var repoPart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// ParseRepoURL accepts a GitHub repository URL in its common forms:
// https://github.com/owner/repo, with or without "www.", a trailing slash or
// ".git", or http://.
func ParseRepoURL(raw string) (GitHubRepo, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "www.")
	if !strings.HasPrefix(strings.ToLower(s), "github.com/") {
		return GitHubRepo{}, ErrInvalidURL
	}
	s = s[len("github.com/"):]
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")

	parts := strings.Split(s, "/")
	if len(parts) != 2 || !repoPart.MatchString(parts[0]) || !repoPart.MatchString(parts[1]) {
		return GitHubRepo{}, ErrInvalidURL
	}
	return GitHubRepo{Owner: parts[0], Repo: parts[1]}, nil
}
