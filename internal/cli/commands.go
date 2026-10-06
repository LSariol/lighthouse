package cli

import (
	"context"
	"fmt"
	"strings"
)

// command is one CLI command. run receives the full argument list, including
// the command name in args[0]. It prints its own results and returns an error
// for anything that went wrong; the caller shows the error.
type command struct {
	names []string // first is the primary name, the rest are aliases

	// group and summary are what `help` shows: one line per command, under
	// its group's heading. synopsis is the arguments shown on that line.
	group    string
	synopsis string
	summary  string

	// What `help <command>` shows, each as aligned columns.
	usages   []usage
	flags    []flag
	examples []example

	run func(c *CLI, ctx context.Context, args []string) error

	// complete returns the Tab-completion candidates for the command's first
	// argument, or is nil when it has none.
	complete func(c *CLI) []string
}

// usage is one way to call a command, e.g. "deploy <name>", and what it does.
type usage struct {
	form string
	help string
}

// flag is an option a command takes, e.g. "--yes".
type flag struct {
	name string
	help string
}

// example is a command line and a short note on what it does.
type example struct {
	line string
	note string
}

// usageError is returned when a command is called with the wrong arguments.
type usageError struct {
	reason string // optional, e.g. `"x" isn't a number.`
	form   string
}

func (e usageError) Error() string {
	if e.reason == "" {
		return "Usage: " + e.form
	}
	return e.reason + " Usage: " + e.form
}

// Command groups, in the order `help` shows them.
const (
	groupProjects   = "Projects"
	groupDeploying  = "Deploying"
	groupContainers = "Containers"
	groupLighthouse = "Lighthouse"
)

var groupOrder = []string{groupProjects, groupDeploying, groupContainers, groupLighthouse}

