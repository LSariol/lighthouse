package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/LSariol/LightHouse/internal/control"
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

const defaultHistory = 10

func (c *CLI) history(ctx context.Context, args []string) error {
	if len(args) < 2 || len(args) > 3 {
		return usageError{form: "history <name> [count]"}
	}

	count := defaultHistory
	if len(args) == 3 {
		n, err := strconv.Atoi(args[2])
		if err != nil || n < 1 {
			return usageError{reason: fmt.Sprintf("%q isn't a positive number.", args[2]), form: "history <name> [count]"}
		}
		count = n
	}

	deploys, err := c.svc.History(ctx, args[1], count)
	if err != nil {
		return err
	}
	if len(deploys) == 0 {
		info(fmt.Sprintf("%s hasn't been deployed by this Lighthouse yet.", args[1]))
		return nil
	}

	rows := [][]string{{"#", "WHEN", "TRIGGER", "RESULT", "COMMIT", "TOOK", "ERROR"}}
	for i, d := range deploys {
		started := d.StartedAt
		rows = append(rows, []string{
			strconv.Itoa(i + 1), ago(&started), d.Trigger, result(d), shortSHA(d.Commit),
			d.FinishedAt.Sub(d.StartedAt).Round(time.Second).String(), firstLine(d.Error, 50),
		})
	}
	table(rows)
	info(fmt.Sprintf("\"report %s <#>\" shows one in detail.", args[1]))
	return nil
}

// firstLine is the first line of s, cut to max characters, or "-".
func firstLine(s string, max int) string {
	if s == "" {
		return "-"
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if r := []rune(s); len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	return s
}

// result describes how a deploy went: "succeeded", "failed at build",
// "rolled back at verify".
func result(d control.Deployment) string {
	switch d.Status {
	case "succeeded":
		return "succeeded"
	case "rolled_back":
		return "rolled back at " + d.FailedStep
	default:
		if d.FailedStep != "" {
			return "failed at " + d.FailedStep
		}
		return d.Status
	}
}

func (c *CLI) retry(ctx context.Context, args []string) error {
	if len(args) != 2 {
		return usageError{form: "retry <name>"}
	}
	info(fmt.Sprintf("Clearing %s's failures and deploying it. This can take a few minutes; \"docker logs -f lighthouse\" shows progress.", args[1]))
	if err := c.svc.Retry(ctx, args[1]); err != nil {
		return err
	}
	success(fmt.Sprintf("Deployed %s.", args[1]))
	return nil
}

func (c *CLI) report(ctx context.Context, args []string) error {
	if len(args) < 2 || len(args) > 3 {
		return usageError{form: "report <name> [n]"}
	}
	n := 1
	if len(args) == 3 {
		v, err := strconv.Atoi(args[2])
		if err != nil || v < 1 {
			return usageError{reason: fmt.Sprintf("%q isn't a deploy number (1 is the latest).", args[2]), form: "report <name> [n]"}
		}
		n = v
	}

	d, err := c.svc.Report(ctx, args[1], n)
	if err != nil {
		return err
	}

	started := d.StartedAt
	table([][]string{
		{"Commit", orDash(d.Commit)},
		{"Started", started.Local().Format("2006-01-02 15:04:05") + " (" + ago(&started) + "), by " + d.Trigger},
		{"Result", result(d) + kindNote(d.FailureKind)},
		{"Took", d.FinishedAt.Sub(d.StartedAt).Round(time.Second).String()},
	})
	if d.Error != "" {
		out("")
		out(d.Error)
	}
	for _, st := range d.Steps {
		out("")
		out(fmt.Sprintf("== %s: %s (%s)", st.Name, st.Status, st.FinishedAt.Sub(st.StartedAt).Round(time.Millisecond)))
		if log := strings.TrimRight(st.Log, "\n"); log != "" {
			out(log)
		}
	}
	return nil
}

func kindNote(kind string) string {
	switch kind {
	case "transient":
		return " (a passing problem: tried again next check)"
	case "permanent":
		return " (a problem with the commit: counts toward broken)"
	}
	return ""
}
