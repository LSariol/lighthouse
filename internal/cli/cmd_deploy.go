package cli

import (
	"context"
	"fmt"
	"strings"
)

func (c *CLI) deploy(ctx context.Context, args []string) error {
	skip, rest := takeYesFlag(args[1:])
	if len(rest) != 1 {
		return usageError{form: "deploy <name|all> [--yes]"}
	}

	if strings.EqualFold(rest[0], "all") {
		ok, err := c.confirmOrRefuse("Deploy every project, one after another?", skip, "deploy all")
		if !ok || err != nil {
			return err
		}
		return c.eachProject(ctx, "deploy", "Deployed", c.deployOne)
	}

	if err := c.deployOne(ctx, rest[0]); err != nil {
		return err
	}
	success(fmt.Sprintf("Deployed %s.", rest[0]))
	return nil
}

func (c *CLI) deployOne(ctx context.Context, name string) error {
	info(fmt.Sprintf("Deploying %s. This can take a few minutes; \"docker logs -f lighthouse\" shows progress.", name))
	return c.svc.Deploy(ctx, name)
}

func (c *CLI) scan(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return usageError{form: "scan"}
	}
	info("Checking every project. New commits are deployed, which can take a few minutes.")
	if err := c.svc.Scan(ctx); err != nil {
		return err
	}
	success("Scan finished.")
	return nil
}

func (c *CLI) pause(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return usageError{form: "pause"}
	}
	if err := c.svc.Pause(ctx); err != nil {
		return err
	}
	success("Automatic deploys paused. \"resume\" turns them back on; a restart of Lighthouse does too.")
	return nil
}

func (c *CLI) resume(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return usageError{form: "resume"}
	}
	if err := c.svc.Resume(ctx); err != nil {
		return err
	}
	success("Automatic deploys resumed.")
	return nil
}

// eachProject runs fn for every watched project, reporting each one, and
// fails if any failed. verb ("deploy") and done ("Deployed") word the messages.
func (c *CLI) eachProject(ctx context.Context, verb string, done string, fn func(context.Context, string) error) error {
	projects, err := c.svc.Projects(ctx)
	if err != nil {
		return err
	}
	if len(projects) == 0 {
		info("No projects are watched yet.")
		return nil
	}

	failed := 0
	for _, p := range projects {
		if err := fn(ctx, p.Name); err != nil {
			fail(err.Error())
			failed++
			continue
		}
		success(fmt.Sprintf("%s %s.", done, p.Name))
	}

	if failed > 0 {
		return fmt.Errorf("%d of %d projects failed to %s.", failed, len(projects), verb)
	}
	return nil
}
