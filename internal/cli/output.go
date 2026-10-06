package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"
)

// Output follows the clig.dev conventions:
//
//   - Data (tables, logs, help) goes to stdout, uncolored, so it can be piped
//     or captured: lighthouse list | grep plop.
//   - Messages go to stderr, each marked with a symbol so the meaning doesn't
//     depend on color: ✓ success, ! warning or wrong usage, ✗ error, ? question.
//   - Color is only used when stderr is a terminal and NO_COLOR isn't set.
var (
	stdout   io.Writer = os.Stdout
	stderr   io.Writer = os.Stderr
	useColor           = colorEnabled()
)

const (
	green  = "\033[32m"
	yellow = "\033[33m"
	red    = "\033[31m"
	reset  = "\033[0m"
)

func colorEnabled() bool {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	return term.IsTerminal(int(os.Stderr.Fd()))
}

func colorize(color string, s string) string {
	if !useColor {
		return s
	}
	return color + s + reset
}

// out writes data to stdout, followed by a newline.
func out(s string) {
	fmt.Fprintln(stdout, s)
}

// success reports that something worked, e.g. "✓ Deployed plop.".
func success(msg string) {
	fmt.Fprintln(stderr, colorize(green, "✓ "+msg))
}

// warn reports something the user should notice or fix, e.g. wrong arguments.
func warn(msg string) {
	fmt.Fprintln(stderr, colorize(yellow, "! "+msg))
}

// fail reports an error.
func fail(msg string) {
	fmt.Fprintln(stderr, colorize(red, "✗ "+msg))
}

// info reports something neutral, e.g. "No projects are watched yet.".
func info(msg string) {
	fmt.Fprintln(stderr, msg)
}

// ask shows a question and leaves the cursor after it for the answer.
func ask(question string) {
	fmt.Fprint(stderr, colorize(yellow, "? "+question)+" ")
}

// promptFor returns the prompt for an environment such as "dev" or "prod":
// "lighthouse (dev)> ". Production is shown in red, so it's hard to mistake.
func promptFor(env string) string {
	env = strings.ToLower(strings.TrimSpace(env))
	if env == "" {
		return "lighthouse> "
	}

	label := "(" + env + ")"
	if env == "prod" || env == "production" {
		label = colorize(red, label)
	}
	return "lighthouse " + label + "> "
}

// table writes rows as aligned columns to stdout. The first row is the header.
func table(rows [][]string) {
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	for _, row := range rows {
		fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	tw.Flush()
}

// ago describes a moment relative to now: "just now", "5m ago", "3h ago",
// "2d ago", or "-" when there is none.
func ago(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	d := time.Since(*t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// shortSHA is a commit's first 7 characters, or "-".
func shortSHA(sha string) string {
	switch {
	case sha == "":
		return "-"
	case len(sha) > 7:
		return sha[:7]
	default:
		return sha
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
