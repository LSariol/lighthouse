// Package cli is Lighthouse's command line: the shell and one-shot commands, through control.Service.
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/lsariol/lighthouse/internal/control"
	"golang.org/x/term"
)

// Options are the CLI's settings.
type Options struct {
	Env string

	Embedded bool
}

type CLI struct {
	svc    control.Service
	prompt string

	scanner     *bufio.Scanner
	term        *term.Terminal
	interactive bool
	embedded    bool

	commands []command
	byName   map[string]*command

	leave func()
}

func New(svc control.Service, opts Options) *CLI {
	c := &CLI{
		svc:         svc,
		prompt:      promptFor(opts.Env),
		scanner:     bufio.NewScanner(os.Stdin),
		interactive: term.IsTerminal(int(os.Stdin.Fd())),
		embedded:    opts.Embedded,
		commands:    commandTable(opts.Embedded),
		byName:      make(map[string]*command),
	}

	for i := range c.commands {
		for _, name := range c.commands[i].names {
			c.byName[name] = &c.commands[i]
		}
	}
	return c
}

// Run reads and runs commands until stdin closes, `exit` is typed, or ctx is
// cancelled.
func (c *CLI) Run(ctx context.Context, stop func()) {
	c.leave = stop

	if !c.embedded {
		c.checkDaemon(ctx)
		if c.interactive && c.runTerminal(ctx) {
			return
		}
	}

	for ctx.Err() == nil {
		fmt.Fprint(stderr, c.prompt)
		if !c.scanner.Scan() {
			return
		}

		args := strings.Fields(c.scanner.Text())
		if len(args) == 0 {
			continue
		}
		report(c.Exec(ctx, args))
	}
}

// checkDaemon warns when the daemon can't be reached, so the first command
// doesn't come as a surprise.
func (c *CLI) checkDaemon(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := c.svc.Status(ctx); errors.Is(err, control.ErrUnreachable) {
		warn(err.Error())
	}
}

// Exec runs a single command, e.g. ["deploy", "plop"].
func (c *CLI) Exec(ctx context.Context, args []string) error {
	cmd, ok := c.byName[strings.ToLower(args[0])]
	if !ok {
		return fmt.Errorf("Unknown command %q. Type \"help\" to see the available commands.", args[0])
	}
	return cmd.run(c, ctx, args)
}

// Report shows an error returned by Exec: wrong arguments as a warning,
// anything else as an error.
func Report(err error) {
	report(err)
}

func report(err error) {
	if err == nil {
		return
	}

	var usage usageError
	if errors.As(err, &usage) {
		warn(err.Error())
		return
	}
	fail(err.Error())
}

// askLine asks question and returns the answer, trimmed.
func (c *CLI) askLine(question string) (answer string, answered bool) {
	switch {
	case c.term != nil:
		c.term.SetPrompt(colorize(yellow, "? "+question) + " ")
		line, err := c.term.ReadLine()
		c.term.SetPrompt(c.prompt)
		if err != nil {
			return "", false
		}
		answer = line
	case c.interactive:
		ask(question)
		if !c.scanner.Scan() {
			fmt.Fprintln(stderr)
			return "", false
		}
		answer = c.scanner.Text()
	default:
		return "", false
	}
	return strings.TrimSpace(answer), true
}

// confirm asks a yes/no question; Enter means no.
func (c *CLI) confirm(question string) (yes bool, answered bool) {
	answer, answered := c.askLine(question + " (y/N)")
	answer = strings.ToLower(answer)
	return answer == "y" || answer == "yes", answered
}

// confirmOrRefuse asks question unless skip (--yes) is set.
func (c *CLI) confirmOrRefuse(question string, skip bool, verb string) (bool, error) {
	if skip {
		return true, nil
	}
	yes, answered := c.confirm(question)
	if !answered {
		return false, fmt.Errorf("%s cancelled: no answer to the confirmation. Use --yes to %s without asking.", capitalize(verb), verb)
	}
	if !yes {
		info(capitalize(verb) + " cancelled.")
	}
	return yes, nil
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// takeYesFlag removes --yes / -y from args and reports whether it was there.
func takeYesFlag(args []string) (bool, []string) {
	return takeFlag(args, "--yes", "-y")
}

// takeValue removes a flag and its value ("--name x" or "--name=x") from
// args.
func takeValue(args []string, flag string) (string, []string, error) {
	value := ""
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == flag:
			if i+1 >= len(args) || args[i+1] == "" {
				return "", nil, fmt.Errorf("%s needs a value", flag)
			}
			value = args[i+1]
			i++
		case strings.HasPrefix(a, flag+"="):
			value = strings.TrimPrefix(a, flag+"=")
			if value == "" {
				return "", nil, fmt.Errorf("%s needs a value", flag)
			}
		default:
			rest = append(rest, a)
		}
	}
	return value, rest, nil
}

// takeFlag removes every spelling of a flag from args and reports whether it
// was there.
func takeFlag(args []string, spellings ...string) (bool, []string) {
	found := false
	rest := make([]string, 0, len(args))
	for _, a := range args {
		if slices.Contains(spellings, a) {
			found = true
			continue
		}
		rest = append(rest, a)
	}
	return found, rest
}
