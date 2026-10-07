package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/lsariol/coveclient"
	"github.com/lsariol/lighthouse"
	"github.com/lsariol/lighthouse/internal/cli"
	"github.com/lsariol/lighthouse/internal/compose"
	"github.com/lsariol/lighthouse/internal/config"
	"github.com/lsariol/lighthouse/internal/control"
	"github.com/lsariol/lighthouse/internal/cove"
	"github.com/lsariol/lighthouse/internal/daemon"
	"github.com/lsariol/lighthouse/internal/database"
	"github.com/lsariol/lighthouse/internal/deploy"
	"github.com/lsariol/lighthouse/internal/docker"
	"github.com/lsariol/lighthouse/internal/github"
	"github.com/lsariol/lighthouse/internal/orchestrator"
	"github.com/lsariol/lighthouse/internal/policy"
	"github.com/lsariol/lighthouse/internal/projects"
	"github.com/lsariol/lighthouse/internal/reposjson"
)

// retryEvery is how long startup waits before trying an unreachable Cove or
// database again.
const retryEvery = 15 * time.Second

// shutdownWait is how long stopping waits for a deploy in progress. Keep it
// under the compose file's stop_grace_period.
const shutdownWait = 100 * time.Second

func main() {
	args := os.Args[1:]

	mode := ""
	if len(args) > 0 {
		mode = args[0]
	}

	switch mode {
	case "":
		runServe(true)
	case "serve":
		runServe(false)
	case "-h", "--help":
		printUsage(os.Stdout)
	case "shell":
		runShell()
	case "migrate":
		runMigrate(args[1:])
	case "import":
		runImport(args[1:])
	case "version", "--version":
		fmt.Println(buildVersion(loadConfig()))
	case "health":
		os.Exit(runHealth())
	case "self-update":
		os.Exit(runSelfUpdate(args[1:]))
	default:
		os.Exit(runCommand(args))
	}
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  lighthouse                    Run Lighthouse with its CLI on this terminal (local use)
  lighthouse serve              Run Lighthouse only (what Docker runs)
  lighthouse shell              Open the CLI of a running Lighthouse
  lighthouse <command> [args]   Run one CLI command and exit
  lighthouse migrate [status|up]
                                Show or apply database migrations
  lighthouse import <file|->    Move a pre-1.0 repos.json into the database
  lighthouse version            Print the version
  lighthouse health             Exit 0 if the daemon answers (the container's healthcheck)
  lighthouse self-update <file> The update helper's work (Lighthouse starts it itself)

In Docker: docker exec -it lighthouse /lighthouse shell
       or: docker exec lighthouse /lighthouse status
Run "lighthouse help" to list the CLI commands.
`)
}

// runServe runs the daemon until SIGINT or SIGTERM (docker stop).
func runServe(withShell bool) {
	cfg := loadConfig()
	if err := cfg.ValidateServe(); err != nil {
		fatal(err)
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	version := buildVersion(cfg)
	slog.Info("Lighthouse starting", "version", version, "env", cfg.Env)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dockerClient, err := docker.New()
	if err != nil {
		fatal(err)
	}
	defer dockerClient.Close()

	d := daemon.New(cfg, version, dockerClient, compose.Runner{})

	controlDone := make(chan error, 1)
	go func() { controlDone <- control.Serve(ctx, cfg.ControlSocket, d) }()

	if withShell {
		shell := cli.New(d, cli.Options{Env: cfg.Env, Embedded: true})
		go shell.Run(ctx, stop)
	}

	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)

		db, orch, err := start(ctx, cfg, d, dockerClient)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			fatalServe(cfg, err)
		}
		defer db.Close()

		orch.Run(ctx, cfg.PollInterval)
	}()

	select {
	case err := <-controlDone:
		if err != nil && ctx.Err() == nil {
			fatal(err)
		}
	case <-ctx.Done():
	}

	slog.Info("Lighthouse stopping")
	select {
	case <-loopDone:
	case <-time.After(shutdownWait):
		slog.Warn("a deploy was still running at shutdown")
	}
	<-controlDone
	slog.Info("Lighthouse stopped")
}

// start connects to Cove and the database, migrates it, and hands the daemon
// its store and orchestrator.
func start(ctx context.Context, cfg config.Config, d *daemon.Daemon, dockerClient *docker.Client) (*database.Database, *orchestrator.Orchestrator, error) {
	rules, err := policy.Parse(lighthouse.PolicyFile)
	if err != nil {
		return nil, nil, err
	}
	if len(rules.Exceptions) > 0 {
		slog.Info("deploy rule exceptions loaded", "exceptions", len(rules.Exceptions))
	}
	if _, err := os.Stat(cfg.StoragePath); err != nil {
		slog.Warn("STORAGE_PATH isn't readable, so symlinks in the projects' data folders can't be checked: mount it read-only at the same path",
			"path", cfg.StoragePath, "err", err)
	}

	coveClient := coveclient.New(cfg.CoveURL, "")

	d.SetPhase(daemon.PhaseWaiting)
	if err := cove.Connect(ctx, coveClient, cfg.CoveTokenPath, retryEvery); err != nil {
		return nil, nil, err
	}
	secrets, err := cove.ReadSecrets(ctx, coveClient)
	if err != nil {
		return nil, nil, err
	}
	slog.Info("secrets loaded from Cove", "github_token_length", len(secrets.GitHubToken))

	db, err := openDatabase(ctx, d, secrets)
	if err != nil {
		return nil, nil, err
	}

	self, err := dockerClient.SelfProject(ctx)
	if err != nil {
		slog.Warn("self-update is off: Lighthouse can't find its own container", "err", err)
	}
	deployer := deploy.New(compose.Runner{}, dockerClient, github.Client{}, coveClient, deploy.Options{
		Self:    self,
		Root:    cfg.StagingPath,
		Storage: cfg.StoragePath,
		Backups: cfg.BackupPath,
		Policy:  rules,
		Log:     os.Stderr,
	})
	if self != "" {
		slog.Info("self-update is on", "compose_project", self)
	}
	commits := github.Client{HTTP: &http.Client{Timeout: 30 * time.Second}}
	orch := orchestrator.New(db, commits, deployer, dockerClient, secrets.GitHubToken)
	d.Ready(db, orch, db)
	orch.RecordHandOffs(ctx)

	list, _ := db.List(ctx)
	slog.Info("Lighthouse ready", "projects", len(list), "poll_interval", cfg.PollInterval.String(), "control_socket", cfg.ControlSocket)
	return db, orch, nil
}

// openDatabase applies pending migrations as the migrator, then connects as
// the app and checks the schema.
func openDatabase(ctx context.Context, d *daemon.Daemon, secrets cove.Secrets) (*database.Database, error) {
	for {
		d.SetPhase(daemon.PhaseMigrating)
		err := database.Migrate(ctx, secrets.MigratorDatabaseURL)
		if err == nil {
			var db *database.Database
			db, err = database.Connect(ctx, secrets.DatabaseURL)
			if err == nil {
				if err := db.CheckSchemaVersion(ctx); err != nil {
					db.Close()
					return nil, err
				}
				return db, nil
			}
		}
		if !database.IsUnreachable(err) {
			return nil, err
		}

		d.SetPhase(daemon.PhaseDatabase)
		slog.Warn("waiting for the database to be reachable", "err", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(retryEvery):
		}
	}
}

// runShell runs the interactive CLI next to a running daemon.
func runShell() {
	cfg := loadConfig()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()

	shell := cli.New(control.NewClient(cfg.ControlSocket), cli.Options{Env: cfg.Env})
	shell.Run(ctx, stop)
}

// runHealth is the container's healthcheck: 0 once the daemon answers.
func runHealth() int {
	cfg := loadConfig()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := control.NewClient(cfg.ControlSocket).Status(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lighthouse health: %v\n", err)
		return 1
	}
	fmt.Println(s.Phase)
	return 0
}

// runSelfUpdate is the update helper's work (internal/deploy/selfupdate.go).
func runSelfUpdate(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "Usage: lighthouse self-update <hand-off file>")
		return 2
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	dockerClient, err := docker.New()
	if err != nil {
		fatal(err)
	}
	defer dockerClient.Close()

	deployer := deploy.New(compose.Runner{}, dockerClient, nil, nil, deploy.Options{
		Root: filepath.Dir(filepath.Dir(args[0])),
		Log:  os.Stderr,
	})
	if err := deployer.FinishHandOff(context.Background(), args[0]); err != nil {
		slog.Error("self-update failed", "err", err)
		return 1
	}
	slog.Info("self-update finished: the new Lighthouse is healthy")
	return 0
}

// runCommand runs one CLI command and returns the exit status.
func runCommand(args []string) int {
	cfg := loadConfig()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shell := cli.New(control.NewClient(cfg.ControlSocket), cli.Options{Env: cfg.Env})
	if err := shell.Exec(ctx, args); err != nil {
		cli.Report(err)
		return 1
	}
	return 0
}

// adminSecrets reads Lighthouse's secrets from Cove for the admin modes
// (migrate, import), which run next to the daemon with the same settings.
func adminSecrets(ctx context.Context, cfg config.Config) cove.Secrets {
	if err := cfg.ValidateCove(); err != nil {
		fatal(err)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	c := coveclient.New(cfg.CoveURL, "")
	if err := cove.Connect(ctx, c, cfg.CoveTokenPath, 5*time.Second); err != nil {
		fatal(fmt.Errorf("connecting to Cove: %w", err))
	}
	secrets, err := cove.ReadSecrets(ctx, c)
	if err != nil {
		fatal(err)
	}
	return secrets
}

// runMigrate handles `lighthouse migrate [status|up]`, as the migrator.
func runMigrate(args []string) {
	command := "status"
	if len(args) > 0 {
		command = args[0]
	}
	if len(args) > 1 || (command != "status" && command != "up") {
		fmt.Fprintln(os.Stderr, "! Usage: lighthouse migrate [status|up]")
		os.Exit(1)
	}

	ctx := context.Background()
	secrets := adminSecrets(ctx, loadConfig())

	var err error
	if command == "up" {
		err = database.Migrate(ctx, secrets.MigratorDatabaseURL)
	} else {
		err = database.PrintMigrationStatus(ctx, secrets.MigratorDatabaseURL, os.Stdout)
	}
	if err != nil {
		fatal(err)
	}
}

// runImport handles `lighthouse import <file|->`: it adds every project in a
// pre-1.0 repos.json to the database, with its recorded state.
func runImport(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "! Usage: lighthouse import <file|->   (- reads standard input)")
		os.Exit(1)
	}

	var in io.Reader = os.Stdin
	if args[0] != "-" {
		f, err := os.Open(args[0])
		if err != nil {
			fatal(err)
		}
		defer f.Close()
		in = f
	}
	list, err := reposjson.Read(in)
	if err != nil {
		fatal(err)
	}

	ctx := context.Background()
	secrets := adminSecrets(ctx, loadConfig())

	db, err := database.Connect(ctx, secrets.DatabaseURL)
	if err != nil {
		fatal(err)
	}
	defer db.Close()
	if err := db.CheckSchemaVersion(ctx); err != nil {
		fatal(err)
	}

	imported, skipped, failed := 0, 0, 0
	for _, p := range list {
		switch err := db.Import(ctx, p); {
		case err == nil:
			fmt.Fprintf(os.Stderr, "✓ Imported %s (%s)\n", p.Name, p.Repo.URL())
			imported++
		case errors.Is(err, projects.ErrNameTaken) || errors.Is(err, projects.ErrRepoWatched):
			fmt.Fprintf(os.Stderr, "! Skipped %s: it's already in the database\n", p.Name)
			skipped++
		default:
			fmt.Fprintf(os.Stderr, "✗ %s: %v\n", p.Name, err)
			failed++
		}
	}

	fmt.Fprintf(os.Stderr, "%d imported, %d already there, %d failed.\n", imported, skipped, failed)
	if failed > 0 {
		os.Exit(1)
	}
}

func loadConfig() config.Config {
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	return cfg
}

// buildVersion returns LIGHTHOUSE_VERSION (set in docker-compose.yml).
func buildVersion(cfg config.Config) string {
	if cfg.Version != "" {
		return cfg.Version
	}

	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && len(setting.Value) >= 7 {
				return "dev-" + setting.Value[:7]
			}
		}
	}
	return "dev"
}

// fatal prints an error and exits.
func fatal(err error) {
	fmt.Fprintf(os.Stderr, "lighthouse: %v\n", err)
	os.Exit(1)
}

// fatalServe is fatal for the daemon once its control socket is open: it
// removes the socket so the next start doesn't find a stale one.
func fatalServe(cfg config.Config, err error) {
	os.Remove(cfg.ControlSocket)
	fatal(err)
}
