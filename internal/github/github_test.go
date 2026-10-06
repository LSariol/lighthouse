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
		{"weird", "t", "unexpected response", false},
		{"missing", "t", "404", false},
		{"good", "wrong", "Bad credentials", false},
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
