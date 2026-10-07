package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseRepoURL(t *testing.T) {
	good := []string{
		"https://github.com/LSariol/plop",
		"https://github.com/LSariol/plop/",
		"https://github.com/LSariol/plop.git",
		"http://github.com/LSariol/plop",
		"https://www.github.com/LSariol/plop",
		"github.com/LSariol/plop",
		"  https://github.com/LSariol/plop  ",
	}
	for _, raw := range good {
		repo, err := ParseRepoURL(raw)
		if err != nil {
			t.Errorf("ParseRepoURL(%q): %v", raw, err)
			continue
		}
		if repo != (Repo{Owner: "LSariol", Name: "plop"}) {
			t.Errorf("ParseRepoURL(%q) = %+v", raw, repo)
		}
	}

	bad := []string{
		"", "https://gitlab.com/a/b", "https://github.com/onlyowner",
		"https://github.com/a/b/tree/main", "https://github.com/a b/c", "git@github.com:a/b.git",
	}
	for _, raw := range bad {
		if _, err := ParseRepoURL(raw); !errors.Is(err, ErrInvalidURL) {
			t.Errorf("ParseRepoURL(%q) = %v, want ErrInvalidURL", raw, err)
		}
	}

	repo := Repo{Owner: "LSariol", Name: "plop"}
	if repo.URL() != "https://github.com/LSariol/plop" || repo.String() != "LSariol/plop" {
		t.Errorf("URL %s, String %s", repo.URL(), repo.String())
	}
}

// fakeGitHub serves the two endpoints Lighthouse uses, for repository o/good.
func fakeGitHub(t *testing.T) Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer t" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"message":"Bad credentials"}`))
			return
		}
		switch r.URL.Path {
		case "/repos/o/good/commits":
			w.Write([]byte(`[{"sha":"abc123"}]`))
		case "/repos/o/empty/commits":
			w.Write([]byte(`[]`))
		case "/repos/o/weird/commits":
			w.Write([]byte(`{"not":"a list"}`))
		case "/repos/o/good/tarball/abc123":
			w.Write([]byte("tarball bytes"))
		case "/repos/o/down/commits":
			w.WriteHeader(http.StatusBadGateway)
		case "/repos/o/limited/commits":
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"message":"API rate limit exceeded for user"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"Not Found"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return Client{HTTP: srv.Client(), API: srv.URL}
}

func temporary(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Temporary()
}

func TestLatestCommit(t *testing.T) {
	c := fakeGitHub(t)
	ctx := context.Background()

	if sha, err := c.LatestCommit(ctx, Repo{"o", "good"}, "t"); err != nil || sha != "abc123" {
		t.Errorf("good: %q, %v", sha, err)
	}

	cases := []struct {
		repo      string
		token     string
		contains  string
		temporary bool
	}{
		{"empty", "t", "no commits", false},
		{"weird", "t", "check o/weird for its newest commit: unexpected answer", false},
		{"missing", "t", "404 Not Found (Not Found). The repository, commit or file doesn't exist, or Lighthouse's token can't read it", false},
		{"good", "wrong", "Bad credentials). Lighthouse's GitHub token (LIGHTHOUSE_GITHUB_TOKEN in Cove) was rejected", false},
		{"down", "t", "502", true},
		{"limited", "t", "rate limit", true},
	}
	for _, c2 := range cases {
		_, err := c.LatestCommit(ctx, Repo{"o", c2.repo}, c2.token)
		if err == nil || !strings.Contains(err.Error(), c2.contains) || temporary(err) != c2.temporary {
			t.Errorf("%s: err = %v (temporary %v), want one containing %q (temporary %v)", c2.repo, err, temporary(err), c2.contains, c2.temporary)
		}
	}

	if _, err := c.LatestCommit(ctx, Repo{"o", "good"}, ""); err == nil {
		t.Error("no token: no error")
	}

	unreachable := Client{API: "http://127.0.0.1:1"}
	if _, err := unreachable.LatestCommit(ctx, Repo{"o", "good"}, "t"); !temporary(err) {
		t.Errorf("unreachable GitHub = %v, want a temporary error", err)
	}
}

