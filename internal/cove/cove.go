// Package cove connects Lighthouse to Cove, the server's secret vault, at
// startup: it gets Lighthouse's own token (through Cove's bootstrap endpoint
// the first time), waits until Cove is ready, and reads Lighthouse's own
// secrets. Deploys fetch projects' secrets with the same client.
package cove

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/lsariol/coveclient"
)

// The Cove keys Lighthouse reads for itself at startup.
const (
	GitHubTokenKey         = "LIGHTHOUSE_GITHUB_TOKEN"
	DatabaseURLKey         = "LIGHTHOUSE_DATABASE_URL"          // lighthouse_app
	MigratorDatabaseURLKey = "LIGHTHOUSE_MIGRATOR_DATABASE_URL" // lighthouse_migrator
)

// Secrets are Lighthouse's own secrets.
type Secrets struct {
	GitHubToken         string
	DatabaseURL         string
	MigratorDatabaseURL string
}

// Connect makes c ready to use. It loads Lighthouse's token from tokenPath,
// or fetches it through Cove's bootstrap endpoint and saves it there. While
// the endpoint is closed or Cove is unreachable, it logs why and tries again
// every retry until ctx is cancelled. Then it waits until Cove is ready and
// checks the token.
func Connect(ctx context.Context, c *coveclient.Client, tokenPath string, retry time.Duration) error {
	for {
		_, err := c.LoadOrBootstrap(tokenPath)
		if err == nil {
			break
		}

		var urlErr *url.Error
		switch {
		case errors.Is(err, coveclient.ErrBootstrapClosed):
			slog.Warn("waiting for a Cove token: run \"bootstrap open lighthouse\" in the Cove shell", "reason", err)
		case errors.As(err, &urlErr):
			slog.Warn("waiting for Cove to be reachable", "url", c.BaseURL, "err", err)
		default:
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retry):
		}
	}

	slog.Info("waiting for Cove to be ready", "url", c.BaseURL)
	if err := c.WaitForReady(ctx); err != nil {
		return err
	}

	if err := c.AuthContext(ctx); err != nil {
		if errors.Is(err, coveclient.ErrUnauthorized) {
			return fmt.Errorf("Cove rejected Lighthouse's token (it was rotated or revoked). Delete %s, run \"bootstrap open lighthouse\" in the Cove shell, and restart Lighthouse", tokenPath)
		}
		return fmt.Errorf("checking Lighthouse's Cove token: %w", err)
	}
	return nil
}

// ReadSecrets reads Lighthouse's own secrets from Cove in one request. If any
// is missing, the error names every missing key.
func ReadSecrets(ctx context.Context, c *coveclient.Client) (Secrets, error) {
	values, err := c.GetSecretsContext(ctx, GitHubTokenKey, DatabaseURLKey, MigratorDatabaseURLKey)
	switch {
	case errors.Is(err, coveclient.ErrNotFound):
		return Secrets{}, fmt.Errorf("Lighthouse's secrets aren't all in Cove (%v). Create them in the Cove shell (DOCUMENTATION.md §10.1) and restart Lighthouse", err)
	case err != nil:
		return Secrets{}, fmt.Errorf("reading Lighthouse's secrets from Cove: %w", err)
	}
	return Secrets{
		GitHubToken:         values[GitHubTokenKey],
		DatabaseURL:         values[DatabaseURLKey],
		MigratorDatabaseURL: values[MigratorDatabaseURLKey],
	}, nil
}
