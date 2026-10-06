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

// askLine asks question and returns the answer, trimmed. answered is false
// when there's no terminal to ask on (e.g. a one-shot command run by a
// script): the caller then refuses rather than guessing.
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

// confirmOrRefuse asks question unless skip (--yes) is set. It returns
// whether to go ahead; when not, it has already explained why. verb names
// the action in the messages: "remove", "stop all".
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

// takeValue removes a flag and its value ("--name x" or "--name=x") from args.
// The value is "" when the flag isn't there; a flag without a value is an
// error.
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
