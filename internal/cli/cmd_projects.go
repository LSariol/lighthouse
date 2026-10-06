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
	keep, rest := takeFlag(rest, "--keep")
	if len(rest) != 1 {
		return usageError{form: "remove <name> [--keep] [--yes]"}
	}
	name := rest[0]

	// A project Lighthouse no longer watches would run on untracked, so its
	// containers go too unless --keep says otherwise.
	question := fmt.Sprintf("Stop watching %q, and stop and remove its containers?", name)
	if keep {
		question = fmt.Sprintf("Stop watching %q? Its containers keep running.", name)
	}
	ok, err := c.confirmOrRefuse(question, skip, "remove")
	if !ok || err != nil {
		return err
	}

	if err := c.svc.Remove(ctx, name, !keep); err != nil {
		return err
	}
	if keep {
		success(fmt.Sprintf("Stopped watching %s. Its containers keep running, untracked.", name))
	} else {
		success(fmt.Sprintf("Removed %s and its containers.", name))
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
