package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/lsariol/lighthouse/internal/control"
)

func (c *CLI) deploy(ctx context.Context, args []string) error {
	const form = "deploy <name> [tag|commit] | deploy all [--yes]"
	skip, rest := takeYesFlag(args[1:])
	if len(rest) < 1 || len(rest) > 2 || len(rest) == 2 && strings.EqualFold(rest[0], "all") {
		return usageError{form: form}
	}

	if strings.EqualFold(rest[0], "all") {
		ok, err := c.confirmOrRefuse("Deploy every project, one after another?", skip, "deploy all")
		if !ok || err != nil {
			return err
		}
		return c.eachProject(ctx, "deploy", "Deployed", c.deployOne)
	}

	version := ""
	if len(rest) == 2 {
		version = rest[1]
	}
	if err := c.deployVersion(ctx, rest[0], version); err != nil {
		if handedOff(err) {
			return nil
		}
		return err
	}
	success(fmt.Sprintf("Deployed %q.", rest[0]))
	return nil
}

// handedOff shows a self-update's hand-off as the success it is.
func handedOff(err error) bool {
	var ce *control.Error
	if errors.As(err, &ce) && ce.Kind == control.KindHandedOff {
		success(ce.Message)
		return true
	}
	return false
}

func (c *CLI) deployOne(ctx context.Context, name string) error {
	return c.deployVersion(ctx, name, "")
}

func (c *CLI) deployVersion(ctx context.Context, name string, version string) error {
	what := fmt.Sprintf("%q", name)
	if version != "" {
		what += " at " + version
	}
	info(fmt.Sprintf("Deploying %s. This can take a few minutes; \"docker logs -f lighthouse\" shows progress.", what))
	return c.svc.Deploy(ctx, name, version)
}

func (c *CLI) rollback(ctx context.Context, args []string) error {
	if len(args) != 2 {
		return usageError{form: "rollback <name>"}
	}
	info(fmt.Sprintf("Rolling %q back to what ran before. This can take a few minutes; \"docker logs -f lighthouse\" shows progress.", args[1]))
	to, err := c.svc.Rollback(ctx, args[1])
	if err != nil {
		return err
	}
	success(fmt.Sprintf("Rolled %q back to %s. Checks won't deploy what it went back from; a newer commit or release will, and so will \"deploy %s\".", args[1], to, args[1]))
	return nil
}

func (c *CLI) retry(ctx context.Context, args []string) error {
	if len(args) != 2 {
		return usageError{form: "retry <name>"}
	}
	info(fmt.Sprintf("Clearing %q's failures and deploying it. This can take a few minutes; \"docker logs -f lighthouse\" shows progress.", args[1]))
	if err := c.svc.Retry(ctx, args[1]); err != nil {
		return err
	}
	success(fmt.Sprintf("Deployed %q.", args[1]))
	return nil
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

// pause and resume act on every project: "pause all" reads the same.
func (c *CLI) pause(ctx context.Context, args []string) error {
	if !allOrNothing(args) {
		return usageError{form: "pause [all]"}
	}
	if err := c.svc.Pause(ctx); err != nil {
		return err
	}
	success("Automatic deploys paused. \"resume\" turns them back on; a restart of Lighthouse does too.")
	return nil
}

func (c *CLI) resume(ctx context.Context, args []string) error {
	if !allOrNothing(args) {
		return usageError{form: "resume [all]"}
	}
	if err := c.svc.Resume(ctx); err != nil {
		return err
	}
	success("Automatic deploys resumed.")
	return nil
}

// allOrNothing reports whether a command got no argument, or just "all".
func allOrNothing(args []string) bool {
	return len(args) == 1 || len(args) == 2 && strings.EqualFold(args[1], "all")
}

// eachProject runs fn for every watched project, reporting each one, and
// fails if any failed.
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
		success(fmt.Sprintf("%s %q.", done, p.Name))
	}

	if failed > 0 {
		return fmt.Errorf("%d of %d projects failed to %s.", failed, len(projects), verb)
	}
	return nil
}

const defaultHistory = 10