// commandTable lists every command. Adding a command here makes it available
// at the prompt, as a one-shot command, in help and in Tab completion.
func commandTable() []command {
	yes := flag{"--yes, -y", "Don't ask for confirmation (for scripts)."}
	names := (*CLI).projectNames
	namesOrAll := (*CLI).projectNamesOrAll

	return []command{
		{
			names:    []string{"list", "ls", "l"},
			group:    groupProjects,
			summary:  "Watched projects and their last deploy",
			usages:   []usage{{"list", "List every watched project with its repository, deployed commit, last deploy and last check, and any error from the last check."}},
			examples: []example{{"list", "everything Lighthouse watches"}},
			run:      (*CLI).list,
		},
		{
			names:    []string{"add"},
			group:    groupProjects,
			synopsis: "<name> <url>",
			summary:  "Start watching a GitHub repository",
			usages: []usage{
				{"add <name> <url>", "Watch a repository's main branch. Its first deploy happens on the next check. The repository must follow the project rules: \"help setup\"."},
			},
			examples: []example{
				{"add plop https://github.com/LSariol/plop", "deployed within one check"},
			},
			run: (*CLI).add,
		},
		{
			names:    []string{"remove", "rm"},
			group:    groupProjects,
			synopsis: "<name>",
			summary:  "Stop watching a project",
			usages: []usage{
				{"remove <name>", "Stop watching a project. Asks first. Its container keeps running; stop it first if it should go too."},
			},
			flags: []flag{yes},
			examples: []example{
				{"remove plop", "asks first"},
				{"remove plop --yes", "no question"},
			},
			run:      (*CLI).remove,
			complete: names,
		},
		{
			names:    []string{"rename"},
			group:    groupProjects,
			synopsis: "<name> <new-name>",
			summary:  "Change a project's name in Lighthouse",
			usages: []usage{
				{"rename <name> <new-name>", "Change the name Lighthouse uses for a project. The repository and container are unchanged."},
			},
			examples: []example{{"rename Plop plop", "lowercase it"}},
			run:      (*CLI).rename,
			complete: names,
		},
		{
			names:    []string{"set-url"},
			group:    groupProjects,
			synopsis: "<name> <url>",
			summary:  "Point a project at another repository",
			usages: []usage{
				{"set-url <name> <url>", "Watch a different repository under the same name, e.g. after renaming it on GitHub. Its container name follows the new repository's name."},
			},
			examples: []example{{"set-url plop https://github.com/LSariol/plop-web", "after a rename on GitHub"}},
			run:      (*CLI).setURL,
			complete: names,
		},
		{
			names:    []string{"deploy", "rebuild"},
			group:    groupDeploying,
			synopsis: "<name|all>",
			summary:  "Deploy the latest commit now",
			usages: []usage{
				{"deploy <name>", "Download, build and start a project's latest commit now, even if it hasn't changed. Waits until it's done, which can take a few minutes."},
				{"deploy all", "Deploy every project, one after another. Asks first."},
			},
			flags: []flag{{"--yes, -y", "deploy all: don't ask for confirmation."}},
			examples: []example{
				{"deploy plop", "redeploy after changing one of its secrets"},
				{"deploy all --yes", "everything, no question"},
			},
			run:      (*CLI).deploy,
			complete: namesOrAll,
		},
		{
			names:    []string{"scan"},
			group:    groupDeploying,
			summary:  "Check every project for new commits now",
			usages:   []usage{{"scan", "Check GitHub for every project now instead of waiting, and deploy any with a new commit. Waits until it's done."}},
			examples: []example{{"scan", "after pushing, to deploy right away"}},
			run:      (*CLI).scan,
		},
		{
			names:   []string{"pause"},
			group:   groupDeploying,
			summary: "Stop automatic deploys",
			usages: []usage{
				{"pause", "Stop checking GitHub, so nothing deploys on its own. \"deploy\" and \"scan\" still work. Lasts until \"resume\" or a restart of Lighthouse."},
			},
			run: (*CLI).pause,
		},
		{
			names:   []string{"resume"},
			group:   groupDeploying,
			summary: "Start automatic deploys again",
			usages:  []usage{{"resume", "Check GitHub again on the usual schedule."}},
			run:     (*CLI).resume,
		},
		{
			names:    []string{"start"},
			group:    groupContainers,
			synopsis: "<name|all>",
			summary:  "Start a project's container",
			usages: []usage{
				{"start <name>", "Start a project's stopped container (the version already built)."},
				{"start all", "Start every project's container."},
			},
			examples: []example{{"start plop", "bring it back after \"stop\""}},
			run:      (*CLI).start,
			complete: namesOrAll,
		},
		{
			names:    []string{"stop"},
			group:    groupContainers,
			synopsis: "<name|all>",
			summary:  "Stop a project's container",
			usages: []usage{
				{"stop <name>", "Stop a project's container; it's offline until started or deployed again. Asks first."},
				{"stop all", "Stop every project's container. Asks first."},
			},
			flags: []flag{yes},
			examples: []example{
				{"stop plop", "asks first"},
				{"stop all --yes", "everything, no question"},
			},
			run:      (*CLI).stop,
			complete: namesOrAll,
		},
		{
			names:    []string{"restart"},
			group:    groupContainers,
			synopsis: "<name|all>",
			summary:  "Restart a project's container",
			usages: []usage{
				{"restart <name>", "Restart a project's container (the version already built). For a new commit or changed secrets, use \"deploy\"."},
				{"restart all", "Restart every project's container. Asks first."},
			},
			flags:    []flag{{"--yes, -y", "restart all: don't ask for confirmation."}},
			examples: []example{{"restart plop", "a quick restart"}},
			run:      (*CLI).restart,
			complete: namesOrAll,
		},
		{
			names:    []string{"logs"},
			group:    groupContainers,
			synopsis: "<name> [lines]",
			summary:  "A project's recent output",
			usages: []usage{
				{"logs <name> [lines]", "Show the last lines of a project's container output (50 unless given, up to 10000)."},
			},
			examples: []example{
				{"logs plop", "the last 50 lines"},
				{"logs plop 500", "the last 500"},
			},
			run:      (*CLI).logs,
			complete: names,
		},
		{
			names:   []string{"status"},
			group:   groupLighthouse,
			summary: "Is everything healthy?",
			usages: []usage{
				{"status", "Show Lighthouse's health (version, startup, Cove, GitHub token, automatic deploys) and every project's container state and last error. Exits non-zero if something needs attention."},
			},
			run: (*CLI).status,
		},
		{
			names:    []string{"help", "h"},
			group:    groupLighthouse,
			synopsis: "[command|guide]",
			summary:  "This overview, one command in detail, or a guide",
			usages: []usage{
				{"help", "List every command."},
				{"help <command>", "Show one command in detail, with examples."},
				{"help setup", "What a repository needs before Lighthouse can deploy it."},
				{"help failed", "What to do when a deploy fails."},
			},
			examples: []example{{"help deploy", "everything about deploy"}},
			run:      (*CLI).help,
			complete: (*CLI).helpTopics,
		},
		{
			names:   []string{"exit", "quit"},
			group:   groupLighthouse,
			summary: "Leave the shell (Lighthouse keeps running)",
			usages:  []usage{{"exit", "Leave the shell. Lighthouse keeps running and deploying."}},
			run:     (*CLI).exit,
		},
	}
}

