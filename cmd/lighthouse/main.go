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
	"runtime/debug"
	"syscall"
	"time"

	"github.com/LSariol/LightHouse/internal/cli"
	"github.com/LSariol/LightHouse/internal/config"
	"github.com/LSariol/LightHouse/internal/control"
	"github.com/LSariol/LightHouse/internal/cove"
	"github.com/LSariol/LightHouse/internal/daemon"
	"github.com/LSariol/LightHouse/internal/database"
	"github.com/LSariol/LightHouse/internal/deploy"
	"github.com/LSariol/LightHouse/internal/docker"
	"github.com/LSariol/LightHouse/internal/github"
	"github.com/LSariol/LightHouse/internal/orchestrator"
	"github.com/LSariol/LightHouse/internal/projects"
	"github.com/LSariol/LightHouse/internal/reposjson"
	"github.com/lsariol/coveclient"
)

// retryEvery is how long startup waits before trying an unreachable Cove or
// database again.
const retryEvery = 15 * time.Second

func main() {
	args := os.Args[1:]

	mode := ""
	if len(args) > 0 {
		mode = args[0]
	}

	switch mode {
	case "", "-h", "--help":
		printUsage(os.Stdout)
	case "serve":
		runServe()
	case "shell":
		runShell()
	case "migrate":
		runMigrate(args[1:])
	case "import":
		runImport(args[1:])
	case "version", "--version":
		fmt.Println(buildVersion(loadConfig()))
	default:
		os.Exit(runCommand(args))
	}
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  lighthouse serve              Run Lighthouse: watch, deploy, answer the CLI
  lighthouse shell              Open the interactive CLI
  lighthouse <command> [args]   Run one CLI command and exit
  lighthouse migrate [status|up]
                                Show or apply database migrations
  lighthouse import <file|->    Move a pre-1.0 repos.json into the database
  lighthouse version            Print the version

In Docker: docker exec -it lighthouse /lighthouse shell
       or: docker exec lighthouse /lighthouse status
Run "lighthouse help" to list the CLI commands.
`)
}

// runServe runs the daemon until SIGINT or SIGTERM (docker stop).
func runServe() {
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

	d := daemon.New(cfg, version, dockerClient)

	// The CLI can connect right away, so `status` shows startup progress
	// while Lighthouse waits for Cove and the database.
	controlDone := make(chan error, 1)
	go func() { controlDone <- control.Serve(ctx, cfg.ControlSocket, d) }()

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
	case <-time.After(8 * time.Second):
		slog.Warn("a deploy was still running at shutdown")
	}
	<-controlDone
	slog.Info("Lighthouse stopped")
}

// start connects to Cove and the database, migrates it, and hands the daemon
// its store and orchestrator. While Cove or the database is unreachable it
// waits and retries; other errors end startup.
func start(ctx context.Context, cfg config.Config, d *daemon.Daemon, dockerClient *docker.Client) (*database.Database, *orchestrator.Orchestrator, error) {
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

	deployer := deploy.New(dockerClient, coveClient, cfg.StagingPath, cfg.DownloadPath)
	commits := github.Client{HTTP: &http.Client{Timeout: 30 * time.Second}}
	orch := orchestrator.New(db, commits, deployer, secrets.GitHubToken)
	d.Ready(db, orch, db)

	list, _ := db.List(ctx)
	slog.Info("Lighthouse ready", "projects", len(list), "poll_interval", cfg.PollInterval.String(), "control_socket", cfg.ControlSocket)
	return db, orch, nil
}

// openDatabase applies pending migrations as the migrator, then connects as
// the app and checks the schema. While the database is unreachable (e.g.
// sparkdb restarting), it waits and retries.
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

// runCommand runs one CLI command, e.g. `lighthouse status`, and returns the
// process exit status: 0 on success, 1 on failure.
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

	// Don't wait forever for a closed bootstrap or a missing Cove: say so.
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
// pre-1.0 repos.json to the database, with its recorded state. Projects that
// are already there are skipped, so running it twice is safe.
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
// Without it, as in a local build, it's "dev" plus the git commit Lighthouse
// was built from when Go recorded one.
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

// fatal prints an error and exits. A clear one-line message is more useful
// here than a panic's stack trace, especially in docker logs.
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
