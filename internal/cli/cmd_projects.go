package cli

import (
	"context"
	"fmt"
)

func (c *CLI) list(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return usageError{form: "list"}
	}

	projects, err := c.svc.Projects(ctx)
	if err != nil {
		return err
	}
	if len(projects) == 0 {
		info("No projects are watched yet. Add one with \"add <name> <url>\" (see \"help setup\").")
		return nil
	}

	rows := [][]string{{"NAME", "REPOSITORY", "COMMIT", "DEPLOYED", "CHECKED"}}
	for _, p := range projects {
		rows = append(rows, []string{p.Name, p.URL, shortSHA(p.Commit), ago(p.LastDeployed), ago(p.LastChecked)})
	}
	table(rows)

	for _, p := range projects {
		if p.Broken {
			out(fmt.Sprintf("\n%s is broken: its latest commit failed %d times. \"report %s\" shows why; \"retry %s\" tries again.", p.Name, p.FailureCount, p.Name, p.Name))
		}
		if p.LastError != "" {
			out(fmt.Sprintf("\n%s, last check %s: %s", p.Name, ago(p.LastErrorAt), p.LastError))
		}
	}
	return nil
}

func (c *CLI) add(ctx context.Context, args []string) error {
	if len(args) != 3 {
		return usageError{form: "add <name> <url>"}
	}

	project, err := c.svc.Add(ctx, args[1], args[2])
	if err != nil {
		return err
	}
	success(fmt.Sprintf("Watching %s (%s). It deploys on the next check; \"scan\" checks now.", project.Name, project.URL))
	info("What the repository needs: \"help setup\".")
	return nil
}

func (c *CLI) remove(ctx context.Context, args []string) error {
	skip, rest := takeYesFlag(args[1:])
	down, rest := takeFlag(rest, "--down")
	if len(rest) != 1 {
		return usageError{form: "remove <name> [--down] [--yes]"}
	}
	name := rest[0]

	question := fmt.Sprintf("Stop watching %q? Its containers keep running.", name)
	if down {
		question = fmt.Sprintf("Stop watching %q, and stop and remove its containers?", name)
	}
	ok, err := c.confirmOrRefuse(question, skip, "remove")
	if !ok || err != nil {
		return err
	}

	if err := c.svc.Remove(ctx, name, down); err != nil {
		return err
	}
	if down {
		success(fmt.Sprintf("Removed %s and its containers.", name))
	} else {
		success(fmt.Sprintf("Stopped watching %s. Its containers keep running; \"remove %s --down\" would have removed them.", name, name))
	}
	return nil
}

func (c *CLI) rename(ctx context.Context, args []string) error {
	if len(args) != 3 {
		return usageError{form: "rename <name> <new-name>"}
	}
	if err := c.svc.Rename(ctx, args[1], args[2]); err != nil {
		return err
	}
	success(fmt.Sprintf("Renamed %s to %s.", args[1], args[2]))
	return nil
}

func (c *CLI) setURL(ctx context.Context, args []string) error {
	if len(args) != 3 {
		return usageError{form: "set-url <name> <url>"}
	}
	if err := c.svc.SetURL(ctx, args[1], args[2]); err != nil {
		return err
	}
	success(fmt.Sprintf("%s now watches %s. It deploys on the next check.", args[1], args[2]))
	return nil
}
