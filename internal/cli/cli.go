// Package cli implements the rillock command line.
//
// Run is the whole program: cmd/rillock/main.go only passes it the real
// arguments, environment, and output streams. Tests call Run with fakes and
// check what it prints and returns.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jamshids/rillock/internal/config"
)

// Exit codes.
const (
	exitOK    = 0
	exitError = 1 // the command failed
	exitUsage = 2 // the command line was wrong
)

// app is what every command gets: where to write, how to read the environment,
// and the clock (replaceable in tests so ages like "3m" are predictable).
type app struct {
	env    config.Env
	stdout io.Writer
	stderr io.Writer
	now    func() time.Time
}

type command struct {
	name    string
	usage   string // arguments after the command name
	summary string
	run     func(ctx context.Context, a *app, fs *flag.FlagSet, args []string) error
}

// commands in the order `rillock help` lists them.
var commands = []command{
	{"server", "[flags]", "Run the API server", runServer},
	{"migrate", "[flags]", "Create or update the database schema", runMigrate},
	{"keys", "generate --name <name> --roles <roles> [flags]", "Create an API key", runKeys},
	{"apply", "-f <file> [-f <file>...]", "Create or update agents and tools from YAML files", runApply},
	{"run", "<agent> [--input <text> | --input-json <json>]", "Submit a run of an agent", runRun},
	{"get", "runs [flags] | run <id>", "Show runs (a unique start of an ID is enough)", runGet},
	{"trace", "<run-id>", "Show everything a run did, step by step", runTrace},
	{"cancel", "<run-id>", "Cancel a run (a unique start of its ID is enough)", runCancel},
	{"version", "", "Print the version", runVersion},
}

// errUsage marks a wrong command line; Run prints the command's usage for it.
var errUsage = errors.New("usage error")

func usageErrorf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errUsage, fmt.Sprintf(format, args...))
}

// Run executes the command in args (without the program name) and returns the
// process exit code.
func Run(ctx context.Context, args []string, env config.Env, stdout, stderr io.Writer) int {
	a := &app{env: env, stdout: stdout, stderr: stderr, now: time.Now}
	if len(args) == 0 {
		printHelp(stderr)
		return exitUsage
	}

	name, rest := args[0], args[1:]
	switch name {
	case "help", "-h", "--help":
		printHelp(stdout)
		return exitOK
	case "--version":
		name = "version"
	}

	for _, cmd := range commands {
		if cmd.name != name {
			continue
		}
		fs := flag.NewFlagSet("rillock "+cmd.name, flag.ContinueOnError)
		fs.SetOutput(stderr)
		fs.Usage = func() {
			fmt.Fprintf(fs.Output(), "Usage: rillock %s %s\n\n%s.\n", cmd.name, cmd.usage, cmd.summary)
			if hasFlags(fs) {
				fmt.Fprintln(fs.Output(), "\nFlags:")
				fs.PrintDefaults()
			}
		}
		return a.exitCode(cmd.run(ctx, a, fs, rest), fs)
	}

	fmt.Fprintf(stderr, "rillock: unknown command %q\n\n", name)
	printHelp(stderr)
	return exitUsage
}

// exitCode prints err (if any) and chooses the exit code.
func (a *app) exitCode(err error, fs *flag.FlagSet) int {
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, flag.ErrHelp):
		return exitOK // the user asked for help; usage is already printed
	case errors.Is(err, errUsage):
		fmt.Fprintf(a.stderr, "rillock %s: %s\n\n", strings.TrimPrefix(fs.Name(), "rillock "),
			strings.TrimPrefix(err.Error(), errUsage.Error()+": "))
		fs.Usage()
		return exitUsage
	case errors.Is(err, config.ErrInvalidFlags):
		return exitUsage // the flag package already printed the problem and usage
	default:
		fmt.Fprintf(a.stderr, "rillock: %v\n", err)
		return exitError
	}
}

func hasFlags(fs *flag.FlagSet) bool {
	found := false
	fs.VisitAll(func(*flag.Flag) { found = true })
	return found
}

func printHelp(w io.Writer) {
	fmt.Fprint(w, "Rillock: a control plane for AI agent actions.\n\nUsage:\n  rillock <command> [arguments]\n\nCommands:\n")
	for _, cmd := range commands {
		fmt.Fprintf(w, "  %-9s %s\n", cmd.name, cmd.summary)
	}
	fmt.Fprint(w, "\nRun \"rillock <command> -h\" for a command's arguments and flags.\n")
}

// noArgs rejects positional arguments for commands that take none.
func noArgs(args []string) error {
	if len(args) > 0 {
		return usageErrorf("unexpected argument %q", args[0])
	}
	return nil
}