func (c *CLI) history(ctx context.Context, args []string) error {
	const form = "history <name> [count]"
	if len(args) < 2 || len(args) > 3 {
		return usageError{form: form}
	}

	count := defaultHistory
	if len(args) == 3 {
		n, err := strconv.Atoi(args[2])
		if err != nil || n < 1 {
			return usageError{reason: fmt.Sprintf("%q isn't a positive number.", args[2]), form: form}
		}
		count = n
	}

	deploys, err := c.svc.History(ctx, args[1], count)
	if err != nil {
		return err
	}
	if len(deploys) == 0 {
		info(fmt.Sprintf("%q hasn't been deployed by this Lighthouse yet.", args[1]))
		return nil
	}

	rows := [][]string{{"#", "WHEN", "TRIGGER", "RESULT", "COMMIT", "TOOK", "ERROR"}}
	for i, d := range deploys {
		started := d.StartedAt
		rows = append(rows, []string{
			strconv.Itoa(i + 1), formatTime(&started), d.Trigger, result(d), deployed(d.Version, d.Commit),
			took(d.StartedAt, d.FinishedAt), firstLine(d.Error, 50),
		})
	}
	table(rows)
	info(fmt.Sprintf("%d %s. \"report %s <#>\" shows one step by step.", len(deploys), plural(len(deploys), "deploy", "deploys"), args[1]))
	return nil
}

func (c *CLI) report(ctx context.Context, args []string) error {
	const form = "report <name> [n]"
	if len(args) < 2 || len(args) > 3 {
		return usageError{form: form}
	}
	n := 1
	if len(args) == 3 {
		v, err := strconv.Atoi(args[2])
		if err != nil || v < 1 {
			return usageError{reason: fmt.Sprintf("%q isn't a deploy number (1 is the latest).", args[2]), form: form}
		}
		n = v
	}

	d, err := c.svc.Report(ctx, args[1], n)
	if err != nil {
		return err
	}
	printDeployment(d)
	return nil
}

func (c *CLI) check(ctx context.Context, args []string) error {
	if len(args) != 2 {
		return usageError{form: "check <name>"}
	}
	name := args[1]
	info(fmt.Sprintf("Checking %q: downloading its latest commit and running the deploy's checks. Nothing is deployed.", name))
	d, err := c.svc.Check(ctx, name)
	if err != nil {
		return err
	}
	printDeployment(d)
	if d.Status != "succeeded" {
		return fmt.Errorf("%q wouldn't deploy: it fails at %s. The step's output above says why; \"help rules\" lists the rules.", name, d.FailedStep)
	}
	success(fmt.Sprintf("%q passes the checks: its latest commit would deploy.", name))
	return nil
}

// printDeployment shows a deploy (or a check) and each step's output.
func printDeployment(d control.Deployment) {
	started := d.StartedAt
	fields([][2]string{
		{"Commit", orDash(strings.TrimSpace(d.Version + " " + d.Commit))},
		{"Started", formatTime(&started) + " (" + d.Trigger + ")"},
		{"Took", took(d.StartedAt, d.FinishedAt)},
		{"Result", result(d) + kindNote(d)},
	})
	if d.Error != "" {
		out("")
		out(d.Error)
	}
	for _, st := range d.Steps {
		out("")
		out(fmt.Sprintf("== %s: %s (%s)", st.Name, st.Status, took(st.StartedAt, st.FinishedAt)))
		if log := strings.TrimRight(st.Log, "\n"); log != "" {
			out(log)
		}
	}
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

// kindNote says what a failure means for the project; a dry run (`check`)
// records nothing, so it means nothing.
func kindNote(d control.Deployment) string {
	if d.Trigger == "dry run" {
		return ""
	}
	switch d.FailureKind {
	case "transient":
		return " (a passing problem: tried again next check)"
	case "permanent":
		return " (a problem with the commit: counts toward broken)"
	}
	return ""
}

// took is how long something took, to the second (or the millisecond, under
// a second).
func took(start time.Time, end time.Time) string {
	d := end.Sub(start)
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(time.Second).String()
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