// guides are the step-by-step help topics: `help setup`, `help failed`.
var guides = []struct {
	name, summary, text string
}{
	{"setup", "What a repository needs to be deployed", setupGuide},
	{"failed", "What to do when a deploy fails", failedGuide},
}

const setupGuide = `Getting a repository ready for Lighthouse (e.g. "plop")

1. A docker-compose.yml at the top of the repository, with:
     name: plop                  the repository's name, lowercase
     container_name: plop        on its main service, the same name
     networks: [spark]           external; to reach Cove, sparkdb, others
   Data that must survive a deploy goes in an absolute host path,
   /srv/server/storage/plop/..., never a relative ./folder.

2. Secrets as ${KEY} placeholders, named PROJECT_PLATFORM_TYPE:
     environment:
       - DATABASE_URL=${PLOP_DATABASE_URL}
       - LOG_LEVEL=info          plain settings are written out
   Create each key in Cove first ("create PLOP_DATABASE_URL ..." in the
   Cove shell). Only real secrets go in ${...}: Lighthouse fetches every
   placeholder from Cove, and a missing key fails the deploy.

3. The deployable code on main. Lighthouse deploys every new commit there.

4. Add it:
     add plop https://github.com/LSariol/plop
   Then "status" shows it running within a minute or two, and "logs plop"
   shows its output.

The full rules: DOCUMENTATION.md §7 in the Lighthouse repository.`

const failedGuide = `When a deploy fails

1. See why: "list" shows each project's last error; "docker logs lighthouse"
   (on the server) has the full build output.

2. The project is probably stopped: today Lighthouse stops the old container
   before building the new one. "start <name>" brings the old version back
   while you fix it.

3. Lighthouse retries the deploy on every check until it succeeds. To stop
   that while you work on it: "pause". Afterwards: "resume".

4. Common causes:
     missing: KEY             the secret isn't in Cove, or its name changed
     forbidden_key            Lighthouse's Cove token can't read the key
     docker compose ... failed the Dockerfile or compose file has an error
     no such file             the compose file isn't at the repository's top
     GitHub: 401              Lighthouse's GitHub token expired or was revoked

5. Fix, push to main, then "deploy <name>" (or wait for the next check).`

// help shows the overview, one command in detail, or a guide.
func (c *CLI) help(ctx context.Context, args []string) error {
	switch len(args) {
	case 1:
		out(c.overview())
		return nil
	case 2:
		topic := strings.ToLower(args[1])
		for _, g := range guides {
			if g.name == topic {
				out(g.text)
				return nil
			}
		}
		cmd, ok := c.byName[topic]
		if !ok {
			return usageError{reason: fmt.Sprintf("Unknown command or guide %q.", args[1]), form: "help [command|setup|failed]"}
		}
		out(commandHelp(*cmd))
		return nil
	default:
		return usageError{form: "help [command|setup|failed]"}
	}
}

