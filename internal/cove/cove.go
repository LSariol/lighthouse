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

// GitHubTokenKey is the Cove key holding Lighthouse's GitHub token.
const GitHubTokenKey = "LIGHTHOUSE_GITHUB_TOKEN"

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

// GitHubToken reads Lighthouse's GitHub token from Cove.
func GitHubToken(ctx context.Context, c *coveclient.Client) (string, error) {
	token, err := c.GetSecretContext(ctx, GitHubTokenKey)
	switch {
	case errors.Is(err, coveclient.ErrNotFound):
		return "", fmt.Errorf("%s isn't in Cove. Create it in the Cove shell (\"create %s <token>\") and restart Lighthouse", GitHubTokenKey, GitHubTokenKey)
	case err != nil:
		return "", fmt.Errorf("reading %s from Cove: %w", GitHubTokenKey, err)
	}
	return token, nil
}
