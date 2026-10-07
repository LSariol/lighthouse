package cli

import (
	"context"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"
)

// runTerminal runs the prompt with line editing: arrow keys, history
// (up/down) and Tab completion of command and project names.
func (c *CLI) runTerminal(ctx context.Context) bool {
	fd := int(os.Stdin.Fd())

	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return false
	}
	defer term.Restore(fd, oldState)

	c.term = term.NewTerminal(struct {
		io.Reader
		io.Writer
	}{os.Stdin, os.Stderr}, c.prompt)
	c.term.AutoCompleteCallback = c.complete

	oldOut, oldErr := stdout, stderr
	stdout, stderr = c.term, c.term
	defer func() { stdout, stderr = oldOut, oldErr }()

	for ctx.Err() == nil {
		line, err := c.term.ReadLine()
		if err != nil {
			return true
		}

		args := strings.Fields(line)
		if len(args) == 0 {
			continue
		}
		report(c.Exec(ctx, args))
	}
	return true
}

// complete handles Tab.
func (c *CLI) complete(line string, pos int, key rune) (string, int, bool) {
	if key != '\t' {
		return "", 0, false
	}

	head, tail := line[:pos], line[pos:]
	start := strings.LastIndex(head, " ") + 1
	word := head[start:]
	before := strings.Fields(head[:start])

	var candidates []string
	switch len(before) {
	case 0:
		candidates = c.commandNames()
	case 1:
		cmd, ok := c.byName[strings.ToLower(before[0])]
		if !ok || cmd.complete == nil {
			return "", 0, false
		}
		candidates = cmd.complete(c)
	default:
		return "", 0, false
	}

	var matches []string
	for _, candidate := range candidates {
		if strings.HasPrefix(strings.ToLower(candidate), strings.ToLower(word)) {
			matches = append(matches, candidate)
		}
	}
	if len(matches) == 0 {
		return "", 0, false
	}

	completed := commonPrefix(matches)
	if len(matches) == 1 {
		completed += " "
	} else if len(completed) <= len(word) {
		if c.term != nil {
			c.term.Write([]byte(strings.Join(matches, "  ") + "\n"))
		}
		return "", 0, false
	}

	return head[:start] + completed + tail, start + len(completed), true
}

// commandNames returns every command's primary name, sorted.
func (c *CLI) commandNames() []string {
	names := make([]string, 0, len(c.commands))
	for _, cmd := range c.commands {
		names = append(names, cmd.names[0])
	}
	sort.Strings(names)
	return names
}

// projectNames returns every watched project's name, for completion.
func (c *CLI) projectNames() []string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	projects, err := c.svc.Projects(ctx)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(projects))
	for _, p := range projects {
		names = append(names, p.Name)
	}
	sort.Strings(names)
	return names
}

// projectNamesOrAll is projectNames plus "all", for commands that take either.
func (c *CLI) projectNamesOrAll() []string {
	return append(c.projectNames(), "all")
}

// projectTargets is every project's name and every "<name>:<service>", for
// the container commands.
func (c *CLI) projectTargets() []string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	s, err := c.svc.Status(ctx)
	if err != nil {
		return c.projectNames()
	}
	var targets []string
	for _, p := range s.Projects {
		targets = append(targets, p.Name)
		for _, svc := range p.Services {
			targets = append(targets, p.Name+":"+svc.Name)
		}
	}
	sort.Strings(targets)
	return targets
}

// projectTargetsOrAll is projectTargets plus "all".
func (c *CLI) projectTargetsOrAll() []string {
	return append(c.projectTargets(), "all")
}

func commonPrefix(words []string) string {
	prefix := words[0]
	for _, w := range words[1:] {
		for !strings.HasPrefix(strings.ToLower(w), strings.ToLower(prefix)) {
			prefix = prefix[:len(prefix)-1]
		}
	}
	return prefix
}
