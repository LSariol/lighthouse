package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	"github.com/LSariol/LightHouse/internal/deploy"
	"github.com/LSariol/LightHouse/internal/docker"
	"github.com/LSariol/LightHouse/internal/github"
	"github.com/LSariol/LightHouse/internal/orchestrator"
	"github.com/LSariol/LightHouse/internal/watchlist"
	"github.com/lsariol/coveclient"
)

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

	projects, err := watchlist.Load(cfg.RepoPath)
	if err != nil {
		fatal(watchlistError(cfg.RepoPath, err))
	}

	coveClient := coveclient.New(cfg.CoveURL, "")
	deployer := deploy.New(dockerClient, coveClient, cfg.StagingPath, cfg.DownloadPath)
	orch := orchestrator.New(projects, github.Client{HTTP: &http.Client{Timeout: 30 * time.Second}}, deployer)
	d := daemon.New(cfg, version, projects, orch, dockerClient)

	// The CLI can connect right away, so `status` shows startup progress
	// while Lighthouse waits for Cove.
	controlDone := make(chan error, 1)
	go func() { controlDone <- control.Serve(ctx, cfg.ControlSocket, d) }()

	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)

		d.SetPhase(daemon.PhaseWaiting)
		token, err := connectCove(ctx, coveClient, cfg.CoveTokenPath)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			fatalServe(cfg, err)
		}
		orch.SetGitToken(token)

		d.SetPhase(daemon.PhaseRunning)
		slog.Info("Lighthouse ready", "projects", len(projects.All()), "poll_interval", cfg.PollInterval.String(), "control_socket", cfg.ControlSocket)
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

// connectCove connects to Cove and returns Lighthouse's GitHub token.
func connectCove(ctx context.Context, c *coveclient.Client, tokenPath string) (string, error) {
	if err := cove.Connect(ctx, c, tokenPath, 15*time.Second); err != nil {
		return "", err
	}
	token, err := cove.GitHubToken(ctx, c)
	if err != nil {
		return "", err
	}
	slog.Info("GitHub token loaded from Cove", "length", len(token))
	return token, nil
}

// watchlistError explains a watchlist that can't be loaded.
func watchlistError(path string, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("the watchlist %s doesn't exist. Create it with an empty list: echo '[]' > %s (in Docker, on the host: /srv/server/storage/lighthouse/repos.json)", path, path)
	case isDirectory(path):
		return fmt.Errorf("the watchlist %s is a directory (Docker creates one when the host file is missing). Remove it and create the file: echo '[]' > <host path>", path)
	default:
		return fmt.Errorf("loading the watchlist: %w", err)
	}
}

func isDirectory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
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
