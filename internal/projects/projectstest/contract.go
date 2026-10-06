package projectstest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/LSariol/LightHouse/internal/github"
	"github.com/LSariol/LightHouse/internal/projects"
)

// RunStoreTests runs the behaviour every projects.Store must have. open
// returns a new, empty Store for each test.
func RunStoreTests(t *testing.T, open func(t *testing.T) projects.Store) {
	ctx := context.Background()
	plop := github.Repo{Owner: "LSariol", Name: "Plop"}
	cove := github.Repo{Owner: "LSariol", Name: "Cove"}

	t.Run("AddGetList", func(t *testing.T) {
		s := open(t)
		p, err := s.Add(ctx, "plop", plop)
		if err != nil {
			t.Fatal(err)
		}
		if p.Name != "plop" || p.Repo != plop || p.CreatedAt.IsZero() || p.DeployedSHA != "" || p.Checks != 0 {
			t.Errorf("Add returned %+v", p)
		}
		if p.Container() != "plop" {
			t.Errorf("Container() = %q", p.Container())
		}

		got, err := s.Get(ctx, "PLOP")
		if err != nil || got.Name != "plop" || got.Repo != plop {
			t.Errorf("Get (other case) = %+v, %v", got, err)
		}

		s.Add(ctx, "cove", cove)
		list, err := s.List(ctx)
		if err != nil || len(list) != 2 || list[0].Name != "cove" || list[1].Name != "plop" {
			t.Errorf("List = %+v, %v (want cove, plop)", list, err)
		}

		if _, err := s.Get(ctx, "nope"); !errors.Is(err, projects.ErrNotFound) {
			t.Errorf("Get of a missing project = %v", err)
		}
	})

	t.Run("Uniqueness", func(t *testing.T) {
		s := open(t)
		s.Add(ctx, "plop", plop)
		if _, err := s.Add(ctx, "PLOP", cove); !errors.Is(err, projects.ErrNameTaken) {
			t.Errorf("same name in another case = %v, want ErrNameTaken", err)
		}
		if _, err := s.Add(ctx, "other", github.Repo{Owner: "lsariol", Name: "plop"}); !errors.Is(err, projects.ErrRepoWatched) {
			t.Errorf("same repository in another case = %v, want ErrRepoWatched", err)
		}
		if _, err := s.Add(ctx, "bad name", cove); !errors.Is(err, projects.ErrInvalidName) {
			t.Errorf("invalid name = %v, want ErrInvalidName", err)
		}
	})

	t.Run("RenameSetRepoRemove", func(t *testing.T) {
		s := open(t)
		s.Add(ctx, "plop", plop)
		s.Add(ctx, "cove", cove)

		if err := s.Rename(ctx, "plop", "Cove"); !errors.Is(err, projects.ErrNameTaken) {
			t.Errorf("rename onto another project = %v", err)
		}
		if err := s.Rename(ctx, "plop", "x y"); !errors.Is(err, projects.ErrInvalidName) {
			t.Errorf("rename to an invalid name = %v", err)
		}
		if err := s.Rename(ctx, "PLOP", "Plop"); err != nil {
			t.Errorf("rename to a different case of itself: %v", err)
		}
		if err := s.Rename(ctx, "nope", "x"); !errors.Is(err, projects.ErrNotFound) {
			t.Errorf("rename of a missing project = %v", err)
		}

		site := github.Repo{Owner: "LSariol", Name: "plop-site"}
		if err := s.SetRepo(ctx, "plop", site); err != nil {
			t.Fatal(err)
		}
		if p, _ := s.Get(ctx, "plop"); p.Repo != site || p.Container() != "plop-site" {
			t.Errorf("after SetRepo: %+v", p)
		}
		if err := s.SetRepo(ctx, "plop", cove); !errors.Is(err, projects.ErrRepoWatched) {
			t.Errorf("SetRepo onto another project's repository = %v", err)
		}
		if err := s.SetRepo(ctx, "plop", site); err != nil {
			t.Errorf("SetRepo to its own repository: %v", err)
		}

		if err := s.Remove(ctx, "PLOP"); err != nil {
			t.Fatal(err)
		}
		if err := s.Remove(ctx, "plop"); !errors.Is(err, projects.ErrNotFound) {
			t.Errorf("second Remove = %v", err)
		}
		// The repository is free again.
		if _, err := s.Add(ctx, "again", site); err != nil {
			t.Errorf("re-adding a removed project's repository: %v", err)
		}
	})

	t.Run("RecordCheck", func(t *testing.T) {
		s := open(t)
		s.Add(ctx, "plop", plop)

		if err := s.RecordCheck(ctx, "Plop", errors.New("GitHub: 404 Not Found")); err != nil {
			t.Fatal(err)
		}
		p, _ := s.Get(ctx, "plop")
		if p.Checks != 1 || p.LastCheckedAt == nil || p.LastError != "GitHub: 404 Not Found" || p.LastErrorAt == nil {
			t.Errorf("after a failed check: %+v", p)
		}

		s.RecordCheck(ctx, "plop", nil)
		p, _ = s.Get(ctx, "plop")
		if p.Checks != 2 || p.LastError != "" || p.LastErrorAt != nil {
			t.Errorf("after a good check: %+v", p)
		}

		if err := s.RecordCheck(ctx, "gone", nil); !errors.Is(err, projects.ErrNotFound) {
			t.Errorf("RecordCheck of a missing project = %v", err)
		}

		long := strings.Repeat("é", projects.MaxErrorLength) // 2 bytes each
		s.RecordCheck(ctx, "plop", errors.New(long))
		p, _ = s.Get(ctx, "plop")
		if len(p.LastError) > projects.MaxErrorLength || !strings.HasSuffix(p.LastError, "…") {
			t.Errorf("a long error is stored at %d bytes", len(p.LastError))
		}
	})

	t.Run("Deployments", func(t *testing.T) {
		s := open(t)
		s.Add(ctx, "plop", plop)
		start := time.Now().Add(-time.Hour).Truncate(time.Millisecond)

		failed := projects.Deployment{Project: "PLOP", SHA: "aaa", Trigger: projects.TriggerCheck, Status: projects.StatusFailed,
			StartedAt: start, FinishedAt: start.Add(time.Minute), Error: "build failed"}
		if err := s.RecordDeployment(ctx, failed); err != nil {
			t.Fatal(err)
		}
		p, _ := s.Get(ctx, "plop")
		if p.DeployedSHA != "" || p.LastError != "build failed" {
			t.Errorf("after a failed deploy: deployed %q, error %q", p.DeployedSHA, p.LastError)
		}

		ok := projects.Deployment{Project: "plop", SHA: "bbb", Trigger: projects.TriggerManual, Status: projects.StatusSucceeded,
			StartedAt: start.Add(2 * time.Minute), FinishedAt: start.Add(3 * time.Minute)}
		if err := s.RecordDeployment(ctx, ok); err != nil {
			t.Fatal(err)
		}
		p, _ = s.Get(ctx, "plop")
		if p.DeployedSHA != "bbb" || p.DeployedAt == nil || !p.DeployedAt.Equal(ok.FinishedAt) || p.LastError != "" {
			t.Errorf("after a good deploy: %+v", p)
		}

		history, err := s.History(ctx, "plop", 10)
		if err != nil || len(history) != 2 {
			t.Fatalf("History = %+v, %v", history, err)
		}
		if history[0].SHA != "bbb" || history[1].SHA != "aaa" || history[1].Error != "build failed" ||
			history[0].Project != "plop" || history[0].Trigger != projects.TriggerManual || !history[0].StartedAt.Equal(ok.StartedAt) {
			t.Errorf("History (newest first) = %+v", history)
		}
		if h, _ := s.History(ctx, "plop", 1); len(h) != 1 || h[0].SHA != "bbb" {
			t.Errorf("History limit 1 = %+v", h)
		}

		// Renaming keeps the history; removing drops it.
		s.Rename(ctx, "plop", "plop-web")
		if h, _ := s.History(ctx, "plop-web", 10); len(h) != 2 || h[0].Project != "plop-web" {
			t.Errorf("History after rename = %+v", h)
		}
		if err := s.RecordDeployment(ctx, projects.Deployment{Project: "gone", Trigger: projects.TriggerCheck, Status: projects.StatusFailed,
			StartedAt: start, FinishedAt: start}); !errors.Is(err, projects.ErrNotFound) {
			t.Errorf("RecordDeployment for a missing project = %v", err)
		}
		if _, err := s.History(ctx, "gone", 10); !errors.Is(err, projects.ErrNotFound) {
			t.Errorf("History of a missing project = %v", err)
		}
		s.Remove(ctx, "plop-web")
		s.Add(ctx, "plop-web", plop)
		if h, _ := s.History(ctx, "plop-web", 10); len(h) != 0 {
			t.Errorf("a re-added project inherited history: %+v", h)
		}
	})
}
