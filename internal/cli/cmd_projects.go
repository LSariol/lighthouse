package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/lsariol/lighthouse/internal/control"
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
		info("No projects are watched yet. Add one with \"add <url>\" (see \"help setup\").")
		return nil
	}

	rows := [][]string{{"NAME", "REPOSITORY", "DEPLOYS", "RUNNING", "DEPLOYED", "CHECKED"}}
	for _, p := range projects {
		running := deployed(p.Version, p.Commit)
		if p.Stopped {
			running += ", stopped"
		}
		rows = append(rows, []string{p.Name, p.URL, follows(p.Mode, p.Tier), running, formatTime(p.LastDeployed), formatTime(p.LastChecked)})
	}
	table(rows)
	info(fmt.Sprintf("%d %s", len(projects), plural(len(projects), "project", "projects")))

	for _, p := range projects {
		if p.Broken {
			warn(fmt.Sprintf("%q is broken: its latest commit failed %d times. \"report %s\" shows why; \"retry %s\" tries again.", p.Name, p.FailureCount, p.Name, p.Name))
		}
		if p.LastError != "" {
			warn(fmt.Sprintf("%q, last check %s: %s", p.Name, formatTime(p.LastErrorAt), p.LastError))
		}
	}
	return nil
}

func (c *CLI) add(ctx context.Context, args []string) error {
	const form = "add <url> [--name <name>]"
	name, rest, err := takeValue(args[1:], "--name")
	if err != nil || len(rest) != 1 {
		return usageError{form: form}
	}
	url := rest[0]

	project, err := c.svc.Add(ctx, name, url)

	// The repository's name is taken by another project: on a terminal, offer
	// to pick another one (the server explains which); otherwise just say so.
	var ce *control.Error
	for name == "" && errors.As(err, &ce) && ce.Kind == control.KindNameTaken {
		if c.term == nil && !c.interactive {
			return err
		}
		warn(err.Error())
		answer, answered := c.askLine("Name for this project (Enter cancels):")
		if !answered || answer == "" {
			info("Add cancelled.")
			return nil
		}
		project, err = c.svc.Add(ctx, answer, url)
	}
	if err != nil {
		return err
	}

	success(fmt.Sprintf("Watching %q (%s). It deploys on the next check; \"scan\" checks now.", project.Name, project.URL))
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
		success(fmt.Sprintf("Stopped watching %q. Its containers keep running, untracked.", name))
	} else {
		success(fmt.Sprintf("Removed %q and its containers.", name))
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
	success(fmt.Sprintf("Renamed %q to %q.", args[1], args[2]))
	return nil
}

func (c *CLI) setURL(ctx context.Context, args []string) error {
	if len(args) != 3 {
		return usageError{form: "set-url <name> <url>"}
	}
	if err := c.svc.SetURL(ctx, args[1], args[2]); err != nil {
		return err
	}
	success(fmt.Sprintf("%q now watches %s. It deploys on the next check.", args[1], args[2]))
	return nil
}
