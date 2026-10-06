package cli

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// status prints Lighthouse's health and every project's state, and returns an
// error (so a one-shot `lighthouse status` exits non-zero) when something
// needs attention.
func (c *CLI) status(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return usageError{form: "status"}
	}

	s, err := c.svc.Status(ctx)
	if err != nil {
		return err
	}

	var problems []string

	phase := s.Phase
	if s.Phase != "running" {
		problems = append(problems, "Lighthouse is "+s.Phase)
	}

	deploys := fmt.Sprintf("on, checking every %s", s.PollInterval)
	if s.Paused {
		deploys = "paused (\"resume\" turns them back on)"
	}

	token := "loaded"
	if !s.GitHubToken {
		token = "not loaded"
		if s.Phase == "running" {
			problems = append(problems, "no GitHub token")
		}
	}

	up := time.Since(s.StartedAt).Round(time.Second)
	table([][]string{
		{"Version", orDash(s.Version)},
		{"Environment", orDash(s.Env)},
		{"State", phase + ", up " + up.String()},
		{"Automatic deploys", deploys},
		{"Cove", orDash(s.CoveURL)},
		{"GitHub token", token},
	})

	if len(s.Projects) == 0 {
		out("")
		info("No projects are watched yet. Add one with \"add <name> <url>\".")
	} else {
		out("")
		rows := [][]string{{"PROJECT", "CONTAINER", "STATE", "COMMIT", "DEPLOYED", "LAST CHECK"}}
		for _, p := range s.Projects {
			check := "ok"
			if p.LastError != "" {
				check = "failed " + ago(p.LastErrorAt)
				problems = append(problems, fmt.Sprintf("%s: %s", p.Name, p.LastError))
			}
			if p.LastChecked == nil {
				check = "-"
			}
			if p.State != "running" {
				problems = append(problems, fmt.Sprintf("%s's container is %s", p.Name, p.State))
			}
			rows = append(rows, []string{p.Name, p.Container, p.State, shortSHA(p.Commit), ago(p.LastDeployed), check})
		}
		table(rows)
	}

	if len(problems) > 0 {
		noun := "things need"
		if len(problems) == 1 {
			noun = "thing needs"
		}
		return fmt.Errorf("%d %s attention:\n  %s", len(problems), noun, strings.Join(problems, "\n  "))
	}
	out("")
	success("Everything is healthy.")
	return nil
}