// overview lists every command on one line, by group, then the guides.
func (c *CLI) overview() string {
	var b strings.Builder

	type line struct{ left, right string }
	var groups [][]line
	width := 0
	for _, group := range groupOrder {
		var lines []line
		for _, cmd := range c.commands {
			if cmd.group != group {
				continue
			}
			left := strings.TrimSpace(cmd.names[0] + " " + cmd.synopsis)
			lines = append(lines, line{left, cmd.summary})
			width = max(width, len(left))
		}
		groups = append(groups, lines)
	}
	for _, g := range guides {
		width = max(width, len("help "+g.name))
	}

	for i, group := range groupOrder {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(group + ":\n")
		for _, l := range groups[i] {
			fmt.Fprintf(&b, "  %-*s  %s\n", width, l.left, l.right)
		}
	}

	b.WriteString("\nGuides:\n")
	for _, g := range guides {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, "help "+g.name, g.summary)
	}

	b.WriteString("\nType \"help <command>\" for details and examples, e.g. \"help deploy\".")
	return b.String()
}

// helpWidth is the widest line help text wraps to.
const helpWidth = 80

// commandHelp is `help <command>`: a header, then usage, flags and examples.
// Usage and flags share one description column; examples have their own.
func commandHelp(cmd command) string {
	var b strings.Builder

	header := cmd.names[0]
	if len(cmd.names) > 1 {
		header += " (" + strings.Join(cmd.names[1:], ", ") + ")"
	}
	b.WriteString(header + ": " + cmd.summary + "\n")

	var usages, flags, examples [][2]string
	for _, u := range cmd.usages {
		usages = append(usages, [2]string{u.form, u.help})
	}
	for _, f := range cmd.flags {
		flags = append(flags, [2]string{f.name, f.help})
	}
	for _, e := range cmd.examples {
		examples = append(examples, [2]string{e.line, e.note})
	}

	width := columnWidth(append(append([][2]string{}, usages...), flags...), 30)
	b.WriteString("\nUsage:\n" + columns(usages, width))
	if len(flags) > 0 {
		b.WriteString("\nFlags:\n" + columns(flags, width))
	}
	if len(examples) > 0 {
		b.WriteString("\nExamples:\n" + columns(examples, columnWidth(examples, 46)))
	}
	return strings.TrimRight(b.String(), "\n")
}

// columnWidth is the width of the left column for rows: the longest left
// side, not counting those longer than maxLeft (they get their own line).
func columnWidth(rows [][2]string, maxLeft int) int {
	width := 0
	for _, r := range rows {
		if len(r[0]) <= maxLeft {
			width = max(width, len(r[0]))
		}
	}
	return width
}

// columns lays out rows as two columns: the left one indented by two spaces
// and width wide, the right one wrapped to helpWidth. A left side wider than
// width gets its own line, with its text below it in the right column.
func columns(rows [][2]string, width int) string {
	col := 2 + width + 3
	pad := strings.Repeat(" ", col)

	var b strings.Builder
	for _, r := range rows {
		lines := wrap(r[1], helpWidth-col)
		if len(r[0]) > width {
			b.WriteString("  " + r[0] + "\n")
			for _, l := range lines {
				b.WriteString(pad + l + "\n")
			}
			continue
		}
		for i, l := range lines {
			left := ""
			if i == 0 {
				left = r[0]
			}
			fmt.Fprintf(&b, "  %-*s   %s\n", width, left, l)
		}
	}
	return b.String()
}

// wrap splits text into lines of at most width characters, at spaces.
func wrap(text string, width int) []string {
	var lines []string
	line := ""
	for _, word := range strings.Fields(text) {
		switch {
		case line == "":
			line = word
		case len(line)+1+len(word) <= width:
			line += " " + word
		default:
			lines = append(lines, line)
			line = word
		}
	}
	return append(lines, line)
}

// helpTopics completes `help <Tab>`: command names and guides.
func (c *CLI) helpTopics() []string {
	topics := c.commandNames()
	for _, g := range guides {
		topics = append(topics, g.name)
	}
	return topics
}

func (c *CLI) exit(ctx context.Context, args []string) error {
	if c.leave != nil {
		c.leave()
	}
	return nil
}
