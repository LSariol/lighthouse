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
	"net/url"
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

// ErrNotModified means GitHub's answer hasn't changed since the ETag given.
// Such a request doesn't count against the rate limit.
var ErrNotModified = errors.New("not modified")

// get sends an authenticated GET and returns the response if it's 200 OK.
// With an etag, a 304 (unchanged) is ErrNotModified.
func (c Client) get(ctx context.Context, url string, token string, accept string, etag string) (*http.Response, error) {
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
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, &Error{Err: err}
	}
	if resp.StatusCode == http.StatusNotModified && etag != "" {
		resp.Body.Close()
		return nil, ErrNotModified
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
	sha, _, err := c.CheckCommit(ctx, repo, token, "")
	return sha, err
}

// CheckCommit is LatestCommit for polling: given the ETag of the last
// answer, it returns ErrNotModified if nothing changed, and the new ETag.
func (c Client) CheckCommit(ctx context.Context, repo Repo, token string, etag string) (string, string, error) {
	resp, err := c.get(ctx, c.url(repo, "/commits?per_page=1"), token, "application/vnd.github+json", etag)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	var commits []struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&commits); err != nil {
		return "", "", &Error{Status: resp.StatusCode, Err: fmt.Errorf("unexpected response: %v", err)}
	}
	if len(commits) == 0 || commits[0].SHA == "" {
		return "", "", &Error{Status: http.StatusNotFound, Err: errors.New("the repository has no commits")}
	}
	return commits[0].SHA, resp.Header.Get("ETag"), nil
}

// ResolveCommit returns the full SHA of a commit given as a full or short
// SHA (or any ref GitHub understands).
func (c Client) ResolveCommit(ctx context.Context, repo Repo, ref string, token string) (string, error) {
	resp, err := c.get(ctx, c.url(repo, "/commits/"+url.PathEscape(ref)), token, "application/vnd.github+json", "")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var commit struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&commit); err != nil || commit.SHA == "" {
		return "", &Error{Status: resp.StatusCode, Err: fmt.Errorf("unexpected answer for commit %q: %v", ref, err)}
	}
	return commit.SHA, nil
}

// Archive returns the repository's files at commit sha, as a gzipped tarball
// whose entries all sit in one top-level folder. The caller closes it. It
// works for private repositories the token can read.
func (c Client) Archive(ctx context.Context, repo Repo, sha string, token string) (io.ReadCloser, error) {
	resp, err := c.get(ctx, c.url(repo, "/tarball/"+sha), token, "application/vnd.github+json", "")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Tag is a tag and the commit it points at.
type Tag struct {
	Name string
	SHA  string
}

// maxTagPages caps how many pages of 100 tags are read.
const maxTagPages = 10

// Tags lists the repository's tags, with the commit each points at
// (annotated tags included). Given the ETag of the last answer, it returns
// ErrNotModified if the first page hasn't changed.
func (c Client) Tags(ctx context.Context, repo Repo, token string, etag string) ([]Tag, string, error) {
	page := c.url(repo, "/tags?per_page=100")
	var tags []Tag
	newETag := ""
	for n := 0; n < maxTagPages && page != ""; n++ {
		pageETag := ""
		if n == 0 {
			pageETag = etag
		}
		resp, err := c.get(ctx, page, token, "application/vnd.github+json", pageETag)
		if err != nil {
			return nil, "", err
		}
		if n == 0 {
			newETag = resp.Header.Get("ETag")
		}
		var list []struct {
			Name   string `json:"name"`
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&list)
		resp.Body.Close()
		if err != nil {
			return nil, "", &Error{Status: resp.StatusCode, Err: fmt.Errorf("unexpected response: %v", err)}
		}
		for _, t := range list {
			tags = append(tags, Tag{Name: t.Name, SHA: t.Commit.SHA})
		}
		page = nextPage(resp.Header.Get("Link"))
	}
	return tags, newETag, nil
}

// nextPage finds the rel="next" URL in a Link header.
func nextPage(link string) string {
	for _, part := range strings.Split(link, ",") {
		url, rel, ok := strings.Cut(part, ";")
		if ok && strings.Contains(rel, `rel="next"`) {
			return strings.Trim(strings.TrimSpace(url), "<>")
		}
	}
	return ""
}

// ComposeFiles are the names Compose looks for, in its order.
var ComposeFiles = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

// ComposeFile returns the compose file at the top of the repository at a
// commit, and its name, without downloading the rest.
func (c Client) ComposeFile(ctx context.Context, repo Repo, sha string, token string) (string, []byte, error) {
	for _, name := range ComposeFiles {
		resp, err := c.get(ctx, c.url(repo, "/contents/"+name+"?ref="+sha), token, "application/vnd.github.raw+json", "")
		var gh *Error
		if errors.As(err, &gh) && gh.Status == http.StatusNotFound {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		text, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if err != nil {
			return "", nil, &Error{Err: err}
		}
		return name, text, nil
	}
	return "", nil, &Error{Status: http.StatusNotFound, Err: fmt.Errorf("no compose file at the top of the repository (%s)", strings.Join(ComposeFiles, ", "))}
}
