package projectstest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lsariol/lighthouse/internal/github"
	"github.com/lsariol/lighthouse/internal/projects"
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
		if p.ComposeName() != "plop" {
			t.Errorf("ComposeName() = %q", p.ComposeName())
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
		if p, _ := s.Get(ctx, "plop"); p.Repo != site || p.ComposeName() != "plop-site" {
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

	t.Run("ComposeProject", func(t *testing.T) {
		s := open(t)
		s.Add(ctx, "personalWebsite", github.Repo{Owner: "lsariol", Name: "Landing"})
		s.Add(ctx, "cove", cove)

		p, _ := s.Get(ctx, "personalWebsite")
		if p.ComposeProject != "" || p.ComposeName() != "landing" {
			t.Errorf("before a deploy: %q, ComposeName %q", p.ComposeProject, p.ComposeName())
		}

		if err := s.SetComposeProject(ctx, "personalwebsite", "website"); err != nil {
			t.Fatal(err)
		}
		if p, _ := s.Get(ctx, "personalWebsite"); p.ComposeProject != "website" || p.ComposeName() != "website" {
			t.Errorf("after SetComposeProject: %+v", p)
		}
		if err := s.SetComposeProject(ctx, "personalWebsite", "website"); err != nil {
			t.Errorf("setting the same compose project again: %v", err)
		}
		if err := s.SetComposeProject(ctx, "cove", "website"); !errors.Is(err, projects.ErrComposeProjectTaken) {
			t.Errorf("another project's compose project = %v, want ErrComposeProjectTaken", err)
		}
		if err := s.SetComposeProject(ctx, "gone", "x"); !errors.Is(err, projects.ErrNotFound) {
			t.Errorf("SetComposeProject of a missing project = %v", err)
		}
	})

	t.Run("FailuresAndBroken", func(t *testing.T) {
		s := open(t)
		s.Add(ctx, "plop", plop)
		start := time.Now().Add(-time.Hour)
		fail := func(sha, kind string) {
			t.Helper()
			start = start.Add(time.Minute)
			if err := s.RecordDeployment(ctx, projects.Deployment{Project: "plop", SHA: sha, Trigger: projects.TriggerCheck,
				Status: projects.StatusFailed, FailureKind: kind, FailedStep: "build", StartedAt: start, FinishedAt: start, Error: "no"}); err != nil {
				t.Fatal(err)
			}
		}

		// Transient failures don't count.
		fail("aaa", projects.FailureTransient)
		fail("aaa", projects.FailureTransient)
		if p, _ := s.Get(ctx, "plop"); p.FailureCount != 0 || p.Broken {
			t.Errorf("after transient failures: count %d, broken %v", p.FailureCount, p.Broken)
		}

		fail("aaa", projects.FailurePermanent)
		fail("aaa", projects.FailurePermanent)
		if p, _ := s.Get(ctx, "plop"); p.FailureCount != 2 || p.FailingSHA != "aaa" || p.Broken {
			t.Errorf("after two permanent failures: %+v", p)
		}

		// A new commit starts the count again.
		fail("bbb", projects.FailurePermanent)
		if p, _ := s.Get(ctx, "plop"); p.FailureCount != 1 || p.FailingSHA != "bbb" {
			t.Errorf("after a new commit failed: %+v", p)
		}
		fail("bbb", projects.FailurePermanent)
		fail("bbb", projects.FailurePermanent)
		if p, _ := s.Get(ctx, "plop"); p.FailureCount != projects.BrokenAfter || !p.Broken {
			t.Errorf("after %d failures: %+v", projects.BrokenAfter, p)
		}

		// retry clears it; so does a success.
		if err := s.ClearFailures(ctx, "PLOP"); err != nil {
			t.Fatal(err)
		}
		if p, _ := s.Get(ctx, "plop"); p.FailureCount != 0 || p.Broken || p.FailingSHA != "" {
			t.Errorf("after ClearFailures: %+v", p)
		}
		fail("bbb", projects.FailurePermanent)
		s.RecordDeployment(ctx, projects.Deployment{Project: "plop", SHA: "ccc", Trigger: projects.TriggerCheck,
			Status: projects.StatusSucceeded, StartedAt: start, FinishedAt: start})
		if p, _ := s.Get(ctx, "plop"); p.FailureCount != 0 || p.FailingSHA != "" || p.DeployedSHA != "ccc" {
			t.Errorf("after a success: %+v", p)
		}

		// A rollback is a failure too.
		s.RecordDeployment(ctx, projects.Deployment{Project: "plop", SHA: "ddd", Trigger: projects.TriggerCheck,
			Status: projects.StatusRolledBack, FailureKind: projects.FailurePermanent, FailedStep: "verify",
			StartedAt: start.Add(time.Minute), FinishedAt: start.Add(time.Minute), Error: "web didn't become healthy"})
		if p, _ := s.Get(ctx, "plop"); p.FailureCount != 1 || p.DeployedSHA != "ccc" || p.LastError == "" {
			t.Errorf("after a rollback: %+v", p)
		}
		if err := s.ClearFailures(ctx, "gone"); !errors.Is(err, projects.ErrNotFound) {
			t.Errorf("ClearFailures of a missing project = %v", err)
		}
	})

	t.Run("DeploymentSteps", func(t *testing.T) {
		s := open(t)
		s.Add(ctx, "plop", plop)
		start := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
		long := strings.Repeat("x", projects.MaxLogLength) + "THE END"

		older := projects.Deployment{Project: "plop", SHA: "aaa", Trigger: projects.TriggerCheck, Status: projects.StatusSucceeded,
			StartedAt: start, FinishedAt: start.Add(time.Minute),
			Steps: []projects.Step{{Name: "fetch", Status: projects.StepSucceeded, StartedAt: start, FinishedAt: start}}}
		newer := projects.Deployment{Project: "plop", SHA: "bbb", Trigger: projects.TriggerManual, Status: projects.StatusFailed,
			FailureKind: projects.FailurePermanent, FailedStep: "build", Error: "build failed",
			StartedAt: start.Add(time.Hour), FinishedAt: start.Add(time.Hour + time.Minute),
			Steps: []projects.Step{
				{Name: "fetch", Status: projects.StepSucceeded, StartedAt: start.Add(time.Hour), FinishedAt: start.Add(time.Hour), Log: "ok"},
				{Name: "build", Status: projects.StepFailed, StartedAt: start.Add(time.Hour), FinishedAt: start.Add(time.Hour), Log: long},
				{Name: "swap", Status: projects.StepSkipped, StartedAt: start.Add(time.Hour), FinishedAt: start.Add(time.Hour)},
			}}
		for _, d := range []projects.Deployment{older, newer} {
			if err := s.RecordDeployment(ctx, d); err != nil {
				t.Fatal(err)
			}
		}

		d, err := s.Deployment(ctx, "PLOP", 1)
		if err != nil {
			t.Fatal(err)
		}
		if d.SHA != "bbb" || d.FailedStep != "build" || d.FailureKind != projects.FailurePermanent || len(d.Steps) != 3 {
			t.Fatalf("Deployment 1 = %+v", d)
		}
		if d.Steps[0].Name != "fetch" || d.Steps[0].Log != "ok" || d.Steps[1].Status != projects.StepFailed || d.Steps[2].Status != projects.StepSkipped {
			t.Errorf("steps = %+v", d.Steps)
		}
		if l := d.Steps[1].Log; len(l) > projects.MaxLogLength || !strings.HasSuffix(l, "THE END") {
			t.Errorf("a long log is stored at %d bytes, ending %q", len(l), l[len(l)-10:])
		}
		if d, _ := s.Deployment(ctx, "plop", 2); d.SHA != "aaa" || len(d.Steps) != 1 {
			t.Errorf("Deployment 2 = %+v", d)
		}
		if _, err := s.Deployment(ctx, "plop", 3); !errors.Is(err, projects.ErrNoDeployment) {
			t.Errorf("Deployment 3 = %v, want ErrNoDeployment", err)
		}
		if _, err := s.Deployment(ctx, "gone", 1); !errors.Is(err, projects.ErrNotFound) {
			t.Errorf("Deployment of a missing project = %v", err)
		}
		if h, _ := s.History(ctx, "plop", 10); len(h) != 2 || h[0].FailedStep != "build" || h[0].Steps != nil {
			t.Errorf("History = %+v", h)
		}
	})
}