func TestArchive(t *testing.T) {
	c := fakeGitHub(t)
	rc, err := c.Archive(context.Background(), Repo{"o", "good"}, "abc123", "t")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if b, _ := io.ReadAll(rc); string(b) != "tarball bytes" {
		t.Errorf("archive = %q", b)
	}

	if _, err := c.Archive(context.Background(), Repo{"o", "good"}, "nope", "t"); err == nil || temporary(err) {
		t.Errorf("a missing commit = %v, want a permanent error", err)
	}
}

func TestPolling(t *testing.T) {
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/o/r/commits":
			w.Header().Set("ETag", `"c1"`)
			if r.Header.Get("If-None-Match") == `"c1"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Write([]byte(`[{"sha":"abc"}]`))
		case r.URL.Path == "/repos/o/r/tags" && r.URL.Query().Get("page") == "":
			w.Header().Set("ETag", `"t1"`)
			if r.Header.Get("If-None-Match") == `"t1"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("Link", `<`+srvURL+`/repos/o/r/tags?per_page=100&page=2>; rel="next", <x>; rel="last"`)
			w.Write([]byte(`[{"name":"v1.2.0","commit":{"sha":"s120"}}]`))
		case r.URL.Path == "/repos/o/r/tags":
			w.Write([]byte(`[{"name":"v1.0.0","commit":{"sha":"s100"}}]`))
		case r.URL.Path == "/repos/o/r/commits/abc1234":
			w.Write([]byte(`{"sha":"abc1234def"}`))
		case r.URL.Path == "/repos/o/r/contents/compose.yaml":
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Path == "/repos/o/r/contents/compose.yml" && r.URL.Query().Get("ref") == "abc":
			if r.Header.Get("Accept") != "application/vnd.github.raw+json" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Write([]byte("name: r\n"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL
	c := Client{HTTP: srv.Client(), API: srv.URL}
	ctx := context.Background()
	repo := Repo{"o", "r"}

	sha, etag, err := c.CheckCommit(ctx, repo, "t", "")
	if err != nil || sha != "abc" || etag != `"c1"` {
		t.Errorf("CheckCommit = %q, %q, %v", sha, etag, err)
	}
	if _, _, err := c.CheckCommit(ctx, repo, "t", etag); !errors.Is(err, ErrNotModified) {
		t.Errorf("CheckCommit with the ETag = %v, want ErrNotModified", err)
	}

	tags, etag, err := c.Tags(ctx, repo, "t", "")
	if err != nil || len(tags) != 2 || tags[0] != (Tag{"v1.2.0", "s120"}) || tags[1] != (Tag{"v1.0.0", "s100"}) || etag != `"t1"` {
		t.Errorf("Tags = %+v, %q, %v (want both pages)", tags, etag, err)
	}
	if _, _, err := c.Tags(ctx, repo, "t", etag); !errors.Is(err, ErrNotModified) {
		t.Errorf("Tags with the ETag = %v, want ErrNotModified", err)
	}

	if sha, err := c.ResolveCommit(ctx, repo, "abc1234", "t"); err != nil || sha != "abc1234def" {
		t.Errorf("ResolveCommit = %q, %v", sha, err)
	}
	if _, err := c.ResolveCommit(ctx, repo, "nope", "t"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("ResolveCommit of an unknown commit = %v", err)
	}

	name, text, err := c.ComposeFile(ctx, repo, "abc", "t")
	if err != nil || name != "compose.yml" || string(text) != "name: r\n" {
		t.Errorf("ComposeFile = %q, %q, %v", name, text, err)
	}
	if _, _, err := c.ComposeFile(ctx, Repo{"o", "none"}, "abc", "t"); err == nil || !strings.Contains(err.Error(), "no compose file") || temporary(err) {
		t.Errorf("no compose file: %v", err)
	}
}
