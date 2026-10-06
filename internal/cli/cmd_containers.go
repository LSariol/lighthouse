package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

func (c *CLI) start(ctx context.Context, args []string) error {
	if len(args) != 2 {
		return usageError{form: "start <name[:service]|all>"}
	}
	if strings.EqualFold(args[1], "all") {
		return c.eachProject(ctx, "start", "Started", c.svc.Start)
	}
	if err := c.svc.Start(ctx, args[1]); err != nil {
		return err
	}
	success(fmt.Sprintf("Started %q.", args[1]))
	return nil
}

func (c *CLI) stop(ctx context.Context, args []string) error {
	skip, rest := takeYesFlag(args[1:])
	if len(rest) != 1 {
		return usageError{form: "stop <name[:service]|all> [--yes]"}
	}

	if strings.EqualFold(rest[0], "all") {
		ok, err := c.confirmOrRefuse("Stop every project? They stay offline until started or deployed again.", skip, "stop all")
		if !ok || err != nil {
			return err
		}
		return c.eachProject(ctx, "stop", "Stopped", c.svc.Stop)
	}

	ok, err := c.confirmOrRefuse(fmt.Sprintf("Stop %q? It stays offline until started or deployed again.", rest[0]), skip, "stop")
	if !ok || err != nil {
		return err
	}
	if err := c.svc.Stop(ctx, rest[0]); err != nil {
		return err
	}
	success(fmt.Sprintf("Stopped %q. \"start %s\" brings it back.", rest[0], rest[0]))
	return nil
}

func (c *CLI) restart(ctx context.Context, args []string) error {
	skip, rest := takeYesFlag(args[1:])
	if len(rest) != 1 {
		return usageError{form: "restart <name[:service]|all> [--yes]"}
	}

	if strings.EqualFold(rest[0], "all") {
		ok, err := c.confirmOrRefuse("Restart every project?", skip, "restart all")
		if !ok || err != nil {
			return err
		}
		return c.eachProject(ctx, "restart", "Restarted", c.svc.Restart)
	}

	if err := c.svc.Restart(ctx, rest[0]); err != nil {
		return err
	}
	success(fmt.Sprintf("Restarted %q.", rest[0]))
	return nil
}

const defaultLogLines = 50

func (c *CLI) logs(ctx context.Context, args []string) error {
	const form = "logs <name[:service]> [lines]"
	if len(args) < 2 || len(args) > 3 {
		return usageError{form: form}
	}

	lines := defaultLogLines
	if len(args) == 3 {
		n, err := strconv.Atoi(args[2])
		if err != nil || n < 1 {
			return usageError{reason: fmt.Sprintf("%q isn't a positive number of lines.", args[2]), form: form}
		}
		lines = n
	}

	output, err := c.svc.Logs(ctx, args[1], lines)
	if err != nil {
		return err
	}
	if output == "" {
		info(fmt.Sprintf("%q hasn't written any output.", args[1]))
		return nil
	}
	fmt.Fprint(stdout, output)
	if !strings.HasSuffix(output, "\n") {
		fmt.Fprintln(stdout)
	}
	return nil
}
