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
	info(fmt.Sprintf("Its compose file must name the container %q: see \"help setup\".", project.Container))
	return nil
}

func (c *CLI) remove(ctx context.Context, args []string) error {
	skip, rest := takeYesFlag(args[1:])
	if len(rest) != 1 {
		return usageError{form: "remove <name> [--yes]"}
	}
	name := rest[0]

	ok, err := c.confirmOrRefuse(fmt.Sprintf("Stop watching %q? Its container keeps running.", name), skip, "remove")
	if !ok || err != nil {
		return err
	}

	if err := c.svc.Remove(ctx, name); err != nil {
		return err
	}
	success(fmt.Sprintf("Stopped watching %s. Its container is still running: \"docker compose -p %s down\" on the server removes it.", name, name))
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
