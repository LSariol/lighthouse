package github

import (
	"context"
	"errors"
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
	if repo.URL() != "https://github.com/LSariol/plop" ||
		repo.APIURL() != "https://api.github.com/repos/LSariol/plop" ||
		repo.ArchiveURL() != "https://github.com/LSariol/plop/archive/refs/heads/main.zip" {
		t.Errorf("URLs: %s %s %s", repo.URL(), repo.APIURL(), repo.ArchiveURL())
	}
}

func TestLatestCommit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer t" {
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/repos/o/good/commits":
			w.Write([]byte(`[{"sha":"abc123"}]`))
		case "/repos/o/empty/commits":
			w.Write([]byte(`[]`))
		case "/repos/o/weird/commits":
			w.Write([]byte(`{"message":"not a list"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := Client{HTTP: srv.Client()}
	ctx := context.Background()

	if sha, err := c.LatestCommit(ctx, srv.URL+"/repos/o/good", "t"); err != nil || sha != "abc123" {
		t.Errorf("good: %q, %v", sha, err)
	}

	for name, want := range map[string]string{
		"empty":   "no commits",
		"weird":   "unexpected response",
		"missing": "404",
	} {
		if _, err := c.LatestCommit(ctx, srv.URL+"/repos/o/"+name, "t"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want one containing %q", name, err, want)
		}
	}

	if _, err := c.LatestCommit(ctx, srv.URL+"/repos/o/good", "wrong"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("wrong token: %v", err)
	}
	if _, err := c.LatestCommit(ctx, srv.URL+"/repos/o/good", ""); err == nil {
		t.Error("no token: no error")
	}
}
