// Package cli is Lighthouse's command line: the interactive prompt
// (`lighthouse shell`, or inside the daemon with plain `lighthouse`) and
// one-shot commands (`lighthouse list`). All of them talk to the daemon
// through control.Service.
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
	// Env is the environment shown in the prompt, e.g. "dev" or "prod"
	// (APP_ENV). Production is shown in red.
	Env string

	// Embedded is true when the CLI runs inside the daemon (plain
	// `lighthouse`). Then `exit` stops Lighthouse too, and input stays plain
	// lines, so Ctrl+C reaches the daemon as a signal and stops it.
	Embedded bool
}

type CLI struct {
	svc    control.Service
	prompt string

	// scanner reads stdin for both the prompt and follow-up questions, so no
	// input is lost between two readers. When the shell has line editing,
	// term is used instead. interactive is whether stdin is a terminal:
	// without one, questions aren't asked and destructive commands refuse.
	scanner     *bufio.Scanner
	term        *term.Terminal
	interactive bool
	embedded    bool

	commands []command
	byName   map[string]*command

	// leave ends the shell (and, embedded, Lighthouse); `exit` calls it. It's
	// set by Run.
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
// cancelled. A standalone shell on a terminal has line editing, history and
// Tab completion. Embedded in the daemon, input stays plain lines so Ctrl+C
// still stops Lighthouse; when stdin closes (no terminal, as in Docker), Run
// returns and the daemon keeps running.
func (c *CLI) Run(ctx context.Context, stop func()) {
	c.leave = stop

	if c.embedded {
		if c.interactive {
			info("Lighthouse is running, with its CLI here. Type \"help\" for commands; \"exit\" or Ctrl+C stops Lighthouse.")
		}
	} else {
		info("Lighthouse shell. Type \"help\" for commands, \"exit\" to leave.")
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
// doesn't come as a surprise. Help still works without it.
func (c *CLI) checkDaemon(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := c.svc.Status(ctx); errors.Is(err, control.ErrUnreachable) {
		warn(err.Error())
	}
}

// Exec runs a single command, e.g. ["deploy", "plop"]. It returns the
// command's error, which the caller shows (see Report); one-shot commands
// also turn it into a non-zero exit status.
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

// confirm asks a yes/no question; Enter means no. answered is false when
// there's no terminal to ask on (e.g. a one-shot command run by a script):
// the caller then refuses and points at --yes rather than guessing.
func (c *CLI) confirm(question string) (yes bool, answered bool) {
	question += " (y/N)"
	var answer string

	switch {
	case c.term != nil:
		c.term.SetPrompt(colorize(yellow, "? "+question) + " ")
		line, err := c.term.ReadLine()
		c.term.SetPrompt(c.prompt)
		if err != nil {
			return false, false
		}
		answer = line
	case c.interactive:
		ask(question)
		if !c.scanner.Scan() {
			fmt.Fprintln(stderr)
			return false, false
		}
		answer = c.scanner.Text()
	default:
		return false, false
	}

	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", true
}

// confirmOrRefuse asks question unless skip (--yes) is set. It returns
// whether to go ahead; when not, it has already explained why.
func (c *CLI) confirmOrRefuse(question string, skip bool, what string) (bool, error) {
	if skip {
		return true, nil
	}
	yes, answered := c.confirm(question)
	if !answered {
		return false, fmt.Errorf("%s needs confirmation, and there's no terminal to ask on. Add --yes to go ahead.", what)
	}
	if !yes {
		info("Cancelled.")
	}
	return yes, nil
}

// takeYesFlag removes --yes / -y from args and reports whether it was there.
func takeYesFlag(args []string) (bool, []string) {
	return takeFlag(args, "--yes", "-y")
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
