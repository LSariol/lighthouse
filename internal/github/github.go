// Package github is what Lighthouse needs from GitHub: parsing repository
// URLs and finding a repository's latest commit.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// ErrInvalidURL means a string isn't a GitHub repository URL.
var ErrInvalidURL = errors.New("invalid GitHub repository URL")

// Repo is a repository on GitHub.
type Repo struct {
	Owner string
	Name  string
}

func (r Repo) URL() string    { return "https://github.com/" + r.Owner + "/" + r.Name }
func (r Repo) APIURL() string { return "https://api.github.com/repos/" + r.Owner + "/" + r.Name }

// ArchiveURL is the ZIP of the repository's main branch.
func (r Repo) ArchiveURL() string { return r.URL() + "/archive/refs/heads/main.zip" }

var namePart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// ParseRepoURL accepts a GitHub repository URL in its common forms:
// https://github.com/owner/repo, with or without "www.", a trailing slash or
// ".git", or http://.
func ParseRepoURL(raw string) (Repo, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "www.")
	if !strings.HasPrefix(strings.ToLower(s), "github.com/") {
		return Repo{}, ErrInvalidURL
	}
	s = s[len("github.com/"):]
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")

	parts := strings.Split(s, "/")
	if len(parts) != 2 || !namePart.MatchString(parts[0]) || !namePart.MatchString(parts[1]) {
		return Repo{}, ErrInvalidURL
	}
	return Repo{Owner: parts[0], Name: parts[1]}, nil
}

// Client calls the GitHub REST API.
type Client struct {
	HTTP *http.Client
}

// LatestCommit returns the newest commit on the default branch of the
// repository whose API URL is apiURL (see Repo.APIURL).
func (c Client) LatestCommit(ctx context.Context, apiURL string, token string) (string, error) {
	if token == "" {
		return "", errors.New("no GitHub token yet (Lighthouse is still waiting for Cove)")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"/commits?per_page=1", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("GitHub: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub: %s", resp.Status)
	}

	var commits []struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&commits); err != nil {
		return "", fmt.Errorf("GitHub: unexpected response: %v", err)
	}
	if len(commits) == 0 || commits[0].SHA == "" {
		return "", errors.New("GitHub: the repository has no commits")
	}
	return commits[0].SHA, nil
}
