package watcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// latestSHA returns the newest commit on the repository's default branch.
func latestSHA(ctx context.Context, hc *http.Client, apiURL string, token string) (string, error) {
	if token == "" {
		return "", errors.New("no GitHub token yet (Lighthouse is still waiting for Cove)")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"/commits?per_page=1", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("GitHub: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub: %s", resp.Status)
	}

	var commits []struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&commits); err != nil {
		return "", fmt.Errorf("GitHub: unexpected response: %v", err)
	}
	if len(commits) == 0 || commits[0].SHA == "" {
		return "", errors.New("GitHub: the repository has no commits")
	}
	return commits[0].SHA, nil
}
