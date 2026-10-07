package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// status prints Lighthouse's health and each project's state; it fails when something needs attention.
func (c *CLI) status(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return usageError{form: "status"}
	}

	s, err := c.svc.Status(ctx)
	if err != nil {
		return err
	}

	var problems []string

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

	switch {
	case s.Database == "unreachable":
		problems = append(problems, "the database is unreachable")
	case strings.Contains(s.Schema, "needs"):
		problems = append(problems, "migrations are missing")
	}

	up := time.Since(s.StartedAt).Round(time.Second)
	rows := [][2]string{
		{"Version", orDash(s.Version)},
		{"Environment", orDash(s.Env)},
		{"State", s.Phase + ", up " + up.String()},
		{"Automatic deploys", deploys},
		{"Cove", orDash(s.CoveURL)},
		{"GitHub token", token},
		{"Database", orDash(s.Database)},
	}
	if s.Schema != "" {
		rows = append(rows, [2]string{"Schema", s.Schema})
	}
	fields(rows)

	switch {
	case s.Phase != "running":
		out("")
		info("Projects are shown once Lighthouse is running.")
	case len(s.Projects) == 0:
		out("")
		info("No projects are watched yet. Add one with \"add <url>\".")
	default:
		out("")
		rows := [][]string{{"PROJECT", "SERVICE", "STATE", "RUNNING", "DEPLOYED", "LAST CHECK"}}
		for _, p := range s.Projects {
			check := "ok"
			switch {
			case p.Broken:
				check = "broken"
				problems = append(problems, fmt.Sprintf("%q is broken (\"report %s\", then \"retry %s\"): %s", p.Name, p.Name, p.Name, strings.TrimSuffix(firstLine(p.LastError, 200), ".")))
			case p.LastError != "":
				check = "failed " + formatTime(p.LastErrorAt)
				problems = append(problems, fmt.Sprintf("%q: %s", p.Name, strings.TrimSuffix(firstLine(p.LastError, 200), ".")))
			case p.LastChecked == nil:
				check = "-"
			}
			state := p.State
			switch {
			case p.Stopped:
				state = "stopped on purpose"
			case p.State != "running":
				problems = append(problems, fmt.Sprintf("%q is %s", p.Name, p.State))
			}
			rows = append(rows, []string{p.Name, "", state, deployed(p.Version, p.Commit), formatTime(p.LastDeployed), check})
			for _, svc := range p.Services {
				state := svc.State
				if svc.Health != "" {
					state += ", " + svc.Health
				}
				rows = append(rows, []string{"", svc.Name, state, "", "", ""})
			}
		}
		table(rows)
	}

	if len(problems) > 0 {
		return errors.New("Needs attention: " + strings.Join(problems, "; ") + ".")
	}
	return nil
}
