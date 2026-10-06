// Package github is what Lighthouse needs from GitHub: parsing repository
// URLs, finding a repository's latest commit, and downloading a commit.
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
func (r Repo) String() string { return r.Owner + "/" + r.Name }

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

// DefaultAPI is GitHub's REST API.
const DefaultAPI = "https://api.github.com"

// Client calls the GitHub REST API. The zero value uses http.DefaultClient
// and DefaultAPI; set HTTP for timeouts.
type Client struct {
	HTTP *http.Client
	API  string // the API's base URL; tests point it at a fake
}

func (c Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c Client) url(repo Repo, path string) string {
	base := c.API
	if base == "" {
		base = DefaultAPI
	}
	return strings.TrimRight(base, "/") + "/repos/" + repo.Owner + "/" + repo.Name + path
}

// Error is a failed GitHub request. Temporary reports whether trying again
// later may work (GitHub or the network had a problem) or not (the
// repository, the commit or the token is wrong).
type Error struct {
	Status int // the HTTP status, or 0 if no answer came
	Err    error
}

func (e *Error) Error() string { return "GitHub: " + e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

func (e *Error) Temporary() bool {
	return e.Status == 0 || e.Status == http.StatusTooManyRequests || e.Status >= 500 ||
		// GitHub answers 403 when a rate limit is exhausted.
		e.Status == http.StatusForbidden && strings.Contains(e.Err.Error(), "rate limit")
}

// get sends an authenticated GET and returns the response if it's 200 OK.
func (c Client) get(ctx context.Context, url string, token string, accept string) (*http.Response, error) {
	if token == "" {
		return nil, &Error{Err: errors.New("no GitHub token yet (Lighthouse is still waiting for Cove)")}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, &Error{Err: err}
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		var body struct{ Message string }
		json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body)
		msg := resp.Status
		if body.Message != "" {
			msg += " (" + body.Message + ")"
		}
		return nil, &Error{Status: resp.StatusCode, Err: errors.New(msg)}
	}
	return resp, nil
}

// LatestCommit returns the newest commit on the repository's default branch.
func (c Client) LatestCommit(ctx context.Context, repo Repo, token string) (string, error) {
	resp, err := c.get(ctx, c.url(repo, "/commits?per_page=1"), token, "application/vnd.github+json")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var commits []struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&commits); err != nil {
		return "", &Error{Status: resp.StatusCode, Err: fmt.Errorf("unexpected response: %v", err)}
	}
	if len(commits) == 0 || commits[0].SHA == "" {
		return "", &Error{Status: http.StatusNotFound, Err: errors.New("the repository has no commits")}
	}
	return commits[0].SHA, nil
}

// Archive returns the repository's files at commit sha, as a gzipped tarball
// whose entries all sit in one top-level folder. The caller closes it. It
// works for private repositories the token can read.
func (c Client) Archive(ctx context.Context, repo Repo, sha string, token string) (io.ReadCloser, error) {
	resp, err := c.get(ctx, c.url(repo, "/tarball/"+sha), token, "application/vnd.github+json")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}
